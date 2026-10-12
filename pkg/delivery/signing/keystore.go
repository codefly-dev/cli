package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/codefly-dev/core/solutionhost"
)

const keychainService = "codefly-delivery-signing"

// securityItemNotFound is the exit status of macOS security when no item
// matches (errSecItemNotFound).
const securityItemNotFound = 44

var developerKeyID = regexp.MustCompile(`^dev-[0-9a-f]{16}$`)

// Keychain is private-key custody. Tests supply a temporary FileKeychain;
// they never invoke the operating system's credential tools.
type Keychain interface {
	Load(keyID string) ([]byte, error)
	Store(keyID string, privatePEM []byte) error
	// Holds reports whether custody keeps any developer key, indexed or not.
	Holds() (bool, error)
}

// DeveloperKeyStore holds one machine key, selected by its private current index.
// A nil Keychain uses permission-checked files in Directory.
type DeveloperKeyStore struct {
	Directory string
	Keychain  Keychain
}

// FileKeychain is the fallback for machines without an OS keychain.
type FileKeychain struct{ Directory string }

func (k FileKeychain) Load(id string) ([]byte, error) {
	if !developerKeyID.MatchString(id) {
		return nil, errors.New("invalid developer key ID")
	}
	if err := privatePath(k.Directory, true); err != nil {
		return nil, err
	}
	return readPrivate(filepath.Join(k.Directory, id+".pem"))
}
func (k FileKeychain) Holds() (bool, error) {
	if err := privatePath(k.Directory, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// A directory that cannot be listed is not an empty one.
	entries, err := os.ReadDir(k.Directory)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, "dev-") && strings.HasSuffix(name, ".pem") {
			return true, nil
		}
	}
	return false, nil
}

func (k FileKeychain) Store(id string, data []byte) error {
	if !developerKeyID.MatchString(id) {
		return errors.New("invalid developer key ID")
	}
	if err := privatePath(k.Directory, true); err != nil {
		return err
	}
	return writePrivate(filepath.Join(k.Directory, id+".pem"), data)
}

func machineKeyStore() (*DeveloperKeyStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	store := &DeveloperKeyStore{Directory: filepath.Join(home, ".codefly", "delivery-keys")}
	switch runtime.GOOS {
	case "darwin":
		store.Keychain = commandKeychain{command: "security"}
	case "linux":
		if path, lookErr := exec.LookPath("secret-tool"); lookErr == nil {
			store.Keychain = commandKeychain{command: path}
		}
	}
	return store, nil
}

func LoadOrCreateDeveloperKey() (solutionhost.Signer, error) {
	store, err := machineKeyStore()
	if err != nil {
		return nil, err
	}
	return store.LoadOrCreate()
}

// LoadDeveloperKey reads existing custody only; an anchor never invents a pin.
func LoadDeveloperKey() (solutionhost.Signer, error) {
	store, err := machineKeyStore()
	if err != nil {
		return nil, err
	}
	return store.Load()
}

