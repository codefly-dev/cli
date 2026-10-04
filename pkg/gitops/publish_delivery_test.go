package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/modulecontract"
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
			Kind: solutionhost.KindSolution, Name: instance, Alias: instance, Package: "example/" + instance, Version: "1.4.0",
			ReleaseDigest: solutionhost.ReleaseDigest("sha256:" + strings.Repeat("c", 64)),
			Units:         []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: instance + "@example.iam.test"}},
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
	opts := deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}

	inventory := repository.stageRender(t, "crm")
	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 1, signer.signed)
	require.Equal(t, []InventoryDeliveredDocument{{Kind: deliveredPresence, ID: "example.prod.crm", Generation: 1, Digest: delivery.Documents[0].Digest}}, delivery.Documents)
	documents := settledDocuments(t, repository)
	carrier := documents["example.prod.crm"]
	require.Contains(t, carrier.Data, solutionhost.FileName)
	signed, err := solutionhost.ParseSigned([]byte(carrier.Data[presenceCarrierKey]))
	require.NoError(t, err)
	require.Equal(t, solutionhost.SchemaSignedV1, signed.Schema)
	// The host's own reader of a verified payload: it refuses any payload that
	// is not the canonical encoding of the document it decodes to — the YAML
	// written beside the carrier would be refused even under a genuine
	// attestation, so the signed bytes are CanonicalBytes, never Marshal.
	parsed, err := solutionhost.PresenceFromVerified(signed.Document)
	require.NoError(t, err)
	require.Equal(t, uint64(1), parsed.Generation)
	canonical, err := parsed.CanonicalBytes()
	require.NoError(t, err)
	require.Equal(t, string(canonical), string(signed.Document), "the carrier holds the canonical bytes verbatim")
	yamlHalf := []byte(carrier.Data[solutionhost.FileName])
	_, err = solutionhost.PresenceFromVerified(yamlHalf)
	require.Error(t, err, "the YAML half is for the repository; the host refuses it as a signed payload")
	require.Contains(t, string(signed.Bundle), signing.MediaTypeBundle)
	// The Job delivers the carrier and the account exists; the kustomization
	// lists all three.
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	for _, want := range []string{"example.prod.crm.yaml", "deliver-presence.yaml", "delivery-service-account.yaml"} {
		require.Contains(t, string(kustomization), want)
	}
	repository.deliver(t)

	// Nothing changed: the generation holds, the carrier is the one delivered
	// before — nothing is re-signed, so a no-op promotion writes the same
	// bytes, enters nothing in the log, and gives a re-sync nothing to do —
	// and the Job that posts it keeps its name.
	before := settledDocuments(t, repository)
	jobBefore, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "deliver-presence.yaml"))
	require.NoError(t, err)
	inventory = repository.stageRender(t, "crm")
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(1), delivery.Documents[0].Generation)
	require.Equal(t, 1, signer.signed, "an unchanged document is not signed again")
	require.True(t, delivery.Signed, "a reused carrier is a signed carrier")
	after := settledDocuments(t, repository)
	require.Equal(t, before["example.prod.crm"].Data[presenceCarrierKey], after["example.prod.crm"].Data[presenceCarrierKey], "the carrier is byte-identical")
	jobAfter, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "deliver-presence.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(jobBefore), string(jobAfter), "the same set is the same Job")
	repository.deliver(t)

	// The artifact changed: one bump.
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances[0].Package, options.SolutionInstances[0].Version = "example/crm", "1.4.0"
	result, err := RenderOwnedTree(ctx, options, renderWorkload(strings.Replace(pinnedDeployment, "aaaa", "bbbb", 1)))
	require.NoError(t, err)
	changed := result.Inventory
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &changed, &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), delivery.Documents[0].Generation)
}

// TestPublishTombstonesWhatThisModuleStoppedDeclaring is the removal rule: a
// binding this module delivered before and no longer declares is withdrawn by
// a tombstone at the next generation, carried forward verbatim on every later
// publish — and a tombstone is terminal: presenting the binding again under
// the same ID is refused at publish, where the rename that fixes it is one
// line away, rather than at the host.
func TestPublishTombstonesWhatThisModuleStoppedDeclaring(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}

	inventory := repository.stageRender(t, "crm", "billing")
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	repository.deliver(t)

	inventory = repository.stageRender(t, "crm")
	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
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
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	for _, document := range delivery.Documents {
		if document.ID == "example.prod.billing" {
			require.Equal(t, uint64(2), document.Generation)
			require.True(t, document.Removed)
		}
	}
	repository.deliver(t)

	// Re-presented under the same ID: refused. The ID is the handle every
	// other system holds, so a later generation would read as continuity of
	// what was withdrawn; a new instance needs a new name.
	inventory = repository.stageRender(t, "crm", "billing")
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.ErrorIs(t, err, solutionhost.ErrTombstoned)
	require.Contains(t, err.Error(), "example.prod.billing")
	require.Contains(t, err.Error(), "a new instance needs a new name")
}

