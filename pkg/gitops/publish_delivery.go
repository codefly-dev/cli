package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// --- Settling delivery documents at publish ---
//
// A render declares; publish settles. Two things about a delivery document are
// only knowable where the delivery repository's base branch is in hand, which
// is publish and not render:
//
//   - The GENERATION. It is monotonic per binding ID across everything ever
//     delivered, and the only record of what was delivered is the delivered
//     tree on the base branch. A render destination is per module and per
//     render, not per environment: rendering staging and then production again
//     replaced the production document, so a render reading its own
//     destination restarted at 1 and the host refused it as stale. Publish
//     reads the base branch instead, so the generation is settled against what
//     the host has actually been given.
//   - REMOVAL. A binding delivered before and absent from this render is, within
//     this module's own delivery, removed — and removal is a generation, never
//     an absence. Publish writes the tombstone, at the prior generation plus
//     one, and keeps carrying a tombstone already delivered so the host keeps
//     seeing it. Nothing is ever withdrawn by a document disappearing.
//
// And the SIGNATURE is made here, because the generation is inside the signed
// bytes and because the signing identity is the publishing workflow's own: a
// release is signed only by CI. A publish with no signing identity delivers
// unsigned documents to a local qualification environment only, and says so;
// everywhere else it refuses, naming the documents, because an unsigned
// document reaching a real host fails at the host, far from the cause.

const (
	// signedCarrierSchema is the schema of the carrier a delivery Job POSTs:
	// the canonical bytes that were signed, verbatim, and the Sigstore bundle
	// over them. It is what core's Carrier produces, and the Job sends it as
	// the request body.
	signedCarrierSchema = "codefly/solution-host-signed/v1"

	deliveredPresence  = "presence"
	deliveredAuthority = "authority"
)

// signedCarrier is the carrier of one signed document. Document is the
// canonical bytes verbatim, because that is what the signature covers; Bundle
// is the Sigstore bundle, a JSON object the host verifies against its trust
// root and identity allowlist — evidence, never authority.
type signedCarrier struct {
	Schema   string          `json:"schema"`
	Document json.RawMessage `json:"document"`
	Bundle   json.RawMessage `json:"bundle"`
}

// deliveryPublishOptions is what settling needs beyond the tree.
type deliveryPublishOptions struct {
	// Signer signs each document's canonical bytes. nil means the process
	// environment decides (signing.FromEnvironment).
	Signer signing.Signer
	// AllowUnsigned lets a publish deliver unsigned documents — a local
	// qualification environment only.
	AllowUnsigned bool
	// Target is the host's delivery API; the Jobs that POST the carriers are
	// rewritten against it once the document set is settled.
	Target *DeliveryTarget
	// Audience is the host audience the presence Job's token is projected for.
	Module string
}

// deliveredPresenceDocument is one presence document after settling.
type deliveredPresenceDocument struct {
	document *solutionhost.SolutionHostBinding
	alias    string
	carrier  []byte
}

