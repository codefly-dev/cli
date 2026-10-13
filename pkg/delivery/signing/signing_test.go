package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
)

func testPayload(t *testing.T) []byte {
	t.Helper()
	// Use core's canonical fixture, so the verifier exercises the document too.
	for _, fixture := range solutionhost.FixturesOf(solutionhost.DocumentTypePresence) {
		raw, err := solutionhost.FixtureDocument(solutionhost.DocumentTypePresence, fixture.Name)
		if err != nil {
			continue
		}
		document, err := solutionhost.Parse(raw)
		if err != nil {
			continue
		}
		payload, err := solutionhost.SignedPayload(document)
		if err == nil {
			return payload
		}
	}
	t.Fatal("no valid core presence fixture")
	return nil
}

func TestKeyCheckVerifiesEnvelopeKeyAndOwnership(t *testing.T) {
	payload := testPayload(t)
	document, err := solutionhost.PresenceFromVerified(payload)
	if err != nil {
		t.Fatal(err)
	}
	signer := solutionhost.FixtureSigner()
	for _, kind := range []solutionhost.KeyKind{solutionhost.KeyKindRelease, solutionhost.KeyKindPerson} {
		check, err := KeyCheck(signer, kind, document.OwnershipDomain, signer.KeyID())
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
	if _, err = KeyCheck(signer, solutionhost.KeyKindRelease, document.OwnershipDomain, "unlisted"); err == nil {
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