func (s *DeveloperKeyStore) backend() Keychain {
	if s.Keychain != nil {
		return s.Keychain
	}
	return FileKeychain{Directory: s.Directory}
}
func (s *DeveloperKeyStore) Load() (solutionhost.Signer, error) {
	if err := privatePath(s.Directory, true); err != nil {
		return nil, err
	}
	current, err := readPrivate(filepath.Join(s.Directory, "current"))
	if err != nil {
		return nil, err
	}
	id := strings.TrimSuffix(string(current), "\n")
	if !developerKeyID.MatchString(id) {
		return nil, errors.New("the developer key index contains an invalid key ID")
	}
	raw, err := s.backend().Load(id)
	if err != nil {
		return nil, fmt.Errorf("load developer key %s from custody", id)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("developer custody does not hold one PKCS#8 private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("developer custody holds an unreadable private key")
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("developer custody requires an Ed25519 key")
	}
	public, isEd25519 := private.Public().(ed25519.PublicKey)
	if !isEd25519 {
		return nil, errors.New("developer custody requires an Ed25519 key")
	}
	derived, err := developerID(public)
	if err != nil {
		return nil, err
	}
	if derived != id {
		return nil, errors.New("developer key does not match its current index")
	}
	return solutionhost.NewInMemorySigner(id, private)
}

func (s *DeveloperKeyStore) LoadOrCreate() (solutionhost.Signer, error) {
	if err := os.MkdirAll(s.Directory, 0o700); err != nil {
		return nil, err
	}
	if err := privatePath(s.Directory, true); err != nil {
		return nil, err
	}
	// Serialize creation across processes. A failed concurrent attempt does not
	// generate a second key; the caller can retry after the owner finishes.
	lock := filepath.Join(s.Directory, ".create-lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		return nil, errors.New("developer key custody is locked; retry after the other key operation finishes (remove .create-lock only after a crashed owner)")
	}
	defer os.Remove(lock)
	signer, err := s.Load()
	if err == nil {
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Only an absent index over empty custody permits generation. An index lost
	// while custody still holds a key would otherwise mint a second identity
	// beside the one the environment admits.
	held, err := s.backend().Holds()
	if err != nil {
		return nil, err
	}
	if held {
		return nil, fmt.Errorf("developer key custody (service %q) already holds a key but its index %s is missing; "+
			"write that key's id (dev-…, the account of the custody entry) into the index to recover it — a second developer key is never created",
			keychainService, filepath.Join(s.Directory, "current"))
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	public, isEd25519 := private.Public().(ed25519.PublicKey)
	if !isEd25519 {
		return nil, errors.New("generated developer key is not Ed25519")
	}
	id, err := developerID(public)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, errors.New("encode developer private key")
	}
	if err = s.backend().Store(id, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, errors.New("store developer key in custody")
	}
	if err = writePrivate(filepath.Join(s.Directory, "current"), []byte(id+"\n")); err != nil {
		return nil, err
	}
	return solutionhost.NewInMemorySigner(id, private)
}

func developerID(public ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(der)
	return "dev-" + hex.EncodeToString(digest[:8]), nil
}
func privatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || (directory && info.Mode().Perm()&0o077 != 0 || !directory && info.Mode().Perm()&^os.FileMode(0o600) != 0) {
		return fmt.Errorf("developer custody path %s must be a private %s (0700 directory, 0600 file; no symlinks)", path, map[bool]string{true: "directory", false: "file"}[directory])
	}
	return nil
}
func readPrivate(path string) ([]byte, error) {
	if err := privatePath(path, false); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
func writePrivate(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

type commandKeychain struct{ command string }

// The macOS keychain stores the key base64-encoded and receives it on stdin
// through security's interactive mode: an argument would put the private key
// in every process listing on the machine.
func (k commandKeychain) macOS() bool { return k.command == "security" }

func (k commandKeychain) Load(id string) ([]byte, error) {
	var args []string
	if k.macOS() {
		args = []string{"find-generic-password", "-s", keychainService, "-a", id, "-w"}
	} else {
		args = []string{"lookup", "service", keychainService, "account", id}
	}
	out, err := exec.Command(k.command, args...).Output() //nolint:gosec // G204: a fixed credential tool (security, or secret-tool found on PATH) with constant arguments and a key ID validated as dev-<16 hex>; never a shell.
	if err != nil {
		return nil, errors.New("OS keychain could not read the developer key")
	}
	if k.macOS() {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
		if err != nil {
			return nil, errors.New("OS keychain holds an unreadable developer key")
		}
		return decoded, nil
	}
	return out, nil
}

// Holds answers false only when custody positively reports no entry: a query
// that fails — a locked keychain, an unavailable secret service — refuses
// generation rather than reading as empty.
func (k commandKeychain) Holds() (bool, error) {
	var stdout, stderr bytes.Buffer
	var cmd *exec.Cmd
	if k.macOS() {
		cmd = exec.Command(k.command, "find-generic-password", "-s", keychainService) //nolint:gosec // G204: a fixed credential tool (security, or secret-tool found on PATH) with constant arguments and a key ID validated as dev-<16 hex>; never a shell.
	} else {
		// search prints each match (secret included) into a buffer that is only
		// measured here, never kept or shown.
		cmd = exec.Command(k.command, "search", "--all", "service", keychainService) //nolint:gosec // G204: a fixed credential tool (security, or secret-tool found on PATH) with constant arguments and a key ID validated as dev-<16 hex>; never a shell.
	}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if k.macOS() {
		var exit *exec.ExitError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &exit) && exit.ExitCode() == securityItemNotFound:
			return false, nil
		}
		return false, errors.New("OS keychain could not be queried for an existing developer key")
	}
	if stdout.Len() > 0 {
		return true, nil
	}
	if err != nil || stderr.Len() > 0 {
		return false, errors.New("OS keychain could not be queried for an existing developer key")
	}
	return false, nil
}

func (k commandKeychain) Store(id string, secret []byte) error {
	var cmd *exec.Cmd
	if k.macOS() {
		cmd = exec.Command(k.command, "-i") //nolint:gosec // G204: a fixed credential tool (security, or secret-tool found on PATH) with constant arguments and a key ID validated as dev-<16 hex>; never a shell.
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -s %s -a %s -w %s\n",
			keychainService, id, base64.StdEncoding.EncodeToString(secret)))
	} else {
		cmd = exec.Command(k.command, "store", "--label=Codefly delivery signing", "service", keychainService, "account", id) //nolint:gosec // G204: a fixed credential tool (security, or secret-tool found on PATH) with constant arguments and a key ID validated as dev-<16 hex>; never a shell.
		cmd.Stdin = bytes.NewReader(secret)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("OS keychain could not store the developer key")
	}
	// Interactive mode exits 0 when a command inside it fails: read the key
	// back before trusting custody with it.
	stored, err := k.Load(id)
	if err != nil || !bytes.Equal(bytes.TrimSpace(stored), bytes.TrimSpace(secret)) {
		return errors.New("OS keychain did not keep the developer key")
	}
	return nil
}
