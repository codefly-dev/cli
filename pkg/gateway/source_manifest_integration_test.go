package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestGatewaySourceManifestUsesRootedCodeflySource proves the production
// Gateway returns exact worktree and revision identities without requiring or
// selecting a language agent. It uses a real filesystem and real Git history.
func TestGatewaySourceManifestUsesRootedCodeflySource(t *testing.T) {
	root := t.TempDir()
	writeGatewaySourceFile(t, root, "README.md", "base\n", 0o644)
	writeGatewaySourceFile(t, root, "bin/run", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("README.md", filepath.Join(root, "readme-link")); err != nil {
		t.Fatalf("create real symlink: %v", err)
	}
	gitGatewaySource(t, root, "init", "-b", "main")
	gitGatewaySource(t, root, "config", "commit.gpgsign", "false")
	gitGatewaySource(t, root, "add", ".")
	gitGatewaySource(t, root, "commit", "-m", "base")
	baseRevision := gitGatewaySource(t, root, "rev-parse", "HEAD")
	writeGatewaySourceFile(t, root, "README.md", "worktree\n", 0o644)
	writeGatewaySourceFile(t, root, "new.txt", "new\n", 0o644)

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	worktree, err := server.GetSourceManifest(t.Context(), &gatewayv1.GetSourceManifestRequest{})
	if err != nil {
		t.Fatalf("worktree source manifest: %v", err)
	}
	if worktree.GetFailure() != nil {
		t.Fatalf("worktree source manifest failure: %+v", worktree.GetFailure())
	}
	worktreeEntries := gatewaySourceEntriesByPath(worktree.GetManifest())
	if len(worktreeEntries) != 4 {
		t.Fatalf("worktree paths = %v, want four", gatewaySourcePaths(worktree.GetManifest()))
	}
	assertGatewaySHA256Entry(t, worktreeEntries["README.md"], "worktree\n")
	assertGatewaySHA256Entry(t, worktreeEntries["new.txt"], "new\n")
	if got := worktreeEntries["bin/run"]; got.GetMode() != 0o100755 {
		t.Fatalf("worktree executable = %+v", got)
	}
	if got := worktreeEntries["readme-link"]; got.GetKind() != basev0.SourceEntryKind_SOURCE_ENTRY_KIND_SYMLINK || got.GetMode() != 0o120000 {
		t.Fatalf("worktree symlink = %+v", got)
	}

	revision, err := server.GetSourceManifest(t.Context(), &gatewayv1.GetSourceManifestRequest{Revision: baseRevision})
	if err != nil {
		t.Fatalf("revision source manifest: %v", err)
	}
	if revision.GetFailure() != nil {
		t.Fatalf("revision source manifest failure: %+v", revision.GetFailure())
	}
	if revision.GetManifest().GetRevision() != baseRevision {
		t.Fatalf("resolved revision = %q, want %q", revision.GetManifest().GetRevision(), baseRevision)
	}
	revisionEntries := gatewaySourceEntriesByPath(revision.GetManifest())
	if len(revisionEntries) != 3 || revisionEntries["new.txt"] != nil {
		t.Fatalf("revision paths = %v, want committed source only", gatewaySourcePaths(revision.GetManifest()))
	}
	if got := revisionEntries["README.md"]; got.GetIdentity().GetAlgorithm() != basev0.SourceIdentityAlgorithm_SOURCE_IDENTITY_ALGORITHM_GIT_BLOB_SHA1 || got.GetIdentity().GetDigest() == worktreeEntries["README.md"].GetIdentity().GetDigest() {
		t.Fatalf("revision README identity = %+v", got)
	}

	contentRevision, err := server.GetSourceManifest(t.Context(), &gatewayv1.GetSourceManifestRequest{
		Revision:     baseRevision,
		IdentityMode: basev0.SourceManifestIdentityMode_SOURCE_MANIFEST_IDENTITY_MODE_CONTENT_SHA256,
	})
	if err != nil {
		t.Fatalf("content-normalized revision source manifest: %v", err)
	}
	if contentRevision.GetFailure() != nil {
		t.Fatalf("content-normalized revision failure: %+v", contentRevision.GetFailure())
	}
	contentEntries := gatewaySourceEntriesByPath(contentRevision.GetManifest())
	assertGatewaySHA256Entry(t, contentEntries["README.md"], "base\n")
	if got := contentEntries["README.md"].GetAttributes(); got.GetContentKind() != basev0.SourceContentKind_SOURCE_CONTENT_KIND_TEXT || got.GetSourceRole() != basev0.SourceRole_SOURCE_ROLE_DOCS {
		t.Fatalf("content-normalized README attributes = %+v", got)
	}
}

func writeGatewaySourceFile(t *testing.T, root, relative, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func gitGatewaySource(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = append(command.Environ(),
		"GIT_AUTHOR_NAME=codefly", "GIT_AUTHOR_EMAIL=test@codefly.dev",
		"GIT_COMMITTER_NAME=codefly", "GIT_COMMITTER_EMAIL=test@codefly.dev",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gatewaySourceEntriesByPath(value *basev0.SourceManifest) map[string]*basev0.SourceManifestEntry {
	entries := make(map[string]*basev0.SourceManifestEntry, len(value.GetEntries()))
	for _, entry := range value.GetEntries() {
		entries[entry.GetPath()] = entry
	}
	return entries
}

func gatewaySourcePaths(value *basev0.SourceManifest) []string {
	paths := make([]string, 0, len(value.GetEntries()))
	for _, entry := range value.GetEntries() {
		paths = append(paths, entry.GetPath())
	}
	return paths
}

func assertGatewaySHA256Entry(t *testing.T, entry *basev0.SourceManifestEntry, body string) {
	t.Helper()
	if entry == nil {
		t.Fatal("source manifest entry is missing")
	}
	digest := sha256.Sum256([]byte(body))
	if entry.GetIdentity().GetAlgorithm() != basev0.SourceIdentityAlgorithm_SOURCE_IDENTITY_ALGORITHM_SHA256 || entry.GetIdentity().GetDigest() != hex.EncodeToString(digest[:]) {
		t.Fatalf("source manifest entry = %+v", entry)
	}
}
