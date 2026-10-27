package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
)

// testManifest is a valid module manifest, so the verifier exercises the
// document as well as the envelope.
func testManifest(t *testing.T) *solutionhost.ModuleManifest {
	t.Helper()
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "acme.local.wiki", Alias: "wiki", Kind: solutionhost.KindModule,
		Generation: 1, Host: solutionhost.HostTarget{Coordinate: "acme/local", Domain: "acme"},
		Release:   solutionhost.Release{Publisher: "acme", Name: "wiki", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "backend", Container: "backend"}},
		Authority: &solutionhost.Authority{Principal: "wiki"},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testSigner(t *testing.T) solutionhost.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := solutionhost.NewInMemorySigner("release", private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestKeyCheckVerifiesEnvelopeKeyAndOwnership(t *testing.T) {
	document := testManifest(t)
	payload, err := solutionhost.SignedPayload(document)
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	for _, kind := range []solutionhost.KeyKind{solutionhost.KeyKindRelease, solutionhost.KeyKindPerson} {
		check, err := KeyCheck(signer, kind, document.Host.Domain, signer.KeyID())
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := SignEnvelope(payload, signer)
		if err != nil {
			t.Fatal(err)
		}
		if err = check(context.Background(), envelope, payload); err != nil {
			t.Fatal(err)
		}
		identity, err := ReadIdentity(envelope)
		if err != nil || identity.KeyID != signer.KeyID() {
			t.Fatalf("identity %v %v", identity, err)
		}
		wrongDomain, err := KeyCheck(signer, kind, "another-domain", signer.KeyID())
		if err != nil {
			t.Fatal(err)
		}
		if err = wrongDomain(context.Background(), envelope, payload); err == nil {
			t.Fatal("foreign domain admitted")
		}
		_, private, _ := ed25519.GenerateKey(rand.Reader)
		other, _ := solutionhost.NewInMemorySigner(signer.KeyID(), private)
		wrong, err := SignEnvelope(payload, other)
		if err != nil {
			t.Fatal(err)
		}
		if err = check(context.Background(), wrong, payload); err == nil {
			t.Fatal("same ID with another key admitted")
		}
		var tampered map[string]any
		if err = json.Unmarshal(envelope, &tampered); err != nil {
			t.Fatal(err)
		}
		tampered["signatures"].([]any)[0].(map[string]any)["keyid"] = "another-key"
		wrong, _ = json.Marshal(tampered)
		if err = check(context.Background(), wrong, payload); err == nil {
			t.Fatal("unlisted key admitted")
		}
		// Parsing the claimed identity does not imply verification.
		if id, err := ReadIdentity(wrong); err != nil || id.KeyID != "another-key" {
			t.Fatal("claim unreadable")
		}
	}
	if _, err = KeyCheck(signer, solutionhost.KeyKindRelease, document.Host.Domain, "unlisted"); err == nil {
		t.Fatal("custody mismatch accepted")
	}
	for _, raw := range []string{"", `{}`, `{"signatures":[{},{}]}`, `{"signatures":[{"keyid":""}]}`} {
		if _, err = ReadIdentity([]byte(raw)); err == nil {
			t.Fatal("malformed identity accepted")
		}
	}
	unavailable := FromEnvironment(func(string) (string, bool) { return "", false })
	if _, err = SignEnvelope(payload, unavailable); err == nil || !strings.Contains(err.Error(), "VAULT_TOKEN") {
		t.Fatal("missing custody was not explained")
	}
}

// testPayload is testManifest's canonical bytes: what a signer signs.
func testPayload(t *testing.T) []byte {
	t.Helper()
	payload, err := solutionhost.SignedPayload(testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestKeyCheckHoldsAnEnvelopeToARelease: a module envelope is held to the
// verifier its host runs, which admits it only from a release key: the same
// key pinned as a person's signs it and fails the check, so a publish never
// writes an envelope carrier the host refuses.
func TestKeyCheckHoldsAnEnvelopeToARelease(t *testing.T) {
	document := &solutionhost.ModuleEnvelope{
		Schema: solutionhost.SchemaModuleEnvelopeV1, Revision: 1,
		Host:       solutionhost.HostTarget{Coordinate: "acme/local", Domain: "acme"},
		Principals: map[string]json.RawMessage{"records": json.RawMessage(`{"operation_audiences":{}}`)},
	}
	payload, err := document.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	envelope, err := SignEnvelope(payload, signer)
	if err != nil {
		t.Fatal(err)
	}
	release, err := KeyCheck(signer, solutionhost.KeyKindRelease, "acme", signer.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	if err = release(context.Background(), envelope, payload); err != nil {
		t.Fatalf("a release key's envelope refused: %v", err)
	}
	person, err := KeyCheck(signer, solutionhost.KeyKindPerson, "acme", signer.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	if err = person(context.Background(), envelope, payload); !errors.Is(err, solutionhost.ErrEnvelopeSignerNotRelease) || !errors.Is(err, ErrSignature) {
		t.Fatalf("a person key's envelope = %v, want ErrEnvelopeSignerNotRelease", err)
	}
	foreign, err := KeyCheck(signer, solutionhost.KeyKindRelease, "another-domain", signer.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	if err = foreign(context.Background(), envelope, payload); err == nil {
		t.Fatal("an envelope speaking for a domain its key is not pinned for admitted")
	}
}
