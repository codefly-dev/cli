package composition

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// currencyRepo builds a repository with n commits on main, tagging commit 1
// v0.1.0 and commit 2 v0.2.0, and returns its file URL and the commits.
func currencyRepo(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		out, err := command.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	var commits []string
	for i := 1; i <= n; i++ {
		git("commit", "-q", "--allow-empty", "-m", "c")
		commits = append(commits, git("rev-parse", "HEAD"))
		if i <= 2 {
			git("tag", "--no-sign", "v0."+string(rune('0'+i))+".0")
		}
	}
	return "file://" + dir, commits
}

func TestReadSourceHeadAndLag(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	url, commits := currencyRepo(t, 5)

	head, err := ReadSourceHead(context.Background(), url)
	require.NoError(t, err)
	require.Equal(t, "main", head.Branch)
	require.Equal(t, commits[4], head.Tip)
	require.Equal(t, "0.2.0", NewestRelease(head.Tags))
	require.Equal(t, 1, ReleasesNewerThan(head.Tags, "0.1.0"))
	require.Zero(t, ReleasesNewerThan(head.Tags, "0.2.0"))

	lag, err := CommitLag(context.Background(), url, head, commits[1])
	require.NoError(t, err)
	require.Equal(t, Lag{Behind: 3, OnBranch: true}, lag)

	lag, err = CommitLag(context.Background(), url, head, head.Tip)
	require.NoError(t, err)
	require.Equal(t, Lag{Behind: 0, OnBranch: true}, lag)

	// A commit the source has never seen is not "behind": it is off the branch.
	lag, err = CommitLag(context.Background(), url, head, strings.Repeat("a", 40))
	require.NoError(t, err)
	require.False(t, lag.OnBranch)
}

func TestReadSourceHeadRefusesAnUnreadableSource(t *testing.T) {
	_, err := ReadSourceHead(context.Background(), "file:///no/such/repository")
	require.Error(t, err)
}

func TestNewestReleaseReadsBothTagConventions(t *testing.T) {
	require.Equal(t, "0.3.1", NewestRelease([]string{"v0.2.0", "module-package/v0.3.1", "nightly", "v0.3.0-rc1"}))
	require.Empty(t, NewestRelease([]string{"nightly"}))
}
