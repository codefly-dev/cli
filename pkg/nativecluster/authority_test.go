package nativecluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/core/workloadidentity"
	"github.com/codefly-dev/core/workloadidentity/native"
)

const digest = "sha256:abababababababababababababababababababababababababababababababab"

func started(t *testing.T) (*Authority, *native.Attester) {
	t.Helper()
	authority, err := Start(context.Background())
	require.NoError(t, err)
	attester, err := native.NewAttester(authority.Anchor())
	require.NoError(t, err)
	return authority, attester
}

func TestACredentialAttestsTheIdentityAndAudienceItWasIssuedFor(t *testing.T) {
	authority, attester := started(t)
	token, err := authority.Token("acme-model", "model", "model-native", "uid-1", "accounts")
	require.NoError(t, err)

	attested, err := attester.Attest(context.Background(), token, "accounts")
	require.NoError(t, err)
	require.Equal(t, workloadidentity.Native, attested.Mechanism)
	require.Equal(t, "acme-model", attested.Namespace)
	require.Equal(t, "model", attested.Account)
	require.Equal(t, "model-native", attested.Instance)
	require.Equal(t, "uid-1", attested.InstanceUID)

	_, err = attester.Attest(context.Background(), token, "delivery")
	require.ErrorIs(t, err, workloadidentity.ErrRejected, "a credential is for the audience it was issued for")
}

func TestACredentialNamesTheBuildOnlyOnceTheRunKnowsIt(t *testing.T) {
	authority, attester := started(t)
	pod := &Pod{Namespace: "acme-model", ServiceAccount: "model", Name: "model-native", UID: "uid-1", Container: "model", Repository: "native/acme/model/model"}
	authority.RegisterPod(pod)

	early, err := authority.Token("acme-model", "model", "model-native", "uid-1", "accounts")
	require.NoError(t, err)
	attested, err := attester.Attest(context.Background(), early, "accounts")
	require.NoError(t, err)
	require.Len(t, attested.Containers, 1)
	require.Empty(t, attested.Containers[0].ImageID, "before delivery the build is not known, and the credential says so")

	pod.Digest = digest
	authority.RegisterPod(pod)
	late, err := authority.Token("acme-model", "model", "model-native", "uid-1", "accounts")
	require.NoError(t, err)
	attested, err = attester.Attest(context.Background(), late, "accounts")
	require.NoError(t, err)
	require.Equal(t, digest, attested.Containers[0].ImageID)
	require.Equal(t, "native/acme/model/model@"+digest, attested.Containers[0].DeclaredImage)
}

func TestAnotherInstanceAtTheSameIdentityIsNotVouchedForWithItsBuild(t *testing.T) {
	authority, attester := started(t)
	authority.RegisterPod(&Pod{Namespace: "acme-model", ServiceAccount: "model", Name: "model-native", UID: "uid-1", Container: "model", Digest: digest})
	token, err := authority.Token("acme-model", "model", "model-native", "uid-other", "accounts")
	require.NoError(t, err)
	attested, err := attester.Attest(context.Background(), token, "accounts")
	require.NoError(t, err)
	require.Empty(t, attested.Containers, "a credential for another instance names no container or build")
}

func TestARefreshIsANewCredential(t *testing.T) {
	authority, attester := started(t)
	workload := &Workload{Namespace: "acme-model", Account: "model", PodName: "model-native", PodUID: "uid-1", Audience: "accounts"}
	projection, err := authority.Project(t.TempDir(), workload)
	require.NoError(t, err)
	first, err := os.ReadFile(projection.TokenFile)
	require.NoError(t, err)
	require.NoError(t, authority.RefreshToken(projection.TokenFile, workload))
	second, err := os.ReadFile(projection.TokenFile)
	require.NoError(t, err)

	a, err := attester.Attest(context.Background(), string(first), "accounts")
	require.NoError(t, err)
	b, err := attester.Attest(context.Background(), string(second), "accounts")
	require.NoError(t, err)
	require.NotEqual(t, a.CredentialID, b.CredentialID, "a host can make each credential single-use")
}

func TestAProjectionIsTheWorkloadsCredentialOwnerOnly(t *testing.T) {
	authority, _ := started(t)
	dir := filepath.Join(t.TempDir(), "identity")
	projection, err := authority.Project(dir, &Workload{Namespace: "acme-model", Account: "model", PodName: "p", PodUID: "u", Audience: "accounts"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "token"), projection.TokenFile)
	info, err := os.Stat(projection.TokenFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing but the credential is projected")
}

func TestEachRunHasItsOwnAnchor(t *testing.T) {
	one, _ := started(t)
	other, attester := started(t)
	require.NotEqual(t, one.AnchorFingerprint(), other.AnchorFingerprint())
	token, err := one.Token("acme-model", "model", "p", "u", "accounts")
	require.NoError(t, err)
	_, err = attester.Attest(context.Background(), token, "accounts")
	require.ErrorIs(t, err, workloadidentity.ErrRejected, "another run's credential is not this run's")
}
