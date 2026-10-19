package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/run/fixture"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// --- Delivering the module envelope at publish ---
//
// The module envelope is what a host grants each module principal, as the
// composition states it in configurations/<environment>/module-envelope.json.
// It is delivered the way a module manifest is: settled against the base
// branch, signed by the release custody, held to the host's own verifier, and
// POSTed by a delivery Job — in the overlay of the module that hosts the
// delivery API, since the envelope is that host's record and nobody else's.
//
//   - The REVISION is settled like a generation: the same canonical content
//     keeps the delivered revision and its carrier, other content is the next
//     one, and the first envelope is revision 1 — never below the
//     composition's minimum_revision, which lifts it (and re-signs) to step
//     past a revision the host's administrator wrote interactively.
//   - A composition that delivered an envelope and now states none is
//     refused, never read as "nothing to say": revoking every grant is the
//     statement {"principals": {}}.
//   - A delivered grant names no organization. Tenancy resolves through each
//     organization's installation; pinning a module to one organization is an
//     administrator's interactive write on the host.
//
// A local qualification publish delivers no envelope: a host admits one only
// from a release key, and a local host pins developer keys. A native run
// writes it as the fixture's administrator instead.

const (
	// deliveredEnvelopeKind is the envelope's kind in the inventory's delivery
	// record, in a carrier's reuse key, and its delivery Job's kind.
	deliveredEnvelopeKind = "envelope"
	// deliveredEnvelopeID is the envelope's ID in the delivery record: an
	// environment's host holds one.
	deliveredEnvelopeID = "module-envelope"
	// envelopeDeliveryPath is the host's envelope delivery route.
	envelopeDeliveryPath = "/platform/_delivery/envelope"
	// moduleEnvelopeConfigMap names the envelope's ConfigMap, its documentLabel
	// value, and (with .yaml) its file in the overlay; moduleEnvelopeDocument
	// is the key of the readable canonical document beside the carrier.
	moduleEnvelopeConfigMap = "module-envelope"
	moduleEnvelopeFile      = moduleEnvelopeConfigMap + ".yaml"
	moduleEnvelopeDocument  = "module-envelope.json"
)

// declaredEnvelope is the module envelope a composition states for one
// environment. Stated is false when no workspace of the composition writes
// the file at all, which is not the same statement as an empty principals.
type declaredEnvelope struct {
	Principals map[string]json.RawMessage
	Stated     bool
	// StatesPrincipals is whether a workspace states a "principals" object,
	// which the file existing does not say: a file written for the floor
	// alone states no grants, and its empty merge must not be signed as a
	// revocation of every grant on the cell.
	StatesPrincipals bool
	// MinimumRevision is the lowest revision the delivered envelope may
	// carry (minimum_revision), 0 for none: how a composition steps past a
	// revision the host's administrator wrote interactively.
	MinimumRevision uint64
}

// declareEnvelope reads the composition's envelope into the publication's
// options when this publish delivers it: the module hosting the delivery
// API, to an environment that declares a host, outside local qualification.
func (p *deliveryPublication) declareEnvelope(inventory *Inventory, environment string) error {
	p.options.Envelope = nil
	if !inventory.HostsDelivery || p.options.Target == nil || p.local || p.workspace == nil {
		return nil
	}
	principals, err := fixture.ReadCompositionGrants(p.workspace, environment)
	if err != nil {
		return fmt.Errorf("read the module envelope the composition states for %s: %w", environment, err)
	}
	stated, err := fixture.StatesGrants(p.workspace, environment)
	if err != nil {
		return err
	}
	statesPrincipals, err := fixture.StatesPrincipals(p.workspace, environment)
	if err != nil {
		return err
	}
	floor, err := fixture.ReadCompositionMinimumRevision(p.workspace, environment)
	if err != nil {
		return err
	}
	p.options.Envelope = &declaredEnvelope{Principals: principals, Stated: stated, StatesPrincipals: statesPrincipals, MinimumRevision: floor}
	return nil
}