// settlePresenceDelivery settles the presence documents of the tree staged at
// target against the base branch, writes tombstones for bindings this module
// delivered before and no longer declares, signs every document, rewrites the
// carriers, the delivery Job and the overlay's kustomization, and returns what
// it delivered for the inventory.
func settlePresenceDelivery(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	opts deliveryPublishOptions,
) (*InventoryDelivery, error) {
	overlay := solutionHostBindingOverlay(environment)
	prior, err := priorDeliveredBindings(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, overlay)))
	if err != nil {
		return nil, err
	}
	rendered := map[string]deliveredPresenceDocument{}
	if inventory.SolutionHostBindingPath != "" {
		rendered, err = renderedBindings(filepath.Join(target, filepath.FromSlash(overlay)))
		if err != nil {
			return nil, err
		}
	}
	if len(rendered) == 0 && len(prior) == 0 {
		return nil, nil
	}
	if len(rendered) == 0 && opts.Target == nil {
		// The render declares nothing and names no host, yet this module
		// delivered bindings before. Writing tombstones for them would withdraw
		// every binding the moment a host declaration is removed, and writing
		// nothing would leave the host holding what it can no longer be told
		// about. Neither is a publish to make silently.
		return nil, fmt.Errorf(
			"module %s delivered the bindings %s to %s before and this render declares none and names no host; to withdraw them, render with the environment's host block in place and without the instances, so publish writes their tombstones",
			inventory.Module, strings.Join(sortedBindingNames(prior), ", "), environment)
	}
	settled, err := settledPresenceSet(rendered, prior)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(settled))
	for binding := range settled {
		names = append(names, binding)
	}
	sort.Strings(names)
	// The whole set is admitted again at its settled generations: a tombstone
	// and a present document never collide, and this is the last check before
	// the host's own.
	documents := make([]*solutionhost.SolutionHostBinding, 0, len(settled))
	for _, binding := range names {
		documents = append(documents, settled[binding].document)
	}
	if _, admitErr := (solutionhost.Host{}).Admit(documents...); admitErr != nil {
		return nil, fmt.Errorf("settled solution host bindings are not admissible: %w", admitErr)
	}
	delivery, err := signPresenceSet(ctx, settled, names, opts, environment)
	if err != nil {
		return nil, err
	}
	files, err := writePresenceSet(target, overlay, inventory, settled, names, opts)
	if err != nil {
		return nil, err
	}
	if err := writeSolutionHostBindingKustomization(target, environment, files); err != nil {
		return nil, err
	}
	inventory.SolutionHostBindingPath = solutionHostBindingDir
	return delivery, nil
}

func sortedBindingNames(documents map[string]*solutionhost.SolutionHostBinding) []string {
	names := make([]string, 0, len(documents))
	for binding := range documents {
		names = append(names, binding)
	}
	sort.Strings(names)
	return names
}

// settledPresenceSet settles each rendered document against what was
// delivered, and tombstones what was delivered and is no longer rendered.
func settledPresenceSet(rendered map[string]deliveredPresenceDocument, prior map[string]*solutionhost.SolutionHostBinding) (map[string]deliveredPresenceDocument, error) {
	settled := make(map[string]deliveredPresenceDocument, len(rendered)+len(prior))
	for binding, entry := range rendered {
		previous, delivered := prior[binding]
		if delivered {
			generation, err := settledGeneration(previous, entry.document)
			if err != nil {
				return nil, err
			}
			entry.document.Generation = generation
		} else {
			entry.document.Generation = 1
		}
		settled[binding] = entry
	}
	for binding, previous := range prior {
		if _, present := rendered[binding]; present {
			continue
		}
		tombstone, err := tombstoneOf(previous)
		if err != nil {
			return nil, fmt.Errorf("withdraw binding %s: %w", binding, err)
		}
		settled[binding] = deliveredPresenceDocument{document: tombstone}
	}
	return settled, nil
}

// signPresenceSet signs every settled document's canonical bytes and assembles
// its carrier, recording what was delivered. Documents the signer cannot sign
// are left unsigned only when the publish allows it.
func signPresenceSet(ctx context.Context, settled map[string]deliveredPresenceDocument, names []string, opts deliveryPublishOptions, environment string) (*InventoryDelivery, error) {
	signer := opts.Signer
	if signer == nil {
		signer = signing.FromEnvironment(nil)
	}
	delivery := &InventoryDelivery{Signed: true}
	var unsigned []string
	for _, binding := range names {
		entry := settled[binding]
		payload, err := entry.document.CanonicalBytes()
		if err != nil {
			return nil, fmt.Errorf("canonicalize binding %s: %w", binding, err)
		}
		bundle, signErr := signer.Sign(ctx, payload)
		switch {
		case signErr == nil:
			carrier, encodeErr := json.Marshal(signedCarrier{Schema: signedCarrierSchema, Document: payload, Bundle: bundle})
			if encodeErr != nil {
				return nil, fmt.Errorf("encode the signed carrier of binding %s: %w", binding, encodeErr)
			}
			entry.carrier = carrier
			if delivery.Identity == "" {
				if identity, readErr := signing.ReadIdentity(bundle); readErr == nil {
					delivery.Identity = identity.Subject + " (" + identity.Issuer + ")"
				}
			}
		case errors.Is(signErr, signing.ErrNoIdentity):
			unsigned = append(unsigned, binding)
			delivery.Signed = false
		default:
			return nil, fmt.Errorf("sign binding %s: %w", binding, signErr)
		}
		digest, err := entry.document.Digest()
		if err != nil {
			return nil, err
		}
		delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
			Kind: deliveredPresence, ID: binding, Generation: entry.document.Generation, Removed: entry.document.Removed, Digest: digest,
		})
		settled[binding] = entry
	}
	if len(unsigned) > 0 && !opts.AllowUnsigned {
		return nil, fmt.Errorf(
			"%w: the presence documents %s cannot be delivered unsigned to %s; publish from the release workflow, whose OIDC identity signs them",
			signing.ErrNoIdentity, strings.Join(unsigned, ", "), environment)
	}
	return delivery, nil
}

