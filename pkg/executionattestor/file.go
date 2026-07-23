// Package executionattestor owns Codefly Gateway execution-attestation keys.
// It is product-neutral and never knows an exporter or receipt consumer.
package executionattestor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/codefly-dev/core/executionreceipt"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	"github.com/codefly-dev/core/resources"
)

const (
	keyFileSchema  = "codefly.execution-attestor-key/v1"
	keyFileName    = "gateway-ed25519-v1.json"
	maxKeyFileSize = 4 * 1024
	loadRetryDelay = 10 * time.Millisecond
	loadRetryLimit = 500 * time.Millisecond
)

var (
	// ErrInvalid is returned when key state is malformed or unsafe.
	ErrInvalid = errors.New("invalid Codefly execution attestor state")
)

// PublicIdentity is the stable enrollment material an exporter trust registry
// needs. It contains no private key.
type PublicIdentity struct {
	SignerID  string
	KeyID     string
	Algorithm string
	PublicKey ed25519.PublicKey
}

// Attestor signs canonical product-neutral execution receipts.
type Attestor interface {
	Attest(*executionv1.ExecutionReceiptV1) (*executionv1.ExecutionAttestationV1, error)
	Identity() PublicIdentity
	Verify(*executionv1.ExecutionAttestationV1) error
}

// FileAttestor is the explicitly local key-custody profile. Production
// gateways can implement Attestor with KMS/HSM custody without changing the
// journal, Gateway recorder, or exporter.
type FileAttestor struct {
	signerID  string
	keyID     string
	private   ed25519.PrivateKey
	publicKey ed25519.PublicKey
}

