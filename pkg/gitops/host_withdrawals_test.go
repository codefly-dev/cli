package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

const legacyBinding = "example.prod.legacy"

// legacyTreePath is the delivered tree of a module named legacy beside the
// delivery repository fixture's own (deployments/modules/crm).
const legacyTreePath = "deployments/modules/legacy"

// commitOnBase writes files (repository-relative; nil removes the path) and
// commits them as the new base branch, the way a merged promotion does.
func commitOnBase(t *testing.T, repository *deliveryRepository, message string, files map[string][]byte) {
	t.Helper()
	for relative, data := range files {
		path := filepath.Join(repository.repo, filepath.FromSlash(relative))
		if data == nil {
			require.NoError(t, os.RemoveAll(path))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o644))
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "--allow-empty", "-m", message}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

// legacyManifest is the legacy module's live manifest at generation.
func legacyManifest(t *testing.T, generation uint64) *solutionhost.ModuleManifest {
	t.Helper()
	manifest, err := ModuleManifestOf(legacyBinding, testHost(),
		&ModuleInstance{Kind: solutionhost.KindModule, Name: "legacy", Package: "example/legacy", Version: "1.0.0", Authority: &solutionhost.Authority{Principal: "legacy"}},
		[]solutionhost.Workload{{Service: "worker", Container: "worker"}})
	require.NoError(t, err)
	manifest.Generation = generation
	return manifest
}

// legacyDelivered is the legacy module's delivered overlay for prod, as its
// own publish left it: manifest, signed, in its own namespace.
func legacyDelivered(t *testing.T, signer signing.Signer, manifest *solutionhost.ModuleManifest) map[string][]byte {
	t.Helper()
	payload, err := solutionhost.SignedPayload(manifest)
	require.NoError(t, err)
	envelope, err := signing.SignEnvelope(payload, signer)
	require.NoError(t, err)
	signed, err := solutionhost.Carrier(payload, envelope)
	require.NoError(t, err)
	carrier, err := solutionhost.MarshalSigned(signed)
	require.NoError(t, err)
	scratch := t.TempDir()
	tree, err := newRenderRoot(scratch)
	require.NoError(t, err)
	defer tree.Close()
	file, err := writeModuleManifestInTree(tree, scratch, "legacy", manifest, carrier)
	require.NoError(t, err)
	configMap, err := os.ReadFile(filepath.Join(scratch, file))
	require.NoError(t, err)
	overlay := legacyTreePath + "/" + moduleManifestOverlay("prod")
	return map[string][]byte{
		legacyTreePath + "/" + InventoryFilename: []byte(`{"module":"legacy","environment":"prod"}`),
		overlay + "/" + file:                     configMap,
		overlay + "/" + kustomizationFile:        []byte("resources:\n  - " + file + "\n"),
	}
}

// hostClaim is a composition that declares the host module crm and the
// modules named.
func hostClaim(declared ...string) *compositionClaim {
	claim := &compositionClaim{module: "crm", declared: map[string]bool{"crm": true}}
	for _, module := range declared {
		claim.declared[module] = true
	}
	return claim
}

// settleAsHost settles the host module crm's render with the withdrawals the
// composition's removals found.
func settleAsHost(t *testing.T, repository *deliveryRepository, signer *fakeSigner, withdraw map[string]deliveredManifest) *InventoryDelivery {
	t.Helper()
	opts := publishOptions(signer)
	opts.Coordinate = testHost().Coordinate
	opts.Withdraw = withdraw
	inventory := repository.stageRender(t, "crm")
	inventory.HostsDelivery = true
	delivery, err := repository.settle(inventory, &opts)
	require.NoError(t, err)
	return delivery
}

