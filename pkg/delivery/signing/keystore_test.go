package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeveloperKeyPersistsWithDerivedIdentity(t *testing.T) {
	store := &DeveloperKeyStore{Directory: filepath.Join(t.TempDir(), "keys")}
	first, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID() != second.KeyID() {
		t.Fatal("created another machine key")
	}
	der, _ := x509.MarshalPKIXPublicKey(first.Public())
	digest := sha256.Sum256(der)
	if first.KeyID() != "dev-"+hex.EncodeToString(digest[:8]) {
		t.Fatal("key ID is not derived from PKIX")
	}
	signature, err := second.Sign([]byte("held locally"))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(first.Public().(ed25519.PublicKey), []byte("held locally"), signature) {
		t.Fatal("reloaded another key")
	}
	for path, mode := range map[string]os.FileMode{store.Directory: 0o700, filepath.Join(store.Directory, "current"): 0o600, filepath.Join(store.Directory, first.KeyID()+".pem"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode of %s: %v %v", path, info, err)
		}
	}
}

func TestDeveloperCustodyRefusesBroadPermissionsAndSymlinks(t *testing.T) {
	for _, which := range []string{"directory", "current", "private", "symlink"} {
		t.Run(which, func(t *testing.T) {
			store := &DeveloperKeyStore{Directory: filepath.Join(t.TempDir(), "keys")}
			signer, err := store.LoadOrCreate()
			if err != nil {
				t.Fatal(err)
			}
			path := store.Directory
			mode := os.FileMode(0o755)
			if which == "current" {
				path = filepath.Join(path, "current")
				mode = 0o644
			}
			if which == "private" || which == "symlink" {
				path = filepath.Join(path, signer.KeyID()+".pem")
				mode = 0o644
			}
			if which == "symlink" {
				target := path + ".target"
				if err = os.Rename(path, target); err == nil {
					err = os.Symlink(target, path)
				}
			} else {
				err = os.Chmod(path, mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.LoadOrCreate(); err == nil {
				t.Fatal("unsafe custody was read")
			}
		})
	}
}

func TestIndexedKeyIsNeverSilentlyReplaced(t *testing.T) {
	store := &DeveloperKeyStore{Directory: filepath.Join(t.TempDir(), "keys")}
	signer, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory, signer.KeyID()+".pem")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LoadOrCreate(); err == nil {
		t.Fatal("missing key silently replaced")
	}
	current, err := os.ReadFile(filepath.Join(store.Directory, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != signer.KeyID()+"\n" {
		t.Fatal("index changed")
	}
}

func TestCustodyBackendUsesTheIndexedID(t *testing.T) {
	dir := t.TempDir()
	store := &DeveloperKeyStore{Directory: filepath.Join(dir, "index"), Keychain: FileKeychain{Directory: filepath.Join(dir, "backend")}}
	if err := os.Mkdir(store.Keychain.(FileKeychain).Directory, 0o700); err != nil {
		t.Fatal(err)
	}
	signer, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != loaded.KeyID() {
		t.Fatal("backend lookup lost the indexed ID")
	}
	if _, err = os.Stat(filepath.Join(store.Directory, signer.KeyID()+".pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private key escaped backend")
	}
	public, err := PublicKeyPEM(signer)
	if err != nil || bytes.Contains([]byte(public), []byte("PRIVATE")) {
		t.Fatal("public export contains a secret")
	}
	if err = os.WriteFile(filepath.Join(store.Directory, "current"), []byte("dev-1111111111111111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(); err == nil {
		t.Fatal("wrong index accepted")
	}
}
