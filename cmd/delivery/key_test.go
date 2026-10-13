package delivery

import (
	"bytes"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
)

func TestKeyPrintsOnlyThePersistentPublicIdentity(t *testing.T) {
	store := &signing.DeveloperKeyStore{Directory: t.TempDir() + "/keys"}
	before := loadKey
	loadKey = store.LoadOrCreate
	t.Cleanup(func() { loadKey = before; KeyCmd.SetOut(nil) })
	var out bytes.Buffer
	KeyCmd.SetOut(&out)
	if err := KeyCmd.RunE(KeyCmd, nil); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	out.Reset()
	if err := KeyCmd.RunE(KeyCmd, nil); err != nil {
		t.Fatal(err)
	}
	if first != out.String() {
		t.Fatal("key changed on second call")
	}
	key, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	public, err := signing.PublicKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if first != key.KeyID()+"\n"+public || strings.Contains(first, "PRIVATE") {
		t.Fatal("output was not exactly the ID and public PEM")
	}
}
