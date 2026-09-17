package composition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// tagged checkout builds a git repository whose HEAD sits `ahead` commits past
// tag, and returns its directory.
func taggedCheckout(t *testing.T, tag string, ahead int) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main", ".")
	runGit(t, dir, "config", "user.email", "checkout@test")
	runGit(t, dir, "config", "user.name", "Checkout Test")
	commit := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", ".")
		runGit(t, dir, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", name)
	}
	commit("released")
	runGit(t, dir, "-c", "tag.gpgSign=false", "tag", tag)
	for i := range ahead {
		commit("past-" + string(rune('a'+i)))
	}
	return dir
}

func TestDescribeCheckoutOnTheTag(t *testing.T) {
	description, ok := DescribeCheckout(context.Background(), taggedCheckout(t, "v0.0.62", 0))
	require.True(t, ok)
	require.Equal(t, "v0.0.62", description.Raw)
	require.Equal(t, "v0.0.62", description.Tag)
	require.Zero(t, description.Ahead)
	require.True(t, description.SatisfiesVersion("0.0.62"))
	require.True(t, description.SatisfiesVersion("v0.0.62"))
}

func TestDescribeCheckoutPastTheTag(t *testing.T) {
	description, ok := DescribeCheckout(context.Background(), taggedCheckout(t, "v0.0.62", 7))
	require.True(t, ok)
	require.Equal(t, "v0.0.62", description.Tag)
	require.Equal(t, 7, description.Ahead)
	require.False(t, description.SatisfiesVersion("0.0.62"))
}

// git describes from anywhere inside the working tree, so a pin pointing at a
// module subdirectory of a vendored repository is still compared against that
// repository's tags.
func TestDescribeCheckoutFromASubdirectory(t *testing.T) {
	root := taggedCheckout(t, "v0.0.62", 3)
	module := filepath.Join(root, "modules", "saas")
	require.NoError(t, os.MkdirAll(module, 0o755))
	description, ok := DescribeCheckout(context.Background(), module)
	require.True(t, ok)
	require.Equal(t, 3, description.Ahead)
	found, ok := CheckoutRoot(context.Background(), module)
	require.True(t, ok)
	rootPath, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, rootPath, found)
}

// A directory outside any working tree, and one whose HEAD has no reachable
// tag (the shape of a shallow CI submodule clone), both answer "unknown"
// rather than reporting a version.
func TestDescribeCheckoutWithoutAnAnswer(t *testing.T) {
	plain := t.TempDir()
	_, ok := CheckoutRoot(context.Background(), plain)
	require.False(t, ok)

	untagged := t.TempDir()
	runGit(t, untagged, "init", "--quiet", "--initial-branch=main", ".")
	runGit(t, untagged, "config", "user.email", "checkout@test")
	runGit(t, untagged, "config", "user.name", "Checkout Test")
	require.NoError(t, os.WriteFile(filepath.Join(untagged, "file"), []byte("x"), 0o644))
	runGit(t, untagged, "add", ".")
	runGit(t, untagged, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "untagged")
	_, ok = DescribeCheckout(context.Background(), untagged)
	require.False(t, ok)
}

func TestCheckoutSatisfiesVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tag     string
		ahead   int
		version string
		want    bool
	}{
		{"exact", "v1.2.3", 0, "1.2.3", true},
		{"exact with v", "v1.2.3", 0, "v1.2.3", true},
		{"module package tag convention", "module-package/v1.2.3", 0, "1.2.3", true},
		{"different tag", "v1.2.4", 0, "1.2.3", false},
		{"ahead of the tag", "v1.2.3", 7, "1.2.3", false},
		{"constraint satisfied", "v1.2.3", 0, "^1.2", true},
		{"constraint unsatisfied", "v2.0.0", 0, "^1.2", false},
		// Past the tag is a different tree than any released version names,
		// whatever constraint the pin states.
		{"constraint but ahead", "v1.2.3", 4, "^1.2", false},
		{"latest accepts any tag", "v1.2.3", 0, "latest", true},
		{"non-semver tag matched literally", "release-2024", 0, "release-2024", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			description := &CheckoutDescription{Raw: tc.tag, Tag: tc.tag, Ahead: tc.ahead}
			require.Equal(t, tc.want, description.SatisfiesVersion(tc.version))
		})
	}
}