// TestPublishRefusesUnsignedDocumentsOutsideLocalQualification pins the rule
// that a release is signed only by CI: with no signing identity, publish
// refuses and names the documents; only a local qualification publish may
// deliver them unsigned, and then renders no Job to POST them.
// TestPublishRefusesACarrierItsOwnCheckRefuses: the publisher verifies each
// carrier right after signing it, as a host would; one the check refuses is
// refused at publish, with nothing written for a Job to deliver.
func TestPublishRefusesACarrierItsOwnCheckRefuses(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	checked := 0
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm",
		SelfCheck: func(_ context.Context, bundle, payload []byte) error {
			checked++
			if len(bundle) == 0 || len(payload) == 0 {
				return errors.New("empty")
			}
			return errors.New("the certificate names another workflow")
		}}
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageRender(t, "crm"), &opts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "binding example.prod.crm: the certificate names another workflow")
	require.Equal(t, 1, checked)
	_, statErr := os.Stat(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "deliver-presence.yaml"))
	require.True(t, os.IsNotExist(statErr), "a refused carrier is never written as a delivery")
}

func TestPublishRefusesUnsignedDocumentsOutsideLocalQualification(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	unsigned := signing.FromEnvironment(func(string) (string, bool) { return "", false })
	inventory := repository.stageRender(t, "crm")

	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory,
		&deliveryPublishOptions{Signer: unsigned, Target: testDeliveryTarget(), Domain: "example", Module: "crm"})
	require.True(t, errors.Is(err, signing.ErrNoIdentity), "got %v", err)
	require.Contains(t, err.Error(), "example.prod.crm")
	require.Contains(t, err.Error(), "release workflow")

	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory,
		&deliveryPublishOptions{Signer: unsigned, Target: testDeliveryTarget(), Domain: "example", Module: "crm", AllowUnsigned: true})
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
// TestPublishRefusesToWithdrawAnotherDomainsBinding: a withdrawal is authored
// by the domain that delivered the binding. An environment whose domain moved
// renders nothing under the old one, and that absence must not sign the old
// domain's bindings away — the scope is held against what the environment
// declares now, not inferred from the tombstones agreeing with each other.
func TestPublishRefusesToWithdrawAnotherDomainsBinding(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)

	moved := opts
	moved.Domain = "other"
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances = nil
	result, err := RenderOwnedTree(ctx, options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	none := result.Inventory
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &none, &moved)
	require.ErrorIs(t, err, solutionhost.ErrWrongDomain)
	require.Contains(t, err.Error(), `binding example.prod.crm was delivered under domain "example" and this environment declares "other"`)
}

