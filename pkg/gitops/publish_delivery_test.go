package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fakeSigner signs by wrapping the payload's length: enough to prove the
// carrier holds the bytes it was handed and a bundle the signer produced.
type fakeSigner struct{ signed int }

func (signer *fakeSigner) Sign(_ context.Context, payload []byte) ([]byte, error) {
	signer.signed++
	return json.Marshal(map[string]any{"mediaType": signing.MediaTypeBundle, "signedBytes": len(payload)})
}

// deliveryRepository is a clone-shaped repository whose base branch is reachable
// as refs/remotes/origin/main, with target the staged module path inside it.
type deliveryRepository struct {
	repo       string
	target     string
	targetPath string
}

func newDeliveryRepository(t *testing.T) *deliveryRepository {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	targetPath := "deployments/modules/crm"
	return &deliveryRepository{repo: repo, target: filepath.Join(repo, filepath.FromSlash(targetPath)), targetPath: targetPath}
}

// deliver commits whatever is staged under the target as the new base branch,
// the way a merged promotion does.
func (repository *deliveryRepository) deliver(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A", "--", repository.targetPath}, {"commit", "-q", "--allow-empty", "-m", "promote"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

// stageRender renders a module tree declaring the given solution instances
// straight into the repository's target, as publish stages it.
func (repository *deliveryRepository) stageRender(t *testing.T, instances ...string) *Inventory {
	t.Helper()
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances = nil
	for _, instance := range instances {
		options.SolutionInstances = append(options.SolutionInstances, SolutionInstance{
			Name: instance, Alias: instance, Package: "example/" + instance, Version: "1.4.0", Subject: instance + "@example.iam.test",
			Units: []SolutionArtifactUnit{{Name: "api", Path: "services/api"}},
		})
	}
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	inventory := result.Inventory
	return &inventory
}

func settledDocuments(t *testing.T, repository *deliveryRepository) map[string]solutionHostBindingConfigMap {
	t.Helper()
	directory := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")))
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	documents := map[string]solutionHostBindingConfigMap{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		require.NoError(t, err)
		var carrier solutionHostBindingConfigMap
		require.NoError(t, yaml.Unmarshal(data, &carrier))
		if carrier.Kind == kindConfigMap {
			documents[strings.TrimSuffix(entry.Name(), ".yaml")] = carrier
		}
	}
	return documents
}

// TestPublishSettlesGenerationsAgainstTheBaseBranch is the publish half of the
// generation rule: a first publish is generation 1; an unchanged re-publish
// keeps it; a change bumps it by one — all decided against what the base
// branch delivered, not against the render destination.
func TestPublishSettlesGenerationsAgainstTheBaseBranch(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Module: "crm"}

	inventory := repository.stageRender(t, "crm")
	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 1, signer.signed)
	require.Equal(t, []InventoryDeliveredDocument{{Kind: deliveredPresence, ID: "example.prod.crm", Generation: 1, Digest: delivery.Documents[0].Digest}}, delivery.Documents)
	documents := settledDocuments(t, repository)
	carrier := documents["example.prod.crm"]
	require.Contains(t, carrier.Data, solutionhost.FileName)
	var signed signedCarrier
	require.NoError(t, json.Unmarshal([]byte(carrier.Data[presenceCarrierKey]), &signed))
	require.Equal(t, signedCarrierSchema, signed.Schema)
	parsed, err := solutionhost.Parse(signed.Document)
	require.NoError(t, err)
	require.Equal(t, uint64(1), parsed.Generation)
	canonical, err := parsed.CanonicalBytes()
	require.NoError(t, err)
	require.Equal(t, string(canonical), string(signed.Document), "the carrier holds the canonical bytes verbatim")
	require.Contains(t, string(signed.Bundle), signing.MediaTypeBundle)
	// The Job delivers the carrier and the account exists; the kustomization
	// lists all three.
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	for _, want := range []string{"example.prod.crm.yaml", "deliver-presence.yaml", "delivery-service-account.yaml"} {
		require.Contains(t, string(kustomization), want)
	}
	repository.deliver(t)

	// Nothing changed: the generation holds.
	inventory = repository.stageRender(t, "crm")
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	require.Equal(t, uint64(1), delivery.Documents[0].Generation)
	repository.deliver(t)

	// The artifact changed: one bump.
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances[0].Package, options.SolutionInstances[0].Version = "example/crm", "1.4.0"
	result, err := RenderOwnedTree(ctx, options, renderWorkload(strings.Replace(pinnedDeployment, "aaaa", "bbbb", 1)))
	require.NoError(t, err)
	changed := result.Inventory
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &changed, opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), delivery.Documents[0].Generation)
}

