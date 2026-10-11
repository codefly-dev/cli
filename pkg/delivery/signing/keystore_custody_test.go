package signing

import (
	"context"
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