// TestRollbackResettlesWhatItRestores: a rollback restores the workloads an
// earlier revision delivered and settles their documents anew — the old
// content at the next generation, signed now, with the base branch's
// tombstones carried forward — never the carriers that revision signed, which
// a host past them refuses as stale. A rollback across a withdrawal is refused
// as any render presenting a withdrawn binding is.
func TestRollbackResettlesWhatItRestores(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	publication := &deliveryPublication{baseBranch: "main", options: opts}

	// Generation 1 delivered, and its tree kept as a rollback target.
	inventory := repository.stageRender(t, "crm")
	delivery, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation)
	kept := t.TempDir()
	require.NoError(t, copyTree(repository.target, kept))
	repository.deliver(t)

	// Generation 2: the release changed.
	inventory = repository.stageRender(t, "crm")
	bindingFile := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
	data, err := os.ReadFile(bindingFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bindingFile, []byte(strings.Replace(string(data), string(testReleaseDigest), string(otherReleaseDigest), 1)), 0o644))
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm").Generation)
	repository.deliver(t)

	// Rolling back to the generation-1 tree: its content comes back as
	// generation 3, signed by this publish, not as the generation-1 carrier.
	require.NoError(t, os.RemoveAll(repository.target))
	require.NoError(t, copyTree(kept, repository.target))
	signed := signer.signed
	// The rollback path hands the restored tree to the settlement as a
	// render's output: staged over the target and settled against the base.
	restored, err := LoadInventory(repository.target)
	require.NoError(t, err)
	resettled, err := stageAndSettleDelivery(ctx, repository.repo, repository.target, repository.targetPath, kept, &restored, "prod", publication)
	require.NoError(t, err)
	require.Equal(t, uint64(3), documentByID(resettled, "example.prod.crm").Generation)
	require.False(t, documentByID(resettled, "example.prod.crm").Removed)
	require.Greater(t, signer.signed, signed, "the rollback signs what it delivers")
	document := deliveredBinding(t, repository.target, "example.prod.crm")
	require.Equal(t, uint64(3), document.Generation)
	require.Equal(t, testReleaseDigest, document.Release.Digest, "the restored content is what generation 1 declared")
	repository.deliver(t)

	// Withdrawn at generation 4; a rollback to the generation-1 tree would
	// reinstate the binding, and is refused as terminal.
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances = nil
	result, err := RenderOwnedTree(ctx, options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	none := result.Inventory
	delivery, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &none, &opts)
	require.NoError(t, err)
	require.True(t, documentByID(delivery, "example.prod.crm").Removed)
	repository.deliver(t)
	require.NoError(t, os.RemoveAll(repository.target))
	require.NoError(t, copyTree(kept, repository.target))
	restored, err = LoadInventory(repository.target)
	require.NoError(t, err)
	_, err = stageAndSettleDelivery(ctx, repository.repo, repository.target, repository.targetPath, kept, &restored, "prod", publication)
	require.ErrorIs(t, err, solutionhost.ErrTombstoned)
}

// TestPublishMergesItsCellContributionIntoTheDeliveredCell: the local cell is
// built from whatever module trees are on disk, so a publish takes only its
// own module's entry from it and sets that into the cell the base branch
// delivers, every other module's entry kept. One module's publish never
// erases another from the platform's inventory, and a module rendered
// locally but not published is not added on another's account.
func TestPublishMergesItsCellContributionIntoTheDeliveredCell(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	cellPath := "deployments/cells/prod/cell.yaml"
	namespace := func(module, digest string) CellNamespace {
		return CellNamespace{Name: "ns-" + module, Module: module, Workloads: []CellWorkload{{
			Name: module, Kind: "Deployment", Service: module + "/api", ServiceAccount: "api",
			Containers: []CellContainer{{Name: "api", Image: CellImage{Repository: "registry.example.test/" + module, Digest: "sha256:" + strings.Repeat(digest, 64)}}},
			Artifact:   CellArtifact{Name: "api", Digest: "sha256:" + strings.Repeat(digest, 64)},
		}}}
	}
	local := &CellFile{Schema: CellSchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example", Environment: "prod",
		Namespaces: []CellNamespace{namespace("crm", "b"), namespace("shop", "c")}}

	// No cell delivered yet: the publish contributes crm alone. shop, rendered
	// locally but not the module being published, is not added.
	merged, err := mergeCellContribution(ctx, repository.repo, "main", cellPath, local, "crm", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"crm"}, cellModules(merged))
	require.Equal(t, "example", merged.Domain)

	// billing and an older crm delivered: crm is replaced, billing kept, shop
	// still not added.
	delivered := &CellFile{Schema: CellSchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example", Environment: "prod",
		Namespaces: []CellNamespace{namespace("billing", "d"), namespace("crm", "a")}}
	data, err := yaml.Marshal(delivered)
	require.NoError(t, err)
	full := filepath.Join(repository.repo, filepath.FromSlash(cellPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, data, 0o644))
	for _, args := range [][]string{{"add", "-A", "--", cellPath}, {"commit", "-q", "-m", "cell"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	merged, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, local, "crm", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"billing", "crm"}, cellModules(merged))
	require.Equal(t, "sha256:"+strings.Repeat("b", 64), merged.Namespaces[1].Workloads[0].Artifact.Digest, "crm is this publish's render")
	require.Equal(t, "sha256:"+strings.Repeat("d", 64), merged.Namespaces[0].Workloads[0].Artifact.Digest, "billing is as delivered")

	// A publish of a module the local cell has no entry for is refused: its
	// tree was not rendered for this environment.
	_, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, local, "ledger", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "carries no entry for module ledger")
}

func cellModules(cell *CellFile) []string {
	modules := make([]string, 0, len(cell.Namespaces))
	for _, namespace := range cell.Namespaces {
		modules = append(modules, namespace.Module)
	}
	return modules
}

