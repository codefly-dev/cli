package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func target(t *testing.T, root string, secret bool) Target {
	t.Helper()
	return Target{ScopeDir: root, Profile: "local", Name: "auth", Secret: secret}
}

// gitRepo makes dir a real git repository with an ignore rule, because the
// secret guard shells out to real `git check-ignore` — there is no fake here.
func gitRepo(t *testing.T, ignore string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@codefly.dev"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	if ignore != "" {
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(ignore+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestWriteCreatesSecretFileWithRestrictivePermissions(t *testing.T) {
	root := gitRepo(t, "*.secret.env")
	tgt := target(t, root, true)

	doc, err := Load(tgt)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("TOKEN", "value")
	if err := Write(tgt, doc); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, err := os.Stat(tgt.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(tgt.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("secret directory mode = %o, want 700", got)
	}
}

func TestWriteRefusesSecretFileGitWouldTrack(t *testing.T) {
	root := gitRepo(t, "") // no ignore rule at all
	tgt := target(t, root, true)

	doc, err := Load(tgt)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("TOKEN", "value")

	err = Write(tgt, doc)
	if !errors.Is(err, ErrNotIgnored) {
		t.Fatalf("write error = %v, want ErrNotIgnored", err)
	}
	if _, statErr := os.Stat(tgt.Path()); !os.IsNotExist(statErr) {
		t.Fatalf("refused write still created %s", tgt.Path())
	}
}

func TestWriteAllowsNonSecretFileGitWouldTrack(t *testing.T) {
	root := gitRepo(t, "")
	tgt := target(t, root, false)

	doc, err := Load(tgt)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("EDGE_URL", "https://localhost:8080")
	if err := Write(tgt, doc); err != nil {
		t.Fatalf("non-secret write refused: %v", err)
	}
	if _, err := os.Stat(tgt.Path()); err != nil {
		t.Fatalf("non-secret file not created: %v", err)
	}
}

func TestWriteRefusesPlaintextWhenReferenceManifestOwnsTheConfiguration(t *testing.T) {
	root := gitRepo(t, "*.secret.env")
	tgt := target(t, root, true)
	if err := os.MkdirAll(tgt.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(tgt.Dir(), "auth.secret.ref.env")
	if err := os.WriteFile(reference, []byte("TOKEN=op://vault/auth/token\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, err := Load(tgt)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("TOKEN", "plaintext")

	err = Write(tgt, doc)
	if err == nil || !strings.Contains(err.Error(), "reference-only manifest") {
		t.Fatalf("write error = %v, want a reference-only manifest refusal", err)
	}
}

func TestDocumentPreservesCommentsAndReplacesInPlace(t *testing.T) {
	root := gitRepo(t, "*.secret.env")
	tgt := target(t, root, true)
	if err := os.MkdirAll(tgt.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# owned by the auth service\nTOKEN=old\n\nOTHER=keep\n"
	if err := os.WriteFile(tgt.Path(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := Load(tgt)
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("TOKEN", "new")
	doc.Set("ADDED", "value")
	if err := Write(tgt, doc); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(tgt.Path())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"# owned by the auth service", "TOKEN=new", "OTHER=keep", "ADDED=value"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rewritten file missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TOKEN=old") {
		t.Fatalf("old value survived the rewrite:\n%s", got)
	}
}

func TestHasTreatsEmptyValueAsMissing(t *testing.T) {
	doc := parseDocument([]byte("SET=value\nEMPTY=\n# COMMENT=value\n"))
	if !doc.Has("SET") {
		t.Fatal("SET should be present")
	}
	if doc.Has("EMPTY") {
		t.Fatal("EMPTY has no value and must count as missing")
	}
	if doc.Has("COMMENT") {
		t.Fatal("a commented line must not count as a value")
	}
}

func TestKeysAreReportedInFileOrder(t *testing.T) {
	doc := parseDocument([]byte("SECOND=b\nFIRST=a\n"))
	keys := doc.Keys()
	if len(keys) != 2 || keys[0] != "SECOND" || keys[1] != "FIRST" {
		t.Fatalf("keys = %v, want [SECOND FIRST]", keys)
	}
}

func TestGenerateProducesRequestedEncodingAndDistinctValues(t *testing.T) {
	first, err := Generate(FormatHex, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("hex of 32 bytes = %d chars, want 64", len(first))
	}
	second, err := Generate(FormatHex, 32)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two generated secrets must not be identical")
	}
	b64, err := Generate(FormatBase64, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(b64) != 44 {
		t.Fatalf("base64 of 32 bytes = %d chars, want 44", len(b64))
	}
}

func TestGenerateRejectsNonPositiveSize(t *testing.T) {
	if _, err := Generate(FormatHex, 0); err == nil {
		t.Fatal("expected an error for --bytes 0")
	}
}

func TestParseFormatRejectsUnknownEncoding(t *testing.T) {
	if _, err := ParseFormat("uuid"); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
	for _, in := range []string{"HEX", " base64 "} {
		if _, err := ParseFormat(in); err != nil {
			t.Fatalf("ParseFormat(%q) = %v, want success", in, err)
		}
	}
}

func TestTargetPathsFollowTheReaderLayout(t *testing.T) {
	secret := Target{ScopeDir: "/w", Profile: "local", Name: "auth", Secret: true}
	if got, want := secret.Path(), filepath.Join("/w", "configurations", "local", "auth.secret.env"); got != want {
		t.Fatalf("secret path = %s, want %s", got, want)
	}
	plain := Target{ScopeDir: "/w", Profile: "aws", Name: "edge"}
	if got, want := plain.Path(), filepath.Join("/w", "configurations", "aws", "edge.env"); got != want {
		t.Fatalf("plaintext path = %s, want %s", got, want)
	}
}