// settleEnvelope settles, signs and writes the envelope beside the manifests
// already settled, and returns the delivery record and file list extended by
// it. Nothing to deliver and nothing delivered before leaves both unchanged.
func settleEnvelope(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	delivery *InventoryDelivery,
	files []string,
	opts *deliveryPublishOptions,
) (*InventoryDelivery, []string, error) {
	overlay := moduleManifestOverlay(environment)
	previous, carrier, err := priorDeliveredEnvelope(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, overlay)))
	if err != nil {
		return nil, nil, err
	}
	declared := opts.Envelope
	switch {
	case declared == nil && previous == nil, declared != nil && !declared.Stated && previous == nil:
		return delivery, files, nil
	case declared == nil:
		return nil, nil, fmt.Errorf("module %s delivered the module envelope (revision %d) to %s and this publish delivers none — the environment names no host, or the module no longer hosts the delivery API; publish the envelope from the module that hosts it, against the host it was delivered to",
			inventory.Module, previous.Revision, environment)
	case !declared.Stated:
		return nil, nil, fmt.Errorf("module %s delivered the module envelope (revision %d) to %s and the composition states none now; to revoke every grant, state {\"principals\": {}} in configurations/%s/module-envelope.json",
			inventory.Module, previous.Revision, environment, environment)
	}
	// Revoking every grant is a statement, and only an authored {} makes it.
	// A file that states no principals object — written for the floor alone,
	// or with the key dropped or misspelled — would otherwise be signed as
	// that revocation and stop every module principal on the cell minting.
	if !declared.StatesPrincipals || declared.Principals == nil {
		return nil, nil, fmt.Errorf("the module envelope configurations/%s/module-envelope.json of %s states no principals, and stating nothing is not revoking: state {\"principals\": {...}}, or {\"principals\": {}} to revoke every grant",
			environment, inventory.Module)
	}
	if err = refuseTenantGrants(declared.Principals, environment); err != nil {
		return nil, nil, err
	}
	if slices.Contains(files, moduleEnvelopeFile) {
		return nil, nil, fmt.Errorf("a module manifest of %s is delivered as %s, the module envelope's file; rename its instance", inventory.Module, moduleEnvelopeFile)
	}
	candidate := &solutionhost.ModuleEnvelope{
		Schema: solutionhost.SchemaModuleEnvelopeV1, Revision: 1, Principals: declared.Principals,
		Host: solutionhost.HostTarget{Coordinate: opts.Coordinate, Domain: opts.Domain},
	}
	carrier, err = settleEnvelopeRevision(previous, candidate, carrier)
	if err != nil {
		return nil, nil, err
	}
	if candidate.Revision < declared.MinimumRevision {
		// The floor lifts the revision, and a new revision is a new signature.
		candidate.Revision, carrier = declared.MinimumRevision, nil
	}
	if _, err = solutionhost.AdmitEnvelope(candidate.Host.Coordinate, previous, candidate); err != nil {
		return nil, nil, fmt.Errorf("the module envelope is not admissible: %w", err)
	}
	payload, err := candidate.CanonicalBytes()
	if err != nil {
		return nil, nil, err
	}
	signer := resolvedSigner(opts)
	key := carrierKey(deliveredEnvelopeKind, deliveredEnvelopeID, candidate.Revision)
	identity, unsigned, err := signCarrier(ctx, "the module envelope", payload, &carrier, opts.Reuse[key], signer, opts)
	if err != nil {
		return nil, nil, err
	}
	if unsigned != nil && !opts.AllowUnsigned {
		return nil, nil, unsignedRefusal("the module envelope", environment, unsigned, opts)
	}
	digest, err := candidate.Digest()
	if err != nil {
		return nil, nil, err
	}
	written, err := writeEnvelope(target, overlay, inventory, payload, carrier, slices.Contains(files, deliveryServiceAccountFile), opts)
	if err != nil {
		return nil, nil, err
	}
	if opts.Carriers == nil {
		opts.Carriers = map[string][]byte{}
	}
	if len(carrier) > 0 {
		opts.Carriers[key] = carrier
	}
	if delivery == nil {
		delivery = &InventoryDelivery{Signed: true}
	}
	delivery.Signed = delivery.Signed && unsigned == nil
	if delivery.Identity == "" {
		delivery.Identity = identity
	}
	delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
		Kind: deliveredEnvelopeKind, ID: deliveredEnvelopeID, Generation: candidate.Revision, Digest: digest,
	})
	return delivery, append(files, written...), nil
}

// settleEnvelopeRevision sets the candidate's revision against the envelope
// the base branch delivered: the same canonical content keeps its revision and
// the carrier it was delivered as, other content is the next revision. Only
// the number is chosen here; whether it may carry it is AdmitEnvelope's.
func settleEnvelopeRevision(previous, candidate *solutionhost.ModuleEnvelope, carrier []byte) ([]byte, error) {
	if previous == nil {
		return nil, nil
	}
	candidate.Revision = previous.Revision
	current, err := candidate.Digest()
	if err != nil {
		return nil, err
	}
	delivered, err := previous.Digest()
	if err != nil {
		return nil, fmt.Errorf("digest the delivered module envelope: %w", err)
	}
	if current == delivered {
		return carrier, nil
	}
	candidate.Revision = previous.Revision + 1
	return nil, nil
}