func TestPublishRefusesToWithdrawEverythingWhenTheHostDeclarationIsGone(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	inventory := repository.stageRender(t, "crm")
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	repository.deliver(t)

	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.Host, options.SolutionInstances = nil, nil
	result, err := RenderOwnedTree(ctx, options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	hostless := result.Inventory
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", &hostless,
		&deliveryPublishOptions{Signer: &fakeSigner{}, Module: "crm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "example.prod.crm")
	require.Contains(t, err.Error(), "tombstones")
}

// stageAuthorityRender renders a module tree declaring one module instance and
// one authority document straight into the repository's target, with the
// contract's bindings as given.
func (repository *deliveryRepository) stageAuthorityRender(t *testing.T, bindings []modulecontract.ResolvedBinding) *Inventory {
	t.Helper()
	require.NoError(t, os.RemoveAll(repository.target))
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "crm@example.iam.test"}}
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances = []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "crm", Package: "example/crm", Version: "1.4.0", ReleaseDigest: testReleaseDigest, Units: units,
	}}
	options.AuthorityInstances = []AuthorityInstance{{
		Module: "crm", Service: "api", Unit: units[0],
		Contract: &modulecontract.Resolved{Principal: "crm", Namespaces: []string{"crm"}, Queues: []string{}, Bindings: bindings},
	}}
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	inventory := result.Inventory
	return &inventory
}

// stageAuthorityRenderPresentedBy renders the module with two services, api
// and worker, each running its own build, and the named one presenting the
// module's authority — or no authority at all when no service is named.
func (repository *deliveryRepository) stageAuthorityRenderPresentedBy(t *testing.T, service string, bindings []modulecontract.ResolvedBinding) *Inventory {
	t.Helper()
	require.NoError(t, os.RemoveAll(repository.target))
	units := []SolutionArtifactUnit{
		{Name: "api", Path: "services/api", Subject: "crm@example.iam.test"},
		{Name: "worker", Path: "services/worker", Subject: "crm@example.iam.test"},
	}
	options := solutionRenderOptions(repository.target)
	options.Units = promotableServiceGraph("crm", []string{"api", "worker"})
	options.SolutionInstances = []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "crm", Package: "example/crm", Version: "1.4.0", ReleaseDigest: testReleaseDigest, Units: units,
	}}
	options.AuthorityInstances = nil
	for _, unit := range units {
		if unit.Name == service {
			options.AuthorityInstances = []AuthorityInstance{{
				Module: "crm", Service: service, Unit: unit,
				Contract: &modulecontract.Resolved{Principal: "crm", Namespaces: []string{"crm"}, Queues: []string{}, Bindings: bindings},
			}}
		}
	}
	result, err := RenderOwnedTree(context.Background(), options, renderApiAndWorker)
	require.NoError(t, err)
	inventory := result.Inventory
	return &inventory
}

