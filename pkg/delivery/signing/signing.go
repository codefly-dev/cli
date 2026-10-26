package signing

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/codefly-dev/core/solutionhost"
)

// Signer is core's custody boundary. Core owns envelope construction and verification.
type Signer = solutionhost.Signer

var ErrNoIdentity = errors.New("no delivery signing key: release publishing requires VAULT_ADDR, VAULT_TOKEN and CODEFLY_RELEASE_TRANSIT_KEY (Vault Transit custody) or CODEFLY_RELEASE_KMS_KEY (Cloud KMS custody)")
var ErrSignature = errors.New("delivery document signature verification failed")

// Unavailable records why this process cannot sign.
type Unavailable struct{ Reason string }

func (u Unavailable) KeyID() string            { return "unavailable" }
func (u Unavailable) Public() crypto.PublicKey { return nil }
func (u Unavailable) Sign([]byte) ([]byte, error) {
	if u.Reason == "" || u.Reason == ErrNoIdentity.Error() {
		return nil, ErrNoIdentity
	}
	return nil, fmt.Errorf("%s: %w", u.Reason, ErrNoIdentity)
}

// SignEnvelope preserves the actionable custody refusal before core requires a public key.
func SignEnvelope(payload []byte, signer Signer) (json.RawMessage, error) {
	switch u := signer.(type) {
	case *Unavailable:
		_, err := u.Sign(nil)
		return nil, err
	case Unavailable:
		_, err := u.Sign(nil)
		return nil, err
	}
	return solutionhost.SignEnvelope(payload, signer)
}

// PublicKeyPEM exports only the public half for a host's pins.
func PublicKeyPEM(signer Signer) (string, error) {
	if signer == nil {
		return "", ErrNoIdentity
	}
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return "", fmt.Errorf("encode delivery public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// SelfCheck holds fresh and reused carriers to the same key and domain.
type SelfCheck func(ctx context.Context, envelope, payload []byte) error

// KeyCheck refuses a custody key outside the environment's declaration before
// signing, and holds what was signed to the host's own verifier: a module
// manifest or a module envelope, its canonical bytes and its ownership domain.
func KeyCheck(signer Signer, kind solutionhost.KeyKind, domain string, admitted ...string) (SelfCheck, error) {
	if signer == nil {
		return nil, ErrNoIdentity
	}
	allowed := false
	for _, id := range admitted {
		if signer.KeyID() == id {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("delivery key %s does not match the environment's admitted keys %v", signer.KeyID(), admitted)
	}
	public, err := PublicKeyPEM(signer)
	if err != nil {
		return nil, err
	}
	keys, err := solutionhost.NewPinnedKeySet(solutionhost.PinnedKey{KeyID: signer.KeyID(), PublicKeyPEM: public, Kind: kind, OwnershipDomains: []string{domain}})
	if err != nil {
		return nil, err
	}
	verifier, err := solutionhost.NewVerifier(keys)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, envelope, payload []byte) error {
		carrier, err := solutionhost.Carrier(payload, envelope)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrSignature, err)
		}
		// Delivered verifies canonical content and ownership as well as the
		// envelope. A module envelope is verified as its host verifies it,
		// which admits only a release key: a key pinned here as a person's
		// fails the check rather than produce a carrier the host refuses.
		if solutionhost.SchemaOf(payload) == solutionhost.SchemaModuleEnvelopeV1 {
			_, err = solutionhost.VerifyDeliveredEnvelope(ctx, carrier, verifier)
		} else {
			_, err = solutionhost.VerifyDelivered(ctx, carrier, verifier)
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrSignature, err)
		}
		return nil
	}, nil
}

// ReadIdentity reads only a claimed key ID, for messages. It does not verify a signature.
func ReadIdentity(envelope []byte) (solutionhost.SignerIdentity, error) {
	var claim struct {
		Signatures []struct {
			KeyID string `json:"keyid"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(envelope, &claim); err != nil {
		return solutionhost.SignerIdentity{}, errors.New("unreadable delivery envelope")
	}
	if len(claim.Signatures) != 1 || claim.Signatures[0].KeyID == "" {
		return solutionhost.SignerIdentity{}, errors.New("delivery envelope must name exactly one signing key")
	}
	return solutionhost.SignerIdentity{KeyID: claim.Signatures[0].KeyID}, nil
}