// refuseTenantGrants refuses a grant that names an organization: a delivered
// envelope speaks for every organization the module is installed in, and the
// host refuses the same grant — failing here saves a red delivery Job.
func refuseTenantGrants(principals map[string]json.RawMessage, environment string) error {
	var named []string
	for principal, grant := range principals {
		var fields map[string]any
		if err := json.Unmarshal(grant, &fields); err != nil {
			return fmt.Errorf("the grant of module principal %q in configurations/%s/module-envelope.json is not a JSON object: %w", principal, environment, err)
		}
		if tenant, ok := fields["tenant"]; ok && tenant != nil && tenant != "" {
			named = append(named, principal)
		}
	}
	if len(named) == 0 {
		return nil
	}
	sort.Strings(named)
	return fmt.Errorf("the module envelope of %s grants %s with a tenant; a delivered module envelope names no organization: tenancy resolves through each organization's installation, and pinning a module to one organization is an administrator's interactive write — remove \"tenant\" from configurations/%s/module-envelope.json",
		environment, strings.Join(named, ", "), environment)
}

// writeEnvelope writes the envelope's ConfigMap — the canonical document and,
// once signed, its carrier — and, toward a host, the delivery account (unless
// the manifests wrote it) and the Job that POSTs the carrier. It returns the
// files written.
func writeEnvelope(target, overlay string, inventory *Inventory, payload, carrier []byte, accountWritten bool, opts *deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create delivery overlay: %w", err)
	}
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	configMap := moduleManifestConfigMap{
		APIVersion: "v1", Kind: kindConfigMap,
		Metadata: moduleManifestConfigMapMeta{
			Name: moduleEnvelopeConfigMap, Namespace: inventory.Namespace,
			Labels: map[string]string{managedByLabel: managedByCodefly, documentLabel: moduleEnvelopeConfigMap},
		},
		Data: map[string]string{moduleEnvelopeDocument: string(payload)},
	}
	if len(carrier) > 0 {
		configMap.Data[solutionhost.EnvelopeSignedFileName] = string(carrier)
	}
	body, err := yaml.Marshal(configMap)
	if err != nil {
		return nil, fmt.Errorf("encode the module envelope: %w", err)
	}
	if err = tree.write(directory, moduleEnvelopeFile, body); err != nil {
		return nil, fmt.Errorf("write the module envelope: %w", err)
	}
	files := []string{moduleEnvelopeFile}
	if len(carrier) == 0 {
		return files, nil
	}
	if !accountWritten {
		account, accountErr := renderDeliveryServiceAccountInTree(tree, directory, inventory.Namespace)
		if accountErr != nil {
			return nil, accountErr
		}
		files = append(files, account)
	}
	job, err := renderDeliveryJob(directory, deliveryJobName(deliveredEnvelopeKind, inventory.Module, inventory.Environment, [][]byte{carrier}), inventory.Namespace,
		deliveredEnvelopeKind, envelopeDeliveryPath, opts.Target,
		[]deliveryDocument{{ConfigMap: moduleEnvelopeConfigMap, Key: solutionhost.EnvelopeSignedFileName, Name: moduleEnvelopeDocument}})
	if err != nil {
		return nil, err
	}
	return append(files, job), nil
}

// priorDeliveredEnvelope reads the envelope the base branch delivers under the
// overlay path, with the carrier it was delivered as; nil when none was.
func priorDeliveredEnvelope(ctx context.Context, repo, baseBranch, overlayPath string) (*solutionhost.ModuleEnvelope, []byte, error) {
	files, err := deliveredOverlayFiles(ctx, repo, baseBranch, overlayPath)
	if err != nil || !slices.Contains(files, moduleEnvelopeFile) {
		return nil, nil, err
	}
	data, err := gitCommandBytes(ctx, repo, "show", "refs/remotes/origin/"+baseBranch+":"+overlayPath+"/"+moduleEnvelopeFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read the delivered module envelope from %s: %w", baseBranch, err)
	}
	var configMap moduleManifestConfigMap
	if err = yaml.Unmarshal(data, &configMap); err != nil {
		return nil, nil, fmt.Errorf("the delivered module envelope on %s cannot be read, so no revision can be settled against it: %w", baseBranch, err)
	}
	document, held := configMap.Data[moduleEnvelopeDocument]
	if configMap.Kind != kindConfigMap || !held {
		return nil, nil, fmt.Errorf("the delivered %s on %s holds no %s, so no revision can be settled against it", moduleEnvelopeFile, baseBranch, moduleEnvelopeDocument)
	}
	envelope, err := solutionhost.EnvelopeFromVerified([]byte(document))
	if err != nil {
		return nil, nil, fmt.Errorf("the delivered module envelope on %s is not a canonical envelope, so no revision can be settled against it: %w", baseBranch, err)
	}
	var carrier []byte
	if raw, present := configMap.Data[solutionhost.EnvelopeSignedFileName]; present {
		carrier = []byte(raw)
	}
	return envelope, carrier, nil
}
