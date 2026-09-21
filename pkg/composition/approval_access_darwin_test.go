//go:build darwin && cgo

package composition

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestApprovalAuthorityRejectsACLMutation(t *testing.T) {
	session, _, _, config, _, _ := approvalFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(parent, "host")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv(resources.CodeflyHomeEnv, home)
	installed, err := session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.NoError(t, err)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	for _, path := range []string{parent, home, filepath.Dir(installed.Path), installed.Path} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			grant := "everyone allow write,append,delete,writeattr,writeextattr,writesecurity,chown"
			if path != installed.Path {
				grant = "everyone allow search,delete_child,add_file,add_subdirectory"
			}
			output, aclErr := exec.CommandContext(t.Context(), "chmod", "+a", grant, path).CombinedOutput()
			require.NoError(t, aclErr, "%s", output)
			t.Cleanup(func() { require.NoError(t, exec.Command("chmod", "-N", path).Run()) })
			info, statErr := os.Stat(path)
			require.NoError(t, statErr)
			require.Zero(t, info.Mode().Perm()&0o022, "ACL attack leaves protected-looking mode bits")
			_, err = session.InspectApprovalAuthority(t.Context())
			require.ErrorContains(t, err, "ACL must not grant mutation")
			_, err = session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
			require.ErrorContains(t, err, "ACL must not grant mutation")
			require.ErrorContains(t, authority.unchanged(), "ACL must not grant mutation")
		})
	}
	output, err := exec.CommandContext(t.Context(), "chmod", "+a", "everyone deny delete", home).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() { require.NoError(t, exec.Command("chmod", "-N", home).Run()) })
	_, err = session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err, "ordinary macOS deny-delete ACL is not a mutation grant")
	output, err = exec.CommandContext(t.Context(), "chmod", "+a", "everyone allow read,readattr,readextattr,readsecurity", installed.Path).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() { require.NoError(t, exec.Command("chmod", "-N", installed.Path).Run()) })
	_, err = session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err, "read-only ACLs do not permit authority replacement")
}