// TestPublishTombstonesWhatThisModuleStoppedDeclaring is the removal rule: a
// binding this module delivered before and no longer declares is withdrawn by
// a tombstone at the next generation, carried forward verbatim on every later
// publish, and re-presenting it starts a new generation after the tombstone.
func TestPublishTombstonesWhatThisModuleStoppedDeclaring(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Module: "crm"}

	inventory := repository.stageRender(t, "crm", "billing")
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	repository.deliver(t)

	inventory = repository.stageRender(t, "crm")
	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	require.Len(t, delivery.Documents, 2)
	byID := map[string]InventoryDeliveredDocument{}
	for _, document := range delivery.Documents {
		byID[document.ID] = document
	}
	require.Equal(t, uint64(2), byID["example.prod.billing"].Generation)
	require.True(t, byID["example.prod.billing"].Removed)
	require.Equal(t, uint64(1), byID["example.prod.crm"].Generation)
	documents := settledDocuments(t, repository)
	tombstone, err := solutionhost.Parse([]byte(documents["example.prod.billing"].Data[solutionhost.FileName]))
	require.NoError(t, err)
	require.True(t, tombstone.Removed)
	require.Empty(t, tombstone.Routes)
	require.Contains(t, documents["example.prod.billing"].Data, presenceCarrierKey, "a tombstone is signed and delivered like any generation")
	repository.deliver(t)

	// Still gone: the tombstone is carried forward at the same generation.
	inventory = repository.stageRender(t, "crm")
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	for _, document := range delivery.Documents {
		if document.ID == "example.prod.billing" {
			require.Equal(t, uint64(2), document.Generation)
			require.True(t, document.Removed)
		}
	}
	repository.deliver(t)

	// Re-presented: a new generation after the tombstone, never the old one.
	inventory = repository.stageRender(t, "crm", "billing")
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	for _, document := range delivery.Documents {
		if document.ID == "example.prod.billing" {
			require.Equal(t, uint64(3), document.Generation)
			require.False(t, document.Removed)
		}
	}
}

// TestPublishRefusesUnsignedDocumentsOutsideLocalQualification pins the rule
// that a release is signed only by CI: with no signing identity, publish
// refuses and names the documents; only a local qualification publish may
// deliver them unsigned, and then renders no Job to POST them.
func TestPublishRefusesUnsignedDocumentsOutsideLocalQualification(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	unsigned := signing.FromEnvironment(func(string) (string, bool) { return "", false })
	inventory := repository.stageRender(t, "crm")

	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory,
		deliveryPublishOptions{Signer: unsigned, Target: testDeliveryTarget(), Module: "crm"})
	require.True(t, errors.Is(err, signing.ErrNoIdentity), "got %v", err)
	require.Contains(t, err.Error(), "example.prod.crm")
	require.Contains(t, err.Error(), "release workflow")

	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory,
		deliveryPublishOptions{Signer: unsigned, Target: testDeliveryTarget(), Module: "crm", AllowUnsigned: true})
	require.NoError(t, err)
	require.False(t, delivery.Signed)
	documents := settledDocuments(t, repository)
	require.NotContains(t, documents["example.prod.crm"].Data, presenceCarrierKey)
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	require.NotContains(t, string(kustomization), "deliver-presence.yaml", "nothing POSTs a document the host refuses")
}

// TestPublishRefusesToWithdrawEverythingWhenTheHostDeclarationIsGone guards
// the silent withdrawal: a render that names no host and declares nothing,
// over a delivery that declared bindings, is refused rather than tombstoned.
func TestPublishRefusesToWithdrawEverythingWhenTheHostDeclarationIsGone(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Module: "crm"}
	inventory := repository.stageRender(t, "crm")
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	repository.deliver(t)

	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.Host, options.SolutionInstances = nil, nil
	result, err := RenderOwnedTree(ctx, options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	hostless := result.Inventory
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &hostless,
		deliveryPublishOptions{Signer: &fakeSigner{}, Module: "crm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "example.prod.crm")
	require.Contains(t, err.Error(), "tombstones")
}
