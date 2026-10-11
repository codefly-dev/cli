package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	ctx509util "github.com/google/certificate-transparency-go/x509util"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
)

// sctSigstore is an in-process Sigstore whose certificates are what the
// public-good Fulcio issues: a Fulcio chain, the issuer extension, and an
// embedded Signed Certificate Timestamp from a CT log the trusted root names —
// plus a Rekor entry with its signed entry timestamp and, on request, an RFC
// 3161 timestamp. sigstore-go's VirtualSigstore embeds no SCT, and the host
// (module-saas-starter#1066) now requires one from every signer, so a fixture
// that cannot produce one could only be verified at a weaker strength than the
// host's. Its Rekor log and timestamp authority are VirtualSigstore's; its
// Fulcio authority and CT log are its own, because their private keys are what
// a certificate and an SCT are made with. (The same construction as the
// host's own fixture, so both sides are tested at one strength.)
type sctSigstore struct {
	*ca.VirtualSigstore

	fulcio          *root.FulcioCertificateAuthority
	intermediateKey *ecdsa.PrivateKey
	ctLogKey        *ecdsa.PrivateKey
}

func newSCTSigstore(t *testing.T) *sctSigstore {
	t.Helper()
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	rootCertificate, rootKey, err := ca.GenerateRootCa()
	if err != nil {
		t.Fatal(err)
	}
	intermediate, intermediateKey, err := ca.GenerateFulcioIntermediate(rootCertificate, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	ctLogKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &sctSigstore{
		VirtualSigstore: virtual,
		fulcio: &root.FulcioCertificateAuthority{
			Root:                rootCertificate,
			Intermediates:       []*x509.Certificate{intermediate},
			ValidityPeriodStart: time.Now().Add(-5 * time.Hour),
			ValidityPeriodEnd:   time.Now().Add(time.Hour),
			URI:                 "https://fulcio.example.test",
		},
		intermediateKey: intermediateKey,
		ctLogKey:        ctLogKey,
	}
}

// trustedRoot pins this fixture's Fulcio chain and CT log, and the virtual
// Rekor log and timestamp authority.
func (s *sctSigstore) trustedRoot(t *testing.T) *root.TrustedRoot {
	t.Helper()
	logID := publicKeyID(s.ctLogKey.Public())
	ctLogs := map[string]*root.TransparencyLog{
		hex.EncodeToString(logID[:]): {
			BaseURL: "https://ctlog.example.test", ID: logID[:],
			ValidityPeriodStart: time.Now().Add(-time.Hour), ValidityPeriodEnd: time.Now().Add(time.Hour),
			HashFunc: crypto.SHA256, PublicKey: s.ctLogKey.Public(),
		},
	}
	trusted, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{s.fulcio}, ctLogs, s.TimestampingAuthorities(), s.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return trusted
}

// sctSigning is how one certificate is made: with an RFC 3161 timestamp, with
// no SCT at all, or with an SCT from a log the root does not name.
type sctSigning struct {
	timestamp   bool
	noSCT       bool
	unknownLogs bool
}

// sctSigned is what a bundle carries.
type sctSigned struct {
	leaf      *x509.Certificate
	digest    []byte
	signature []byte
	timestamp []byte
	entry     *protorekor.TransparencyLogEntry
}

func (s *sctSigstore) sign(t *testing.T, identity, issuer string, payload []byte, how sctSigning) sctSigned {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := s.issue(t, identity, issuer, leafKey, how)
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, leafKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signed := sctSigned{leaf: leaf, digest: digest[:], signature: signature, entry: s.logEntry(t, leaf, digest[:], signature)}
	if how.timestamp {
		if signed.timestamp, err = s.TimestampResponse(signature); err != nil {
			t.Fatal(err)
		}
	}
	return signed
}

// issue mints the leaf the way a CT-logging CA does: the TBS certificate is
// signed into an SCT first, then the SCT is embedded and the certificate
// issued; a verifier strips the SCT extension and recovers exactly the TBS the
// log signed.
func (s *sctSigstore) issue(t *testing.T, identity, issuer string, leafKey *ecdsa.PrivateKey, how sctSigning) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	issuerV2, err := asn1.MarshalWithParams(issuer, "utf8")
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:    serial,
		NotBefore:       time.Now().Add(-time.Minute),
		NotAfter:        time.Now().Add(10 * time.Minute),
		KeyUsage:        x509.KeyUsageDigitalSignature,
		ExtKeyUsage:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{{Id: certificate.OIDIssuerV2, Value: issuerV2}},
	}
	if strings.Contains(identity, "://") {
		uri, parseErr := url.Parse(identity)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		template.URIs = []*url.URL{uri}
	} else {
		template.EmailAddresses = []string{identity}
	}
	if how.noSCT {
		return s.create(t, template, leafKey)
	}
	precertificate := s.create(t, template, leafKey)
	sctKey := s.ctLogKey
	if how.unknownLogs {
		if sctKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	unsigned := ct.SignedCertificateTimestamp{
		SCTVersion: ct.V1,
		LogID:      ct.LogID{KeyID: publicKeyID(sctKey.Public())},
		Timestamp:  uint64(time.Now().UnixMilli()), //nolint:gosec // G115: a current Unix time in milliseconds is positive
	}
	input, err := ct.SerializeSCTSignatureInput(unsigned, ct.LogEntry{Leaf: ct.MerkleTreeLeaf{
		Version:  ct.V1,
		LeafType: ct.TimestampedEntryLeafType,
		TimestampedEntry: &ct.TimestampedEntry{
			Timestamp: unsigned.Timestamp,
			EntryType: ct.PrecertLogEntryType,
			PrecertEntry: &ct.PreCert{
				IssuerKeyHash:  sha256.Sum256(s.fulcio.Intermediates[0].RawSubjectPublicKeyInfo),
				TBSCertificate: precertificate.RawTBSCertificate,
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	inputDigest := sha256.Sum256(input)
	sctSignature, err := ecdsa.SignASN1(rand.Reader, sctKey, inputDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	unsigned.Signature = ct.DigitallySigned{
		Algorithm: cttls.SignatureAndHashAlgorithm{Hash: cttls.SHA256, Signature: cttls.ECDSA},
		Signature: sctSignature,
	}
	list, err := ctx509util.MarshalSCTsIntoSCTList([]*ct.SignedCertificateTimestamp{&unsigned})
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := cttls.Marshal(*list)
	if err != nil {
		t.Fatal(err)
	}
	octets, err := asn1.Marshal(serialized)
	if err != nil {
		t.Fatal(err)
	}
	template.ExtraExtensions = append(template.ExtraExtensions,
		pkix.Extension{Id: asn1.ObjectIdentifier(ctx509.OIDExtensionCTSCT), Value: octets})
	return s.create(t, template, leafKey)
}

func (s *sctSigstore) create(t *testing.T, template *x509.Certificate, leafKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, s.fulcio.Intermediates[0], leafKey.Public(), s.intermediateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// logEntry records the signature in the virtual Rekor as a hashedrekord, with
// a signed entry timestamp the root's Rekor key verifies.
func (s *sctSigstore) logEntry(t *testing.T, leaf *x509.Certificate, digest, signature []byte) *protorekor.TransparencyLogEntry {
	t.Helper()
	certificatePEM := "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(leaf.Raw) + "\n-----END CERTIFICATE-----\n"
	body, err := json.Marshal(map[string]any{
		"apiVersion": "0.0.1",
		"kind":       "hashedrekord",
		"spec": map[string]any{
			"data": map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(digest)}},
			"signature": map[string]any{
				"content":   base64.StdEncoding.EncodeToString(signature),
				"publicKey": map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(certificatePEM))},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logID, err := s.RekorLogID()
	if err != nil {
		t.Fatal(err)
	}
	logIDBytes, err := hex.DecodeString(logID)
	if err != nil {
		t.Fatal(err)
	}
	// A v0.3 bundle carries the inclusion proof with its checkpoint, as the
	// public-good Rekor returns it; the entry sits at the proof's index.
	proof, err := s.GetInclusionProof(body)
	if err != nil {
		t.Fatal(err)
	}
	integrated := time.Now().Unix()
	promise, err := s.RekorSignPayload(tlog.RekorPayload{
		Body: base64.StdEncoding.EncodeToString(body), IntegratedTime: integrated, LogIndex: *proof.LogIndex, LogID: logID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootHash, err := hex.DecodeString(*proof.RootHash)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][]byte, 0, len(proof.Hashes))
	for _, encoded := range proof.Hashes {
		hash, decodeErr := hex.DecodeString(encoded)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		hashes = append(hashes, hash)
	}
	return &protorekor.TransparencyLogEntry{
		LogIndex:         *proof.LogIndex,
		LogId:            &protocommon.LogId{KeyId: logIDBytes},
		KindVersion:      &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"},
		IntegratedTime:   integrated,
		InclusionPromise: &protorekor.InclusionPromise{SignedEntryTimestamp: promise},
		InclusionProof: &protorekor.InclusionProof{
			LogIndex: *proof.LogIndex, RootHash: rootHash, TreeSize: *proof.TreeSize, Hashes: hashes,
			Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
		},
		CanonicalizedBody: body,
	}
}

func publicKeyID(public crypto.PublicKey) [32]byte {
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(der)
}