// writePresenceSet writes the settled documents, the delivery account and the
// Job that POSTs every signed carrier, and returns the overlay's file list.
func writePresenceSet(target, overlay string, inventory *Inventory, settled map[string]deliveredPresenceDocument, names []string, opts deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create delivery overlay: %w", err)
	}
	var files []string
	var jobDocuments []deliveryDocument
	for _, binding := range names {
		entry := settled[binding]
		file, err := writeSettledBinding(directory, inventory.Namespace, entry)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		if len(entry.carrier) > 0 {
			jobDocuments = append(jobDocuments, deliveryDocument{
				ConfigMap: solutionHostBindingKind + "-" + binding, Key: presenceCarrierKey, Name: binding + ".json",
			})
		}
	}
	if opts.Target == nil {
		return files, nil
	}
	account, err := renderDeliveryServiceAccount(directory, inventory.Namespace)
	if err != nil {
		return nil, err
	}
	files = append(files, account)
	if len(jobDocuments) > 0 {
		job, err := renderDeliveryJob(directory, deliveryJobName(deliveryPresence, inventory.Module), inventory.Namespace,
			deliveryServiceAccount, deliveryPresence, presenceDeliveryPath, opts.Target, jobDocuments)
		if err != nil {
			return nil, err
		}
		files = append(files, job)
	}
	return files, nil
}

// settledGeneration is the generation of a candidate against the document the
// base branch delivered for the same binding: unchanged keeps it, changed is
// prior + 1. "Unchanged" is decided by core's own canonical digest, so a byte
// the host would read as a rewrite is a byte that bumps the generation.
func settledGeneration(prior, candidate *solutionhost.SolutionHostBinding) (uint64, error) {
	at := *candidate
	at.Generation = prior.Generation
	// A binding delivered as a tombstone and declared again is a new
	// generation of the same binding, never a resurrection of the old one: the
	// tombstone keeps its generation and the re-presentation follows it.
	current, err := at.Digest()
	if err != nil {
		return 0, err
	}
	delivered, err := prior.Digest()
	if err != nil {
		return 0, fmt.Errorf("digest the delivered binding %q: %w", candidate.Binding, err)
	}
	if current == delivered && !prior.Removed {
		return prior.Generation, nil
	}
	return prior.Generation + 1, nil
}

// tombstoneOf is the generation that withdraws a delivered binding. A tombstone
// already delivered is carried forward verbatim, so the host keeps being told,
// at the same generation, that the binding is absent.
func tombstoneOf(prior *solutionhost.SolutionHostBinding) (*solutionhost.SolutionHostBinding, error) {
	if prior.Removed {
		copied := *prior
		return &copied, nil
	}
	tombstone := *prior
	tombstone.Generation = prior.Generation + 1
	tombstone.Removed = true
	tombstone.Routes, tombstone.Artifacts, tombstone.Modules, tombstone.Endpoints = nil, nil, nil, nil
	if err := tombstone.Validate(); err != nil {
		return nil, err
	}
	return &tombstone, nil
}