// renderApiAndWorker renders a pinned Deployment for the api and the worker
// services, each on its own build.
func renderApiAndWorker(_ context.Context, root string) error {
	for name, body := range map[string]string{
		"api":    pinnedDeployment,
		"worker": strings.NewReplacer("name: api", "name: worker", "codefly-dev/api@sha256:"+strings.Repeat("a", 64), "codefly-dev/worker@sha256:"+strings.Repeat("b", 64)).Replace(pinnedDeployment),
	} {
		overlay := filepath.Join(root, "services", name, "overlays", "prod")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func modelBinding(revision uint64, scopes ...string) modulecontract.ResolvedBinding {
	return modulecontract.ResolvedBinding{
		ID: "model", Revision: revision, Operations: []string{"invoke"}, Audience: "model-gateway",
		Scopes: map[string][]string{"invoke": scopes},
	}
}

func settleBoth(t *testing.T, repository *deliveryRepository, inventory *Inventory, opts *deliveryPublishOptions) *InventoryDelivery {
	t.Helper()
	ctx := context.Background()
	presence, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
	require.NoError(t, err)
	authority, err := settleAuthorityDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, presence, opts)
	require.NoError(t, err)
	return mergeDeliveries(presence, authority)
}

func documentByID(delivery *InventoryDelivery, id string) InventoryDeliveredDocument {
	for _, document := range delivery.Documents {
		if document.ID == id {
			return document
		}
	}
	return InventoryDeliveredDocument{}
}

// TestPublishSettlesAuthorityWithThePresenceItIsEffectiveFrom: an authority
// document is effective from the presence generation settled in the same
// publish, follows its own generation rule, and is withdrawn by a tombstone.
func TestPublishSettlesAuthorityWithThePresenceItIsEffectiveFrom(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", EnvelopeRevision: 1, Module: "crm"}

	delivery := settleBoth(t, repository, repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")}), &opts)
	require.True(t, delivery.Signed)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm-authority").Generation)
	directory := filepath.Join(repository.target, filepath.FromSlash(solutionAuthorityOverlay("prod")))
	document, carrier := readDeliveredAuthority(t, repository.target, "prod", "example.prod.crm-authority.yaml")
	require.Equal(t, uint64(1), document.EffectiveFrom)
	require.Contains(t, carrier.Data, authorityCarrierKey)
	signed, err := solutionhost.ParseSigned([]byte(carrier.Data[authorityCarrierKey]))
	require.NoError(t, err)
	parsed, err := solutionhost.AuthorityFromVerified(signed.Document)
	require.NoError(t, err)
	require.Equal(t, document.Authority, parsed.Authority)
	kustomization, err := os.ReadFile(filepath.Join(directory, kustomizationFile))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "deliver-authority.yaml")
	require.NotContains(t, string(kustomization), "delivery-service-account.yaml", "the authority namespace's account is the platform's")
	job, err := os.ReadFile(filepath.Join(directory, "deliver-authority.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(job), "namespace: "+authorityNamespace)
	require.Contains(t, string(job), authorityDeliveryPath)
	repository.deliver(t)

	// The presence changes (a new release digest): its generation moves, and
	// the authority — whose content is otherwise unchanged — follows, because
	// it is effective from the new presence generation.
	require.NoError(t, os.RemoveAll(repository.target))
	inventory := repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")})
	bindingFile := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
	data, err := os.ReadFile(bindingFile)
	require.NoError(t, err)
	require.Contains(t, string(data), string(testReleaseDigest))
	require.NoError(t, os.WriteFile(bindingFile, []byte(strings.Replace(string(data), string(testReleaseDigest), string(otherReleaseDigest), 1)), 0o644))
	delivery = settleBoth(t, repository, inventory, &opts)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm").Generation)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm-authority").Generation)
	document, _ = readDeliveredAuthority(t, repository.target, "prod", "example.prod.crm-authority.yaml")
	require.Equal(t, uint64(2), document.EffectiveFrom)
	repository.deliver(t)

	// The module stops publishing authority: a tombstone withdraws it.
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	options.SolutionInstances = []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "crm", Package: "example/crm", Version: "1.4.0", ReleaseDigest: testReleaseDigest,
		Units: []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "crm@example.iam.test"}},
	}}
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	withoutAuthority := result.Inventory
	delivery = settleBoth(t, repository, &withoutAuthority, &opts)
	tombstone := documentByID(delivery, "example.prod.crm-authority")
	require.True(t, tombstone.Removed)
	require.Equal(t, uint64(3), tombstone.Generation)
	require.Equal(t, solutionAuthorityDir, withoutAuthority.SolutionAuthorityPath, "the tombstone is delivered through the authority overlay")
	repository.deliver(t)

	// Granted again under the same authority ID: refused, as for presence. A
	// withdrawn authority is terminal; the instance is renamed.
	regranted := repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")})
	ctx := context.Background()
	presence, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", regranted, &opts)
	require.NoError(t, err, "the presence binding itself was never withdrawn")
	_, err = settleAuthorityDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", regranted, presence, &opts)
	require.ErrorIs(t, err, solutionhost.ErrTombstoned)
	require.Contains(t, err.Error(), "example.prod.crm-authority")
}

// TestPublishRefusesABindingChangeThatKeepsItsRevision: a credential seals the
// binding revision and is refused when the live one moves, so a contract that
// changes what a binding grants without bumping the revision is refused at
// publish rather than left for no credential to detect.
func TestPublishRefusesABindingChangeThatKeepsItsRevision(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", EnvelopeRevision: 1, Module: "crm"}
	settleBoth(t, repository, repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")}), &opts)
	repository.deliver(t)

	ctx := context.Background()
	widened := repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke", "modelservice.profiles:read")})
	presence, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", widened, &opts)
	require.NoError(t, err)
	_, err = settleAuthorityDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", widened, presence, &opts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "crm:model:invoke")
	require.Contains(t, err.Error(), "without bumping their revision")

	// The same change with the revision bumped is a new generation.
	bumped := repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(2, "modelservice.profiles:invoke", "modelservice.profiles:read")})
	delivery := settleBoth(t, repository, bumped, &opts)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm-authority").Generation)
	repository.deliver(t)

	// A revision never moves backwards, whatever the content: an old number
	// must not acquire a new meaning under a credential sealed to it.
	rewound := repository.stageAuthorityRender(t, []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")})
	presence, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", rewound, &opts)
	require.NoError(t, err)
	_, err = settleAuthorityDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", rewound, presence, &opts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "moves the revision of the bindings example.prod.crm:model:invoke (1, delivered at 2) backwards")
}

