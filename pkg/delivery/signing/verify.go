package signing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// LoadTrustedRoot reads a Sigstore trusted root (the TUF "trusted_root.json" document) from a
// file: the mirrored trust root a disconnected perimeter verifies against.
func LoadTrustedRoot(path string) (*root.TrustedRoot, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading the Sigstore trusted root: %w", err)
	}
	trusted, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing the Sigstore trusted root %s: %w", path, err)
	}
	return trusted, nil
}

// PublicGoodTrustedRoot fetches the current Sigstore public-good trust root through TUF. The TUF
// client caches its metadata under $HOME/.sigstore/root and verifies every update against the
// embedded TUF root, so a tampered mirror cannot hand out a trust root of its own.
func PublicGoodTrustedRoot(ctx context.Context) (*root.TrustedRoot, error) {
	trusted, err := root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetching the Sigstore public-good trusted root through TUF (%s): %w", tuf.DefaultMirror, err)
	}
	return trusted, nil
}

// Verify verifies that bundle is a valid Sigstore bundle whose signature covers exactly payload,
// signed under an identity the policy admits, with a transparency-log entry (or a signed
// timestamp) establishing the signing time, against the trusted root. It returns the Identity.
//
// A verification failure, including an unreadable bundle, is returned wrapped in ErrSignature.
// A policy that could never admit a signer is a configuration error, wrapped in ErrPolicy and
// never in ErrSignature, so an operator can tell a misconfigured verifier from a bad document.
func Verify(bundle, payload []byte, trusted *root.TrustedRoot, policy Policy) (Identity, error) {
	if trusted == nil {
		return Identity{}, errors.New("signing: a trusted root is required to verify a delivery document")
	}
	identity, err := policy.certificateIdentity()
	if err != nil {
		return Identity{}, err
	}
	entity, err := decodeBundle(bundle)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return Identity{}, fmt.Errorf("signing: configuring the Sigstore verifier: %w", err)
	}
	result, err := verifier.Verify(entity, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(payload)),
		verify.WithCertificateIdentity(identity),
	))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	if result.Signature == nil || result.Signature.Certificate == nil {
		return Identity{}, fmt.Errorf("%w: the bundle was not signed under a certificate", ErrSignature)
	}
	return identityOf(result.Signature.Certificate), nil
}

// ReadIdentity returns the Identity of the bundle's leaf certificate WITHOUT verifying anything:
// not the signature, not the certificate chain, not the log entry. The result is a claim made by
// whoever produced the bytes, suitable only for reporting ("signed by <subject> at <issuer>")
// next to, never instead of, Verify.
func ReadIdentity(bundle []byte) (Identity, error) {
	entity, err := decodeBundle(bundle)
	if err != nil {
		return Identity{}, err
	}
	content, err := entity.VerificationContent()
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	leaf := content.Certificate()
	if leaf == nil {
		return Identity{}, fmt.Errorf("%w: the bundle carries a public key hint, not a certificate, so it names no identity", ErrBundle)
	}
	summary, err := certificate.SummarizeCertificate(leaf)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: reading the certificate's identity: %w", ErrBundle, err)
	}
	return identityOf(&summary), nil
}

// decodeBundle parses and structurally validates a JSON bundle, wrapping every failure in
// ErrBundle so a corrupted file reads as such.
func decodeBundle(raw []byte) (*sigbundle.Bundle, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: the bundle is empty", ErrBundle)
	}
	var bundle sigbundle.Bundle
	if err := bundle.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	return &bundle, nil
}

func identityOf(summary *certificate.Summary) Identity {
	return Identity{Issuer: summary.Issuer, Subject: summary.SubjectAlternativeName}
}
