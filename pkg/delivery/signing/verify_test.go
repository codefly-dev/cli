package signing

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	releaseIssuer  = GitHubActionsIssuer
	releaseSubject = "https://github.com/codefly-dev/cli/.github/workflows/release.yaml@refs/tags/v1.2.3"
)

// virtualRelease is a document signed by an in-memory Sigstore: a Fulcio-style CA, a Rekor log
// and a timestamp authority whose keys exist only for this test, and the trusted root pinning them.
type virtualRelease struct {
	sigstore *ca.VirtualSigstore
	payload  []byte
	bundle   []byte
	trusted  *root.TrustedRoot
}

// signVirtually signs payload for subject at issuer. sigstore-go's test CA yields a
// verify.SignedEntity rather than a bundle, so the bundle is laid out here the way sign.Bundle
// lays it out: the leaf certificate, the message signature over the payload digest, and the
// hashedrekord log entry with its inclusion promise and proof (plus the RFC 3161 timestamp when
// asked, as a signer configured with a TimestampURL would add).
func signVirtually(t *testing.T, subject, issuer string, payload []byte, withTimestamp bool) virtualRelease {
	t.Helper()
	sigstore, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	entity, err := sigstore.Sign(subject, issuer, payload)
	if err != nil {
		t.Fatalf("virtual Sign: %v", err)
	}
	verification, err := entity.VerificationContent()
	if err != nil {
		t.Fatalf("VerificationContent: %v", err)
	}
	signature, err := entity.SignatureContent()
	if err != nil {
		t.Fatalf("SignatureContent: %v", err)
	}
	message := signature.MessageSignatureContent()
	entries, err := entity.TlogEntries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("TlogEntries: %v (%d entries)", err, len(entries))
	}

	bundle := &protobundle.Bundle{
		MediaType: MediaTypeBundle,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: verification.Certificate().Raw},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{completeLogEntry(t, sigstore, entries[0])},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{
				Algorithm: protocommon.HashAlgorithm(protocommon.HashAlgorithm_value[message.DigestAlgorithm()]),
				Digest:    message.Digest(),
			},
			Signature: message.Signature(),
		}},
	}
	if withTimestamp {
		timestamps, err := entity.Timestamps()
		if err != nil {
			t.Fatalf("Timestamps: %v", err)
		}
		data := &protobundle.TimestampVerificationData{}
		for _, timestamp := range timestamps {
			data.Rfc3161Timestamps = append(data.Rfc3161Timestamps, &protocommon.RFC3161SignedTimestamp{SignedTimestamp: timestamp})
		}
		bundle.VerificationMaterial.TimestampVerificationData = data
	}
	raw, err := protojson.Marshal(bundle)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return virtualRelease{sigstore: sigstore, payload: payload, bundle: raw, trusted: trustedRootOf(t, sigstore)}
}

// completeLogEntry finishes the virtual log entry: the test CA records the body, the log and the
// integrated time but leaves the kind, the signed entry timestamp and the inclusion proof to the
// caller. The log holds this single leaf, so the entry sits at the proof's index.
func completeLogEntry(t *testing.T, sigstore *ca.VirtualSigstore, entry *tlog.Entry) *protorekor.TransparencyLogEntry {
	t.Helper()
	tle, ok := proto.Clone(entry.TransparencyLogEntry()).(*protorekor.TransparencyLogEntry)
	if !ok {
		t.Fatalf("TransparencyLogEntry cloned as %T", proto.Clone(entry.TransparencyLogEntry()))
	}
	proof, err := sigstore.GetInclusionProof(tle.CanonicalizedBody)
	if err != nil {
		t.Fatalf("GetInclusionProof: %v", err)
	}
	tle.LogIndex = *proof.LogIndex
	tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}

	set, err := sigstore.RekorSignPayload(tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(tle.CanonicalizedBody),
		IntegratedTime: tle.IntegratedTime,
		LogIndex:       tle.LogIndex,
		LogID:          hex.EncodeToString(tle.LogId.KeyId),
	})
	if err != nil {
		t.Fatalf("RekorSignPayload: %v", err)
	}
	tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}

	rootHash, err := hex.DecodeString(*proof.RootHash)
	if err != nil {
		t.Fatalf("root hash: %v", err)
	}
	hashes := make([][]byte, 0, len(proof.Hashes))
	for _, encoded := range proof.Hashes {
		hash, err := hex.DecodeString(encoded)
		if err != nil {
			t.Fatalf("proof hash: %v", err)
		}
		hashes = append(hashes, hash)
	}
	tle.InclusionProof = &protorekor.InclusionProof{
		LogIndex:   *proof.LogIndex,
		RootHash:   rootHash,
		TreeSize:   *proof.TreeSize,
		Hashes:     hashes,
		Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
	}
	return tle
}

func trustedRootOf(t *testing.T, sigstore *ca.VirtualSigstore) *root.TrustedRoot {
	t.Helper()
	trusted, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		sigstore.FulcioCertificateAuthorities(), sigstore.CTLogs(), sigstore.TimestampingAuthorities(), sigstore.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return trusted
}

func exactPolicy(issuer, subject string) Policy {
	return Policy{Issuer: issuer, SubjectPattern: "^" + regexp.QuoteMeta(subject) + "$"}
}

func TestVerifyAdmitsTheReleaseWorkflow(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte(`{"document":"presence","generation":7}`), false)

	identity, err := Verify(release.bundle, release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity != (Identity{Issuer: releaseIssuer, Subject: releaseSubject}) {
		t.Errorf("identity = %+v, want the release workflow at the GitHub Actions issuer", identity)
	}

	workflow, err := GitHubActionsPolicy("codefly-dev/cli", ".github/workflows/release.yaml", "refs/tags/v.*")
	if err != nil {
		t.Fatalf("GitHubActionsPolicy: %v", err)
	}
	if _, err := Verify(release.bundle, release.payload, release.trusted, workflow); err != nil {
		t.Errorf("the repository's release policy must admit its own tag: %v", err)
	}
}