// TestPublishRefusesAPairTheHostWouldNotActivate: before an authority document
// is signed, publish holds it against the presence document it is granted over
// with core's own activation rules — so a pair the host would refuse at
// activation is refused here, with the host's reason, and never written.
func TestPublishRefusesAPairTheHostWouldNotActivate(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", EnvelopeRevision: 1, Module: "crm"}
	bindings := []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")}
	settleAuthority := func(inventory *Inventory, opts *deliveryPublishOptions) error {
		t.Helper()
		presence, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
		require.NoError(t, err)
		_, err = settleAuthorityDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, presence, opts)
		return err
	}
	authorityFile := filepath.Join(repository.target, filepath.FromSlash(solutionAuthorityOverlay("prod")), "example.prod.crm-authority.yaml")
	rewrite := func(file, from, to string) {
		t.Helper()
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Contains(t, string(data), from)
		require.NoError(t, os.WriteFile(file, []byte(strings.Replace(string(data), from, to, 1)), 0o644))
	}

	// The composition's host block was re-reviewed since the render: both
	// halves name revision 1 and the environment declares 2 now. The halves
	// agree with each other, which is not enough.
	reviewed := opts
	reviewed.EnvelopeRevision = 2
	err := settleAuthority(repository.stageAuthorityRender(t, bindings), &reviewed)
	require.ErrorIs(t, err, solutionhost.ErrNotActivated)
	require.Contains(t, err.Error(), "envelope revision 1 and this is revision 2")

	// The host block is gone from the composition: the presence half is
	// refused for it before the authority half is reached.
	hostless := opts
	hostless.Target, hostless.EnvelopeRevision = nil, 0
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageAuthorityRender(t, bindings), &hostless)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no host now")
	require.Contains(t, err.Error(), "names no host now")

	// The authority approves a build the presence does not say the binding
	// runs: sound on its own, and activates nothing.
	inventory := repository.stageAuthorityRender(t, bindings)
	rewrite(authorityFile, "approved_build: sha256:aaaa", "approved_build: sha256:bbbb")
	err = settleAuthority(inventory, &opts)
	require.ErrorIs(t, err, solutionhost.ErrNotActivated)
	require.Contains(t, err.Error(), "example.prod.crm-authority and binding example.prod.crm")
	require.Contains(t, err.Error(), "runs builds")
	_, statErr := os.Stat(filepath.Join(repository.target, filepath.FromSlash(solutionAuthorityOverlay("prod")), "deliver-authority.yaml"))
	require.True(t, os.IsNotExist(statErr), "a refused pair is never written as a delivery")

	// Delivered once under one domain, the authority does not migrate to
	// another: the fold runs against what the base branch delivered. (The
	// presence half moving domains is refused earlier, by its own settlement.)
	require.NoError(t, settleAuthority(repository.stageAuthorityRender(t, bindings), &opts))
	repository.deliver(t)
	// The environment moved domains and the render followed, both halves:
	// the delivered records were applied under the old one, which core's fold
	// refuses at the presence half before the authority half is reached. A
	// half moved on its own never reaches the fold — publish holds every
	// document to the domain declared now first.
	inventory = repository.stageAuthorityRender(t, bindings)
	rewrite(authorityFile, "ownership_domain: example", "ownership_domain: other")
	rewrite(filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml"), "ownership_domain: example", "ownership_domain: other")
	moved := opts
	moved.Domain = "other"
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &moved)
	require.ErrorIs(t, err, solutionhost.ErrWrongDomain)
	require.Contains(t, err.Error(), `applied under domain "example"`)
}