// TestTheHostModuleWithdrawsAModuleRemovedInAnEarlierCommit is the case the
// module's own tree can no longer answer: the composition dropped legacy and
// an earlier publication already deleted its tree, namespace and Job. The
// host module's publish reads what legacy last delivered from the base
// branch's history and withdraws it at the next generation from its own
// overlay, signed like a live manifest and POSTed by its own Job; delivered,
// the withdrawal is carried forward verbatim, never written twice, and
// re-delivered it is a replay the host accepts.
func TestTheHostModuleWithdrawsAModuleRemovedInAnEarlierCommit(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	commitOnBase(t, repository, "legacy delivers", legacyDelivered(t, signer, legacyManifest(t, 2)))
	commitOnBase(t, repository, "legacy leaves the composition", map[string][]byte{legacyTreePath: nil})

	withdraw, err := composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", nil, hostClaim())
	require.NoError(t, err)
	require.Len(t, withdraw, 1)
	require.Equal(t, uint64(2), withdraw[legacyBinding].manifest.Generation, "the generation legacy last delivered, read from history")

	delivery := settleAsHost(t, repository, signer, withdraw)
	document := documentByID(delivery, legacyBinding)
	require.Equal(t, InventoryDeliveredDocument{Kind: deliveredManifestKind, ID: legacyBinding, Generation: 3, Removed: true, Digest: document.Digest}, document)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation, "the host module's own binding is settled as before")
	withdrawal := signedManifest(t, repository, legacyBinding)
	require.True(t, withdrawal.Removed)
	require.Equal(t, uint64(3), withdrawal.Generation)
	require.Equal(t, "legacy", withdrawal.Alias)
	require.Empty(t, withdrawal.Workloads, "a withdrawal carries the identity of what it withdraws and nothing else")
	overlay := filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")))
	job, err := os.ReadFile(filepath.Join(overlay, "deliver-manifest.yaml"))
	require.NoError(t, err)
	for _, want := range []string{"namespace: crm", "serviceAccountName: delivery", moduleManifestConfigMapName(legacyBinding), moduleManifestConfigMapName("example.prod.crm")} {
		require.Contains(t, string(job), want, "the host module's own Job carries the withdrawal beside its live manifest")
	}
	require.Equal(t, []string{legacyBinding + " (module legacy) at generation 3"}, withdrawnByHost(delivery, withdraw))
	carried := settledDocuments(t, repository)[legacyBinding].Data[solutionhost.SignedFileName]
	signed := signer.signed
	repository.deliver(t)

	// Published again: history still names legacy, and the host module's base
	// branch now carries its withdrawal, which is carried forward verbatim.
	withdraw, err = composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", nil, hostClaim())
	require.NoError(t, err)
	delivery = settleAsHost(t, repository, signer, withdraw)
	require.Len(t, delivery.Documents, 2, "the withdrawal is not duplicated")
	require.Equal(t, uint64(3), documentByID(delivery, legacyBinding).Generation)
	require.Equal(t, carried, settledDocuments(t, repository)[legacyBinding].Data[solutionhost.SignedFileName], "carried forward verbatim")
	require.Equal(t, signed, signer.signed, "a withdrawal already delivered is not signed again")
	decision, err := solutionhost.Admit(withdrawal.Host.Coordinate, withdrawal, signedManifest(t, repository, legacyBinding))
	require.NoError(t, err)
	require.Equal(t, solutionhost.DecisionReplay, decision, "re-delivered, an applied withdrawal is a replay the host accepts")
}

// TestTheHostModuleWithdrawsAModuleThisPublicationRemoves: legacy left the
// composition in this checkout and its tree is still on the base branch —
// this publication removes it — so what it delivered is read there.
func TestTheHostModuleWithdrawsAModuleThisPublicationRemoves(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	commitOnBase(t, repository, "legacy delivers", legacyDelivered(t, signer, legacyManifest(t, 1)))

	withdraw, err := composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", []string{legacyTreePath}, hostClaim())
	require.NoError(t, err)
	delivery := settleAsHost(t, repository, signer, withdraw)
	require.Equal(t, uint64(2), documentByID(delivery, legacyBinding).Generation)
	require.True(t, documentByID(delivery, legacyBinding).Removed)
}

// TestNothingIsWithdrawnForAModuleStillComposed: a module the composition
// declares is never withdrawn by the host, whatever history says of its
// tree; a module whose tree already withdrew its binding is not withdrawn
// again; and a module other than the one hosting the delivery API writes no
// withdrawal of another module's binding.
func TestNothingIsWithdrawnForAModuleStillComposed(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	commitOnBase(t, repository, "legacy delivers", legacyDelivered(t, signer, legacyManifest(t, 1)))
	commitOnBase(t, repository, "legacy's tree is gone", map[string][]byte{legacyTreePath: nil})

	withdraw, err := composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", nil, hostClaim("legacy"))
	require.NoError(t, err)
	require.Empty(t, withdraw, "the composition still declares legacy")

	withdraw, err = composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", nil, hostClaim())
	require.NoError(t, err)
	require.NotEmpty(t, withdraw)
	opts := publishOptions(signer)
	opts.Withdraw = withdraw
	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Len(t, delivery.Documents, 1, "only the module hosting the delivery API carries withdrawals")

	withdrawn := newDeliveryRepository(t)
	withdrawal, err := WithdrawalOf(legacyManifest(t, 1))
	require.NoError(t, err)
	commitOnBase(t, withdrawn, "legacy withdraws its binding", legacyDelivered(t, signer, withdrawal))
	withdraw, err = composedOutManifests(context.Background(), withdrawn.repo, "deployments", "main", "prod", []string{legacyTreePath}, hostClaim())
	require.NoError(t, err)
	require.Empty(t, withdraw, "a binding its own tree withdrew is not withdrawn again")
}

// TestTheHostModuleRefusesToWithdrawABindingALiveTreeDelivers: a binding a
// dropped module delivered that another tree on the base branch delivers
// live is that module's now, and the host module's publish refuses by name
// rather than withdraw it.
func TestTheHostModuleRefusesToWithdrawABindingALiveTreeDelivers(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	commitOnBase(t, repository, "legacy delivers", legacyDelivered(t, signer, legacyManifest(t, 1)))
	moved := map[string][]byte{legacyTreePath: nil}
	for path, data := range legacyDelivered(t, signer, legacyManifest(t, 1)) {
		moved[strings.Replace(path, legacyTreePath, "deployments/modules/successor", 1)] = data
	}
	commitOnBase(t, repository, "the instance moves to another module", moved)

	_, err := composedOutManifests(context.Background(), repository.repo, "deployments", "main", "prod", nil, hostClaim("successor"))
	require.Error(t, err)
	for _, want := range []string{legacyBinding, "deployments/modules/successor", "delivered live"} {
		require.Contains(t, err.Error(), want)
	}
}
