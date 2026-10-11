package signing

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	releaseIssuer  = GitHubActionsIssuer
	releaseSubject = "https://github.com/codefly-dev/cli/.github/workflows/release.yaml@refs/tags/v1.2.3"
)

// virtualRelease is a document signed by an in-memory Sigstore whose
// certificates are what the public-good Fulcio issues (sctSigstore): a Fulcio
// chain, the issuer extension, an embedded SCT, a Rekor entry with its signed
// entry timestamp — and the trusted root pinning keys that exist only for this
// test.
type virtualRelease struct {
	payload []byte
	bundle  []byte
	trusted *root.TrustedRoot
}

// signVirtually signs payload for subject at issuer, with an RFC 3161
// timestamp beside the log entry when asked (as a signer configured with a
// TimestampURL would add).
func signVirtually(t *testing.T, subject, issuer string, payload []byte, withTimestamp bool) virtualRelease {
	t.Helper()
	return signVirtuallyWith(t, subject, issuer, payload, sctSigning{timestamp: withTimestamp})
}

// signVirtuallyWith is signVirtually with the certificate's SCT chosen.
func signVirtuallyWith(t *testing.T, subject, issuer string, payload []byte, how sctSigning) virtualRelease {
	t.Helper()
	sigstore := newSCTSigstore(t)
	signed := sigstore.sign(t, subject, issuer, payload, how)
	bundle := &protobundle.Bundle{
		MediaType: MediaTypeBundle,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: signed.leaf.Raw},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{signed.entry},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: signed.digest},
			Signature:     signed.signature,
		}},
	}
	if how.timestamp {
		bundle.VerificationMaterial.TimestampVerificationData = &protobundle.TimestampVerificationData{
			Rfc3161Timestamps: []*protocommon.RFC3161SignedTimestamp{{SignedTimestamp: signed.timestamp}},
		}
	}
	raw, err := protojson.Marshal(bundle)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return virtualRelease{payload: payload, bundle: raw, trusted: sigstore.trustedRoot(t)}
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

// TestVerifyNamesAMissingTransparencyLog: a bundle with no transparency-log entry is what a
// signer configured without a log produces — a misconfiguration to fix at the signer — and
// never a bad signature to investigate, so it is refused by its own name and not as
// ErrSignature. A signed timestamp does not stand in: it says when, not that it was logged.
func TestVerifyNamesAMissingTransparencyLog(t *testing.T) {
	for name, withTimestamp := range map[string]bool{"no timestamp": false, "a signed timestamp": true} {
		release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), withTimestamp)
		_, err := Verify(withoutTransparencyLog(t, release.bundle), release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject))
		if !errors.Is(err, ErrNoTransparency) {
			t.Errorf("%s: err = %v, want ErrNoTransparency", name, err)
		}
		if errors.Is(err, ErrSignature) {
			t.Errorf("%s: a signer misconfiguration must not read as a bad signature: %v", name, err)
		}
		if _, err := Verify(release.bundle, release.payload, release.trusted, exactPolicy(releaseIssuer, releaseSubject)); err != nil {
			t.Errorf("%s: the bundle with its log entry must still verify: %v", name, err)
		}
	}
}

// withoutTransparencyLog re-encodes a bundle with its transparency-log entries dropped, as a
// signer with no TransparencyLogs configured would have written it.
func withoutTransparencyLog(t *testing.T, raw []byte) []byte {
	t.Helper()
	var bundle protobundle.Bundle
	if err := protojson.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}
	bundle.VerificationMaterial.TlogEntries = nil
	stripped, err := protojson.Marshal(&bundle)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	return stripped
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

// TestVerifyRequiresAnSCTFromALogTheRootNames: the check runs at the host's
// strength — a certificate no CT log saw, or one whose SCT comes from a log
// the trusted root does not name, is refused as a failing signature, with the
// signature, the log entry and the identity otherwise sound. It holds for a
// developer's carrier and a release's alike: one verifier.
func TestVerifyRequiresAnSCTFromALogTheRootNames(t *testing.T) {
	for _, identity := range [][2]string{{releaseIssuer, releaseSubject}, {"https://github.com/login/oauth", "dev@example.test"}} {
		sound := signVirtually(t, identity[1], identity[0], []byte("presence document"), false)
		if _, err := Verify(sound.bundle, sound.payload, sound.trusted, exactPolicy(identity[0], identity[1])); err != nil {
			t.Fatalf("%s: the control, a certificate with its SCT, must verify: %v", identity[1], err)
		}
		for name, how := range map[string]sctSigning{"no SCT": {noSCT: true}, "an SCT from an unnamed log": {unknownLogs: true}} {
			release := signVirtuallyWith(t, identity[1], identity[0], []byte("presence document"), how)
			_, err := Verify(release.bundle, release.payload, release.trusted, exactPolicy(identity[0], identity[1]))
			if !errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), "SCT") {
				t.Errorf("%s, %s: err = %v, want a refused SCT", identity[1], name, err)
			}
		}
	}
}