type keyFileV1 struct {
	Schema     string `json:"schema"`
	SignerID   string `json:"signer_id"`
	KeyID      string `json:"key_id"`
	Algorithm  string `json:"algorithm"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

// DefaultPath returns the SDK/Core-owned Codefly state location. It honors the
// standard Codefly home override through resources.CodeflyHomeDir.
func DefaultPath() string {
	return filepath.Join(resources.CodeflyHomeDir(), "execution", keyFileName)
}

// OpenFile creates or opens one owner-only local attestor key. Creation is
// exclusive and fsync-backed. Existing malformed state fails closed and is
// never silently replaced or rotated.
func OpenFile(ctx context.Context, path string) (*FileAttestor, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: key path is required", ErrInvalid)
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := rejectUnsafeFile(path); err != nil {
		return nil, err
	}
	if attestor, err := loadFile(path); err == nil {
		return attestor, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		// O_EXCL makes identity creation single-writer, but the final path is
		// visible before that writer completes its fsync. A concurrent opener
		// may therefore observe a zero/partial safe file. Retry only for this
		// bounded convergence window; persistent corruption still fails closed
		// and is never replaced.
		return loadFileAfterConcurrentCreate(ctx, path)
	}

	key, err := generateKey()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(key)
	if err != nil {
		return nil, fmt.Errorf("encode execution attestor key: %w", err)
	}
	payload = append(payload, '\n')

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return loadFileAfterConcurrentCreate(ctx, path)
	}
	if err != nil {
		return nil, fmt.Errorf("create execution attestor key: %w", err)
	}
	writeErr := writeAndSync(file, payload)
	closeErr := file.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("persist execution attestor key: %w", writeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close execution attestor key: %w", closeErr)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("sync execution attestor directory: %w", err)
	}
	if err := rejectUnsafeFile(path); err != nil {
		return nil, err
	}
	return parseKey(payload)
}

// Attest signs a canonical receipt without mutating it.
func (a *FileAttestor) Attest(receipt *executionv1.ExecutionReceiptV1) (*executionv1.ExecutionAttestationV1, error) {
	if a == nil || len(a.private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: attestor is not initialized", ErrInvalid)
	}
	return executionreceipt.Attest(receipt, a.signerID, a.keyID, a.private)
}

// Identity returns a defensive copy of the public enrollment material.
func (a *FileAttestor) Identity() PublicIdentity {
	if a == nil {
		return PublicIdentity{}
	}
	return PublicIdentity{
		SignerID:  a.signerID,
		KeyID:     a.keyID,
		Algorithm: executionreceipt.SignatureAlgorithm,
		PublicKey: append(ed25519.PublicKey(nil), a.publicKey...),
	}
}

// Verify proves an attestation was signed by this key and identity.
func (a *FileAttestor) Verify(attestation *executionv1.ExecutionAttestationV1) error {
	if a == nil || len(a.publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: attestor is not initialized", ErrInvalid)
	}
	if attestation.GetSignerId() != a.signerID || attestation.GetKeyId() != a.keyID {
		return fmt.Errorf("%w: attestation signer identity mismatch", ErrInvalid)
	}
	_, err := executionreceipt.Verify(attestation, a.publicKey)
	return err
}

func generateKey() (*keyFileV1, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate execution attestor key: %w", err)
	}
	signerBytes := make([]byte, 16)
	if _, err := rand.Read(signerBytes); err != nil {
		return nil, fmt.Errorf("generate execution attestor signer ID: %w", err)
	}
	keyDigest := sha256.Sum256(publicKey)
	return &keyFileV1{
		Schema:     keyFileSchema,
		SignerID:   "gateway-" + hex.EncodeToString(signerBytes),
		KeyID:      "ed25519-" + hex.EncodeToString(keyDigest[:]),
		Algorithm:  executionreceipt.SignatureAlgorithm,
		PublicKey:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}, nil
}

func loadFileAfterConcurrentCreate(ctx context.Context, path string) (*FileAttestor, error) {
	timer := time.NewTimer(loadRetryLimit)
	defer timer.Stop()
	ticker := time.NewTicker(loadRetryDelay)
	defer ticker.Stop()
	var lastErr error
	for {
		attestor, err := loadFile(path)
		if err == nil {
			return attestor, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("load concurrently created execution attestor key: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func loadFile(path string) (*FileAttestor, error) {
	if err := rejectUnsafeFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxKeyFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read execution attestor key: %w", err)
	}
	if len(payload) == 0 || len(payload) > maxKeyFileSize {
		return nil, fmt.Errorf("%w: execution attestor key size is invalid", ErrInvalid)
	}
	return parseKey(payload)
}

func parseKey(payload []byte) (*FileAttestor, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var stored keyFileV1
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("%w: decode execution attestor key: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing execution attestor key data", ErrInvalid)
	}
	if stored.Schema != keyFileSchema || stored.Algorithm != executionreceipt.SignatureAlgorithm {
		return nil, fmt.Errorf("%w: unsupported execution attestor key schema or algorithm", ErrInvalid)
	}
	publicKey, err := base64.RawURLEncoding.Strict().DecodeString(stored.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key is malformed", ErrInvalid)
	}
	privateKey, err := base64.RawURLEncoding.Strict().DecodeString(stored.PrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key is malformed", ErrInvalid)
	}
	derived := ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey)
	if !bytes.Equal(publicKey, derived) {
		return nil, fmt.Errorf("%w: public/private key mismatch", ErrInvalid)
	}
	keyDigest := sha256.Sum256(publicKey)
	if stored.KeyID != "ed25519-"+hex.EncodeToString(keyDigest[:]) {
		return nil, fmt.Errorf("%w: key ID does not match public key", ErrInvalid)
	}
	if len(stored.SignerID) < len("gateway-")+16 || len(stored.SignerID) > 128 {
		return nil, fmt.Errorf("%w: signer ID is malformed", ErrInvalid)
	}
	return &FileAttestor{
		signerID:  stored.SignerID,
		keyID:     stored.KeyID,
		private:   append(ed25519.PrivateKey(nil), privateKey...),
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
	}, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create execution attestor directory: %w", err)
		}
		info, err = os.Lstat(path)
	case err != nil:
		return fmt.Errorf("inspect execution attestor directory: %w", err)
	}
	if err != nil {
		return fmt.Errorf("inspect execution attestor directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: execution attestor parent must be a real directory", ErrInvalid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: execution attestor directory permissions %04o are not owner-only", ErrInvalid, info.Mode().Perm())
	}
	return nil
}

func rejectUnsafeFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect execution attestor key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: execution attestor key must be a regular file", ErrInvalid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: execution attestor key permissions %04o are not owner-only", ErrInvalid, info.Mode().Perm())
	}
	if info.Size() > maxKeyFileSize {
		return fmt.Errorf("%w: execution attestor key exceeds %d bytes", ErrInvalid, maxKeyFileSize)
	}
	return nil
}

func writeAndSync(file *os.File, payload []byte) error {
	for len(payload) > 0 {
		written, err := file.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return file.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