// TestPublishRefusesABindingThatMovesBetweenDomains: the delivered domain is
// what says who may change a binding, so a generation arriving under another
// one is refused at publish with the host's own reason — for a module with no
// contract too, whose presence is folded by publish alone.
func TestPublishRefusesABindingThatMovesBetweenDomains(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)

	inventory := repository.stageRender(t, "crm")
	file := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")), "example.prod.crm.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Contains(t, string(data), "ownership_domain: example")
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(string(data), "ownership_domain: example", "ownership_domain: other", 1)), 0o644))
	// The environment moved domains with the render; the delivered record
	// was applied under the old one.
	moved := opts
	moved.Domain = "other"
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &moved)
	require.ErrorIs(t, err, solutionhost.ErrWrongDomain)
	require.Contains(t, err.Error(), `binding "example.prod.crm" was applied under domain "example"`)
}

// TestRenderRefusesTwoAuthorityInstances: the instances are derived one per
// module, and a caller handing the render two would get two documents under one
// ID, the second overwriting the first — refused by name instead.
func TestRenderRefusesTwoAuthorityInstances(t *testing.T) {
	destination := t.TempDir()
	options := solutionRenderOptions(destination)
	options.Units = promotableServiceGraph("crm", []string{"api", "worker"})
	units := []SolutionArtifactUnit{
		{Name: "api", Path: "services/api", Subject: "crm@example.iam.test"},
		{Name: "worker", Path: "services/worker", Subject: "crm@example.iam.test"},
	}
	options.SolutionInstances = []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "crm", Package: "example/crm", Version: "1.4.0", ReleaseDigest: testReleaseDigest, Units: units,
	}}
	contract := &modulecontract.Resolved{Principal: "crm", Namespaces: []string{"crm"}, Queues: []string{}, Bindings: []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")}}
	options.AuthorityInstances = []AuthorityInstance{
		{Module: "crm", Service: "api", Unit: units[0], Contract: contract},
		{Module: "crm", Service: "worker", Unit: units[1], Contract: contract},
	}
	_, err := RenderOwnedTree(context.Background(), options, renderApiAndWorker)
	require.Error(t, err)
	require.Contains(t, err.Error(), "crm/api, crm/worker")
	require.Contains(t, err.Error(), "one authority document from one service")
}

// TestPublishMovesTheAuthorityWithTheServicePresentingIt: the authority is
// named after the binding, not the service, so a module moving its identity
// from one service to another delivers a new generation approving the new
// build — not a withdrawal, which the host holds terminal for the binding, plus
// a grant it would therefore never activate.
func TestPublishMovesTheAuthorityWithTheServicePresentingIt(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", EnvelopeRevision: 1, Module: "crm"}
	bindings := []modulecontract.ResolvedBinding{modelBinding(1, "modelservice.profiles:invoke")}

	// Presence delivered first, on its own: the authority's first generation
	// then folds against a presence record and no authority record, each half
	// stated for what it is.
	_, err := settlePresenceDelivery(context.Background(), repository.repo, "main", repository.target, repository.targetPath, "prod", repository.stageAuthorityRenderPresentedBy(t, "", nil), &opts)
	require.NoError(t, err)
	repository.deliver(t)

	delivery := settleBoth(t, repository, repository.stageAuthorityRenderPresentedBy(t, "api", bindings), &opts)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation, "the presence is unchanged")
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm-authority").Generation)
	document, _ := readDeliveredAuthority(t, repository.target, "prod", "example.prod.crm-authority.yaml")
	require.Equal(t, solutionhost.ImageDigest("sha256:"+strings.Repeat("a", 64)), document.ApprovedBuild)
	repository.deliver(t)

	delivery = settleBoth(t, repository, repository.stageAuthorityRenderPresentedBy(t, "worker", bindings), &opts)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm-authority").Generation)
	require.False(t, documentByID(delivery, "example.prod.crm-authority").Removed)
	require.Len(t, delivery.Documents, 2, "one presence document and one authority document, nothing withdrawn")
	document, _ = readDeliveredAuthority(t, repository.target, "prod", "example.prod.crm-authority.yaml")
	require.Equal(t, solutionhost.ImageDigest("sha256:"+strings.Repeat("b", 64)), document.ApprovedBuild, "the worker's build is the approved one now")
}