// renderedBindings reads the presence ConfigMaps a render wrote into its
// overlay, keyed by binding ID.
func renderedBindings(directory string) (map[string]deliveredPresenceDocument, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the rendered solution host bindings: %w", err)
	}
	rendered := map[string]deliveredPresenceDocument{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == kustomizationFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // a file of the render this publish stages
		if err != nil {
			return nil, err
		}
		document, alias, ok, err := bindingFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if !ok {
			continue
		}
		rendered[document.Binding] = deliveredPresenceDocument{document: document, alias: alias}
	}
	return rendered, nil
}

// bindingFromConfigMap parses the document out of a ConfigMap carrying one, and
// reports false for any other manifest in the overlay (the Job, the account).
func bindingFromConfigMap(data []byte) (*solutionhost.SolutionHostBinding, string, bool, error) {
	var carrier solutionHostBindingConfigMap
	if err := yaml.Unmarshal(data, &carrier); err != nil {
		return nil, "", false, fmt.Errorf("decode: %w", err)
	}
	if carrier.Kind != kindConfigMap {
		return nil, "", false, nil
	}
	encoded, held := carrier.Data[solutionhost.FileName]
	if !held {
		return nil, "", false, nil
	}
	document, err := solutionhost.Parse([]byte(encoded))
	if err != nil {
		return nil, "", false, fmt.Errorf("the delivered document is not one this Core reads: %w", err)
	}
	return document, carrier.Metadata.Labels[solutionLabel], true, nil
}

// priorDeliveredBindings reads the presence documents the base branch delivers
// under the overlay path, keyed by binding ID. A path the base branch does not
// have delivered nothing.
func priorDeliveredBindings(ctx context.Context, repo, baseBranch, overlayPath string) (map[string]*solutionhost.SolutionHostBinding, error) {
	ref := "refs/remotes/origin/" + baseBranch
	listing, err := gitCommand(ctx, repo, "ls-tree", "--name-only", ref+":"+overlayPath)
	if err != nil {
		// git fails the listing when the path is absent from the branch, which
		// is the ordinary first publish. Any other failure surfaces when the
		// blob is shown below.
		return map[string]*solutionhost.SolutionHostBinding{}, nil
	}
	prior := map[string]*solutionhost.SolutionHostBinding{}
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, ".yaml") || name == kustomizationFile {
			continue
		}
		data, err := gitCommandBytes(ctx, repo, "show", ref+":"+overlayPath+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read the delivered %s from %s: %w", name, baseBranch, err)
		}
		document, _, ok, err := bindingFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("the delivered %s on %s cannot be read, so no generation can be settled against it: %w", name, baseBranch, err)
		}
		if !ok {
			continue
		}
		prior[document.Binding] = document
	}
	return prior, nil
}

// writeSettledBinding writes one settled document's ConfigMap: the readable
// document, and the signed carrier when there is one.
func writeSettledBinding(directory, namespace string, entry deliveredPresenceDocument) (string, error) {
	encoded, err := solutionhost.Marshal(entry.document)
	if err != nil {
		return "", fmt.Errorf("marshal binding %q: %w", entry.document.Binding, err)
	}
	labels := map[string]string{
		managedByLabel:           managedByCodefly,
		solutionHostBindingLabel: solutionHostBindingKind,
		bindingLabel:             entry.document.Binding,
	}
	if entry.alias != "" {
		labels[solutionLabel] = entry.alias
	}
	carrier := solutionHostBindingConfigMap{
		APIVersion: "v1",
		Kind:       kindConfigMap,
		Metadata: solutionHostBindingConfigMapMeta{
			Name: solutionHostBindingKind + "-" + entry.document.Binding, Namespace: namespace, Labels: labels,
		},
		Data: map[string]string{solutionhost.FileName: string(encoded)},
	}
	if len(entry.carrier) > 0 {
		carrier.Data[presenceCarrierKey] = string(entry.carrier)
	}
	body, err := yaml.Marshal(carrier)
	if err != nil {
		return "", fmt.Errorf("encode binding %q: %w", entry.document.Binding, err)
	}
	file := entry.document.Binding + ".yaml"
	if err := os.WriteFile(filepath.Join(directory, file), body, 0o644); err != nil { //nolint:gosec // a delivered manifest, readable beside the rest of the tree
		return "", fmt.Errorf("write binding %q: %w", entry.document.Binding, err)
	}
	return file, nil
}
