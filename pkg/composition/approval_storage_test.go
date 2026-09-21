package composition

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/policy"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestApprovalAuthorityRejectsReplaceableAncestors(t *testing.T) {
	session, _, _, config, _, _ := approvalFixture(t)
	for _, mode := range []os.FileMode{0o777, 0o770} {
		parent, err := filepath.EvalSymlinks(t.TempDir())
		require.NoError(t, err)
		home := filepath.Join(parent, "intermediate", "host")
		require.NoError(t, os.MkdirAll(home, 0o700))
		require.NoError(t, os.Chmod(parent, mode))
		t.Setenv(resources.CodeflyHomeEnv, home)
		_, err = session.ConfigureApprovalAuthority(t.Context(), config, "")
		require.ErrorContains(t, err, "directory must not be group/world writable")
		_, err = os.Stat(filepath.Join(home, "composition-authorities"))
		require.ErrorIs(t, err, os.ErrNotExist, "reject unsafe ancestry before creating registry")
	}
}

func TestApprovalRejectsAncestorReplacementAfterConfiguration(t *testing.T) {
	session, files, recorded, config, _, now := approvalFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(parent, "host")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv(resources.CodeflyHomeEnv, home)
	installed, err := session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.NoError(t, err)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)

	// These current-UID operations model the rename rights a different UID gets
	// through a non-sticky writable ancestor; they are not a second-UID test.
	require.NoError(t, os.Chmod(parent, 0o777))
	require.ErrorContains(t, authority.unchanged(), "directory must not be group/world writable")
	require.NoError(t, os.Rename(home, home+"-displaced"))
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.Mkdir(filepath.Dir(installed.Path), 0o755))
	attackerKey, attackerPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	config.Key = attackerKey
	document, err := normalizedApprovalAuthority(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(installed.Path, document, 0o644))
	claims := policy.MintInput{Principal: &config.Approver, Action: approvalAction, Resource: recorded.Identity,
		AudienceID: config.Audience, CatalogDigest: contentDigest(document),
		RequestDigest: recorded.Record.BindingIdentity, TTL: time.Minute, MaxUses: 1,
		NowFunc: func() time.Time { return now }}
	token, _, err := policy.MintEd25519(claims, attackerPrivate)
	require.NoError(t, err)
	_, err = session.CheckApproval(t.Context(), files, &DeploymentApproval{Admission: *recorded, Authorization: token}, now)
	require.ErrorContains(t, err, "directory must not be group/world writable")
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "directory must not be group/world writable")
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
	require.ErrorContains(t, err, "directory must not be group/world writable")
	_, err = readAuthorityDocument(installed.Path)
	require.ErrorContains(t, err, "directory must not be group/world writable")
	require.ErrorContains(t, authority.unchanged(), "directory must not be group/world writable")
}

func TestApprovalAuthorityAcceptsTrustedStickyAncestors(t *testing.T) {
	session, _, _, config, _, _ := approvalFixture(t)
	// Exercise the real system /tmp ancestry, including its root-owned sticky
	// directory and (on macOS) canonical /private/tmp spelling.
	temporary, err := os.MkdirTemp("/tmp", "codefly-approval-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(temporary)) })
	parent, err := filepath.EvalSymlinks(temporary)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, os.ModeSticky|0o777))
	home := filepath.Join(parent, "host")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv(resources.CodeflyHomeEnv, home)
	installed, err := session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.NoError(t, err)
	inspected, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	require.Equal(t, installed, inspected)
	for _, path := range []string{home, filepath.Dir(installed.Path)} {
		require.NoError(t, os.Chmod(path, os.ModeSticky|0o777))
		_, err = session.InspectApprovalAuthority(t.Context())
		require.ErrorContains(t, err, "directory must not be group/world writable", "sticky is only allowed on ancestors, not host/registry")
		require.NoError(t, os.Chmod(path, 0o700))
	}
}

func TestApprovalAuthorityHandlesStayOnValidatedStorage(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(parent, "registry", "authority.json")
	directory, err := openAuthorityRegistry(path, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	require.NoError(t, writeAuthorityDocumentAt(t.Context(), directory, "authority.json", []byte("original")))
	displaced := filepath.Join(parent, "displaced")
	require.NoError(t, os.Rename(filepath.Dir(path), displaced))
	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))
	read, err := readAuthorityDocumentAt(directory, "authority.json")
	require.NoError(t, err)
	require.Equal(t, "original", string(read))
	require.NoError(t, writeAuthorityDocumentAt(t.Context(), directory, "authority.json", []byte("updated")))
	read, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(read), "write must not resolve the replaced path")
	read, err = os.ReadFile(filepath.Join(displaced, "authority.json"))
	require.NoError(t, err)
	require.Equal(t, "updated", string(read))
}

func TestApprovalAuthorityValidatesHomeAliasAncestry(t *testing.T) {
	session, _, _, config, _, _ := approvalFixture(t)
	installed, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	home := filepath.Dir(filepath.Dir(installed.Path))
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	alias := filepath.Join(parent, "host")
	require.NoError(t, os.Symlink(home, alias))
	t.Setenv(resources.CodeflyHomeEnv, alias)
	inspected, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err, "a protected trusted-owned alias remains usable")
	require.Equal(t, installed, inspected)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, 0o777))
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "directory must not be group/world writable")
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
	require.ErrorContains(t, err, "directory must not be group/world writable")
	require.ErrorContains(t, authority.unchanged(), "directory must not be group/world writable")
	require.NoError(t, os.Chmod(parent, 0o700))
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink("missing/../host", alias))
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "without parent traversal")
}

func TestApprovalAuthorityRejectsForeignOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing real filesystem ownership requires root; no synthetic stat substitutes")
	}
	session, _, _, config, _, _ := approvalFixture(t)
	installed, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	for _, path := range []string{installed.Path, filepath.Dir(installed.Path), filepath.Dir(filepath.Dir(installed.Path))} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			require.NoError(t, os.Chown(path, 65534, -1))
			t.Cleanup(func() { require.NoError(t, os.Chown(path, 0, -1)) })
			_, err = session.InspectApprovalAuthority(t.Context())
			require.ErrorContains(t, err, "owned by root or the effective user")
			_, err = session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
			require.ErrorContains(t, err, "owned by root or the effective user")
			require.ErrorContains(t, authority.unchanged(), "owned by root or the effective user")
			_, err = readAuthorityDocument(installed.Path)
			require.ErrorContains(t, err, "owned by root or the effective user")
		})
	}
	// A sticky parent protects only trusted-owned children, not a foreign home.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(parent, os.ModeSticky|0o777))
	home := filepath.Join(parent, "host")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.Chown(home, 65534, -1))
	t.Setenv(resources.CodeflyHomeEnv, home)
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.ErrorContains(t, err, "owned by root or the effective user")
}