func TestVerifyAcceptsASignedTimestampAsTheObserver(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("authority document"), true)
	identity, err := Verify(release.bundle, release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject))
	if err != nil {
		t.Fatalf("Verify with an RFC 3161 timestamp: %v", err)
	}
	if identity.Subject != releaseSubject {
		t.Errorf("subject = %q, want %q", identity.Subject, releaseSubject)
	}
}

func TestVerifyRefusesIdentitiesThePolicyDoesNotAdmit(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	branch, err := GitHubActionsPolicy("codefly-dev/cli", ".github/workflows/release.yaml", "refs/heads/main")
	if err != nil {
		t.Fatalf("GitHubActionsPolicy: %v", err)
	}
	policies := map[string]Policy{
		"another subject":        exactPolicy(releaseIssuer, "https://github.com/codefly-dev/other/.github/workflows/release.yaml@refs/tags/v1.2.3"),
		"another issuer":         exactPolicy("https://accounts.google.com", releaseSubject),
		"a branch, not a tag":    branch,
		"a mere substring":       {Issuer: releaseIssuer, SubjectPattern: "codefly-dev/cli"},
		"an unanchored suffix":   {Issuer: releaseIssuer, SubjectPattern: regexp.QuoteMeta("@refs/tags/v1.2.3")},
		"an unanchored prefix":   {Issuer: releaseIssuer, SubjectPattern: regexp.QuoteMeta("https://github.com/codefly-dev/cli/")},
		"the issuer as a prefix": exactPolicy("https://token.actions.githubusercontent.com/evil", releaseSubject),
	}
	for name, policy := range policies {
		_, err := Verify(release.bundle, release.payload, release.trusted, policy)
		if !errors.Is(err, ErrSignature) {
			t.Errorf("%s: err = %v, want ErrSignature", name, err)
		}
		if errors.Is(err, ErrPolicy) {
			t.Errorf("%s: a refused identity is not a misconfigured policy: %v", name, err)
		}
	}
}

func TestVerifyRefusesAnAlteredDocument(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte(`{"document":"presence","generation":7}`), false)
	altered := bytes.Clone(release.payload)
	altered[len(altered)-2] ^= 0x01 // generation 7 becomes 6

	_, err := Verify(release.bundle, altered, release.trusted, exactPolicy(releaseIssuer, releaseSubject))
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature for a document that differs by one byte", err)
	}
	if _, err := Verify(release.bundle, release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject)); err != nil {
		t.Fatalf("the unaltered document must still verify: %v", err)
	}
}

func TestVerifyRefusesAnotherTrustRoot(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	other := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)

	_, err := Verify(release.bundle, release.payload, other.trusted, exactPolicy(releaseIssuer, releaseSubject))
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature when the bundle's Sigstore is not the trusted one", err)
	}
}

func TestVerifyTellsAMisconfiguredPolicyFromABadSignature(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	policies := map[string]Policy{
		"empty policy":    {},
		"no issuer":       {SubjectPattern: "^" + regexp.QuoteMeta(releaseSubject) + "$"},
		"no subject":      {Issuer: releaseIssuer},
		"invalid pattern": {Issuer: releaseIssuer, SubjectPattern: "("},
	}
	for name, policy := range policies {
		_, err := Verify(release.bundle, release.payload, release.trusted, policy)
		if !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: err = %v, want ErrPolicy", name, err)
		}
		if errors.Is(err, ErrSignature) {
			t.Errorf("%s: a configuration error must not read as a bad signature: %v", name, err)
		}
	}
	if _, err := Verify(release.bundle, release.payload, nil, exactPolicy(releaseIssuer, releaseSubject)); err == nil {
		t.Error("verifying without a trusted root must fail")
	}
}

func TestReadIdentityReportsTheSignerWithoutATrustRoot(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	identity, err := ReadIdentity(release.bundle)
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if identity != (Identity{Issuer: releaseIssuer, Subject: releaseSubject}) {
		t.Errorf("identity = %+v, want the signer's subject and issuer", identity)
	}
}

func TestMalformedBundlesAreRefusedReadably(t *testing.T) {
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	truncated := release.bundle[:len(release.bundle)/2]
	corrupted := bytes.Clone(release.bundle)
	corrupted[len(corrupted)/2] ^= 0xff
	bundles := map[string][]byte{
		"nil":                 nil,
		"empty":               {},
		"whitespace":          []byte("  \n"),
		"not JSON":            []byte("not a bundle"),
		"truncated JSON":      truncated,
		"corrupted byte":      corrupted,
		"media type only":     []byte(`{"mediaType":"` + MediaTypeBundle + `"}`),
		"unknown media type":  []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v9+json"}`),
		"wrong document kind": []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`),
	}
	for name, bundle := range bundles {
		_, err := Verify(bundle, release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject))
		if !errors.Is(err, ErrSignature) || !errors.Is(err, ErrBundle) {
			t.Errorf("Verify(%s): err = %v, want ErrSignature wrapping ErrBundle", name, err)
		}
		_, err = ReadIdentity(bundle)
		if !errors.Is(err, ErrBundle) {
			t.Errorf("ReadIdentity(%s): err = %v, want ErrBundle", name, err)
		}
		if err == nil || strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrBundle.Error())) == "" {
			t.Errorf("ReadIdentity(%s): the error must say what is wrong, got %v", name, err)
		}
	}
}