// TestPublishHoldsEveryDocumentToTheHostDeclaredNow: a render made under one
// host declaration is refused by a publish under another — the ownership
// domain, or the host coordinate and component — and a render that declares
// documents is refused outright when the environment names no host now, so
// nothing is signed for no Job to deliver.
func TestPublishHoldsEveryDocumentToTheHostDeclaredNow(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	inventory := repository.stageRender(t, "crm")
	settle := func(opts *deliveryPublishOptions) error {
		_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
		return err
	}
	err := settle(&deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "other", Module: "crm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `ownership domain "example"`)
	err = settle(&deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm", Coordinate: "elsewhere/prod/x", Component: "host"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "elsewhere/prod/x")
	err = settle(&deliveryPublishOptions{Signer: &fakeSigner{}, Domain: "example", Module: "crm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no host now")
}

// TestPublishRefusesAReusedCarrierThatSignsOtherBytes: a carrier delivered
// before is reused only once it is held to the document it would carry. Here
// the delivered carrier signs other bytes than the document beside it, and a
// publish that changes nothing is refused rather than republishing it as
// signed.
func TestPublishRefusesAReusedCarrierThatSignsOtherBytes(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := deliveryPublishOptions{Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
	inventory := repository.stageRender(t, "crm")
	_, err := settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.NoError(t, err)
	repository.deliver(t)

	// The delivered carrier's signed bytes are altered in place; the readable
	// document beside it is untouched, so only the carrier disagrees.
	overlay := filepath.Join(repository.target, filepath.FromSlash(solutionHostBindingOverlay("prod")))
	entries, err := os.ReadDir(overlay)
	require.NoError(t, err)
	tampered := 0
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == kustomizationFile || !strings.HasSuffix(entry.Name(), yamlExtension) {
			continue
		}
		path := filepath.Join(overlay, entry.Name())
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		// The carrier is JSON inside a YAML scalar, quoted one way or the
		// other by the encoder; the readable document beside it spells the
		// generation as YAML, so neither pattern touches it.
		altered := bytes.Replace(data, []byte(`"generation":1`), []byte(`"generation":7`), 1)
		if bytes.Equal(altered, data) {
			altered = bytes.Replace(data, []byte(`\"generation\":1`), []byte(`\"generation\":7`), 1)
		}
		if bytes.Equal(altered, data) {
			continue
		}
		require.NoError(t, os.WriteFile(path, altered, 0o600))
		tampered++
	}
	require.Equal(t, 1, tampered, "the delivered carrier was not found to alter")
	for _, args := range [][]string{{"add", "-A", "--", repository.targetPath}, {"commit", "-q", "-m", "tamper"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, string(out))
	}

	inventory = repository.stageRender(t, "crm")
	_, err = settlePresenceDelivery(ctx, repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, &opts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "signs other bytes")
}

// TestCellMergeReconcilesEdgesAndRefusesAnotherHostsCell: the publishing
// module's consumer edges in other modules' delivered entries follow its
// render — dropped where it no longer consumes, added where it does now —
// and a contribution made under another host declaration is refused rather
// than relabelling entries it does not own.
func TestCellMergeReconcilesEdgesAndRefusesAnotherHostsCell(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	cellPath := "deployments/cells/prod/cell.yaml"
	provider := CellNamespace{Name: "ns-billing", Module: "billing", Workloads: []CellWorkload{{
		Name: "billing", Kind: "Deployment", Service: "billing/api", ServiceAccount: "api",
		Endpoints: []CellEndpoint{{Name: "grpc", Consumers: []string{"crm/api", "crm/worker", "shop/api"}}},
	}}}
	delivered := &CellFile{Schema: CellSchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example",
		Namespaces: []CellNamespace{provider, {Name: "ns-crm", Module: "crm"}}}
	data, err := yaml.Marshal(delivered)
	require.NoError(t, err)
	full := filepath.Join(repository.repo, filepath.FromSlash(cellPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, data, 0o600))
	for _, args := range [][]string{{"add", "-A", "--", cellPath}, {"commit", "-q", "-m", "cell"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, string(out))
	}
	local := &CellFile{Schema: CellSchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example",
		Namespaces: []CellNamespace{{Name: "ns-crm", Module: "crm"}}}
	merged, err := mergeCellContribution(ctx, repository.repo, "main", cellPath, local, "crm", []consumedEndpoint{{Provider: "billing/api", Endpoint: "grpc", Consumer: "crm/api"}})
	require.NoError(t, err)
	var billing *CellNamespace
	for i := range merged.Namespaces {
		if merged.Namespaces[i].Module == "billing" {
			billing = &merged.Namespaces[i]
		}
	}
	require.NotNil(t, billing)
	require.Equal(t, []string{"crm/api", "shop/api"}, billing.Workloads[0].Endpoints[0].Consumers)

	moved := *local
	moved.Domain = "other"
	_, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, &moved, "crm", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), `domain "example"`)
}
