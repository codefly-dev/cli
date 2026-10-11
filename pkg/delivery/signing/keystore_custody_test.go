package signing

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A lost index with a key still in custody is recovered by hand, never by
// minting a second identity.
func TestLostIndexNeverMintsASecondKey(t *testing.T) {
	dir := t.TempDir()
	backend := FileKeychain{Directory: filepath.Join(dir, "backend")}
	if err := os.Mkdir(backend.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &DeveloperKeyStore{Directory: filepath.Join(dir, "index"), Keychain: backend}
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.Directory, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOrCreate(); err == nil || !strings.Contains(err.Error(), "never created") {
		t.Fatalf("a lost index minted a key or failed unclearly: %v", err)
	}
	held, _ := filepath.Glob(filepath.Join(backend.Directory, "dev-*.pem"))
	if len(held) != 1 {
		t.Fatalf("custody holds %d keys, want the one it had", len(held))
	}
}

// The macOS keychain receives the private key on stdin: a fake security tool
// records every argument it is given, and none of them may carry the key.
func TestMacOSKeychainNeverReceivesTheKeyAsAnArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	bin := t.TempDir()
	state := filepath.Join(bin, "state")
	argv := filepath.Join(bin, "argv")
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "` + argv + `"
case "$1" in
-i) read -r line; printf '%s' "$line" | sed 's/.* -w //' > "` + state + `" ;;
find-generic-password)
  case "$*" in *" -w"*) cat "` + state + `" ;; *) [ -s "` + state + `" ] || exit 44 ;; esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := &DeveloperKeyStore{Directory: filepath.Join(t.TempDir(), "index"), Keychain: commandKeychain{command: "security"}}
	signer, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(state)
	if err != nil || len(stored) == 0 {
		t.Fatal("the key never reached the keychain")
	}
	arguments, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(arguments), strings.TrimSpace(string(stored))) || strings.Contains(string(arguments), "PRIVATE KEY") {
		t.Fatal("the private key was passed as a process argument")
	}
	loaded, err := store.Load()
	if err != nil || loaded.KeyID() != signer.KeyID() {
		t.Fatalf("the stored key does not load back: %v", err)
	}
}

// A cleartext Vault request never goes through a proxy and dials only
// loopback, whatever the host name's case.
func TestCleartextVaultTransportStaysOnLoopback(t *testing.T) {
	transport, ok := vaultTransport(true).(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("a cleartext Vault request may consult a proxy")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "192.0.2.1:8200"); err == nil {
		t.Fatal("a cleartext Vault request dialed a non-loopback address")
	}
	if secure := vaultTransport(false).(*http.Transport); secure.DialContext == nil {
		t.Fatal("the HTTPS transport lost its dialer")
	}
}

// A keychain that cannot be queried is not an empty one: with the index lost,
// generation is refused rather than minting a second identity.
func TestUnqueryableKeychainRefusesGeneration(t *testing.T) {
	bin := t.TempDir()
	// Every query fails with a status other than "no such item".
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 36\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := &DeveloperKeyStore{Directory: filepath.Join(t.TempDir(), "index"), Keychain: commandKeychain{command: "security"}}
	if _, err := store.LoadOrCreate(); err == nil || !strings.Contains(err.Error(), "could not be queried") {
		t.Fatalf("an unqueryable keychain read as empty: %v", err)
	}
}

// A custody directory that cannot be listed refuses generation.
func TestUnlistableFileCustodyRefusesGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists every directory")
	}
	backend := FileKeychain{Directory: filepath.Join(t.TempDir(), "custody[1]")}
	if err := os.Mkdir(backend.Directory, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(backend.Directory, 0o700) })
	if held, err := backend.Holds(); err == nil {
		t.Fatalf("an unlistable custody directory answered held=%v without error", held)
	}
}

// A literal directory name with pattern characters is listed, not matched.
func TestFileCustodyListsALiteralDirectory(t *testing.T) {
	backend := FileKeychain{Directory: filepath.Join(t.TempDir(), "custody[1]")}
	if err := os.Mkdir(backend.Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend.Directory, "dev-0123456789abcdef.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if held, err := backend.Holds(); err != nil || !held {
		t.Fatalf("held=%v err=%v, want a held key", held, err)
	}
}

// When the first loopback address refuses, the next one is tried.
func TestCleartextVaultTransportFallsBackAcrossLoopback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no IPv4 loopback listener")
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	transport := vaultTransport(true).(*http.Transport)
	// localhost resolves to ::1 and 127.0.0.1 here; nothing listens on ::1.
	conn, err := transport.DialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("no fallback to the IPv4 loopback listener: %v", err)
	}
	conn.Close()
}
