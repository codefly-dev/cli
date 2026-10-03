package gitops

import (
	"context"
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
	deliveredPresence  = "presence"
	deliveredAuthority = "authority"
)

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
	// EnvelopeRevision is the revision of the host's envelope the environment
	// declares at publish — the one number of the envelope a composition holds.
	// Both halves of every authority pair must name it; a render stamped
	// against a revision the composition no longer declares is refused before
	// it is written. Zero when the environment declares no host.
	EnvelopeRevision uint64
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
	// The whole set is checked again at its settled generations: a tombstone
	// and a present document never collide, and this is the last check before
	// the host's own. It is the renderer's share of admission over parsed
	// documents (core's AdmitRendered): the carriers are assembled below, and
	// verifying them here against the signing identity would be the host's
	// own check run early — worth having, not built.
	documents := make([]*solutionhost.SolutionHostBinding, 0, len(settled))
	for _, binding := range names {
		documents = append(documents, settled[binding].document)
	}
	if oneErr := solutionhost.OneDelivery(documents...); oneErr != nil {
		return nil, fmt.Errorf("settled solution host bindings are not one delivery: %w", oneErr)
	}
	if _, admitErr := solutionhost.AdmitRendered(documents...); admitErr != nil {
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
			signed, carrierErr := solutionhost.Carrier(payload, bundle)
			if carrierErr != nil {
				return nil, fmt.Errorf("assemble the signed carrier of binding %s: %w", binding, carrierErr)
			}
			carrier, encodeErr := solutionhost.MarshalSigned(signed)
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
	// A tombstone is terminal. The binding ID is the handle every other system
	// holds — installations, operation bindings, team grants — so a later
	// generation under the same ID is indistinguishable from continuity, which
	// is the one thing a withdrawal exists to make distinguishable; the host
	// refuses it (ErrTombstoned) and so does publish, where the rename that
	// fixes it is one line away. A genuinely new instance has a genuinely new
	// ID, because the ID is derived from the instance.
	if prior.Removed {
		return 0, fmt.Errorf("%w: binding %q was withdrawn at generation %d, so this render cannot present it again under that ID; a new instance needs a new name, which gives it a new binding ID",
			solutionhost.ErrTombstoned, candidate.Binding, prior.Generation)
	}
	at := *candidate
	at.Generation = prior.Generation
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
	tombstone.Routes, tombstone.Artifacts, tombstone.Workloads, tombstone.Modules, tombstone.Endpoints = nil, nil, nil, nil, nil
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

// --- Settling authority documents at publish ---
//
// An authority document is settled the way a presence document is — generation
// against the base branch, tombstone for what is no longer declared, signature
// over the canonical bytes — with two rules of its own. It is effective from
// the presence generation settled in the same publish, so the two halves of a
// tuple always travel together. And a binding whose authority content changed
// while its revision did not is refused: a credential seals the revision and is
// refused when the live one moves, so a change that keeps the number keeps
// every outstanding credential's old authority with nothing detecting it.

// deliveredAuthorityDocument is one authority document after settling.
type deliveredAuthorityDocument struct {
	document *solutionhost.AuthorityDocument
	module   string
	carrier  []byte
}

// settleAuthorityDelivery settles the authority documents of the tree staged at
// target against the base branch and the presence generations settled in this
// publish, signs them, and rewrites the carriers, the authority Job and the
// overlay's kustomization.
func settleAuthorityDelivery(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	presence *InventoryDelivery,
	opts deliveryPublishOptions,
) (*InventoryDelivery, error) {
	overlay := solutionAuthorityOverlay(environment)
	prior, err := priorDeliveredAuthorities(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, overlay)))
	if err != nil {
		return nil, err
	}
	rendered := map[string]deliveredAuthorityDocument{}
	if inventory.SolutionAuthorityPath != "" {
		if rendered, err = renderedAuthorities(filepath.Join(target, filepath.FromSlash(overlay))); err != nil {
			return nil, err
		}
	}
	if len(rendered) == 0 && len(prior) == 0 {
		return nil, nil
	}
	if len(rendered) == 0 && opts.Target == nil {
		names := make([]string, 0, len(prior))
		for authority := range prior {
			names = append(names, authority)
		}
		sort.Strings(names)
		return nil, fmt.Errorf(
			"module %s delivered the authority documents %s to %s before and this render declares none and names no host; to withdraw them, render with the environment's host block in place, so publish writes their tombstones",
			inventory.Module, strings.Join(names, ", "), environment)
	}
	settled, err := settledAuthoritySet(rendered, prior, presenceGenerations(presence))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(settled))
	for authority := range settled {
		names = append(names, authority)
	}
	sort.Strings(names)
	if err = refuseUnmatchedPairs(target, environment, inventory.Module, settled, names, prior, opts.EnvelopeRevision); err != nil {
		return nil, err
	}
	delivery, err := signAuthoritySet(ctx, settled, names, opts, environment)
	if err != nil {
		return nil, err
	}
	files, err := writeAuthoritySet(target, overlay, inventory, settled, names, opts)
	if err != nil {
		return nil, err
	}
	if err := writeAuthorityKustomization(target, environment, files); err != nil {
		return nil, err
	}
	inventory.SolutionAuthorityPath = solutionAuthorityDir
	return delivery, nil
}

// refuseUnmatchedPairs holds every settled authority document against the
// presence document it is granted over — the one this publish just wrote to the
// staged tree — with core's ActivateRendered: the renderer's share of
// activation, as AdmitRendered is its share of admission. It answers what needs
// no host state: the fold against what the base branch delivered (a domain or
// binding the authority would migrate across, a rewritten generation), the
// target binding, host and domain agreeing, both halves naming the envelope
// revision the environment declares NOW, the approved build being one the
// presence says the binding runs, and the effective-from generation. What it
// cannot answer stays the host's: who signed either half, and whether the
// authority fits the ceiling — the envelope is the host's record and a renderer
// holds only its revision, so a RenderedMatch is not an Activation.
func refuseUnmatchedPairs(
	target, environment, module string,
	settled map[string]deliveredAuthorityDocument,
	names []string,
	prior map[string]deliveredAuthorityDocument,
	envelopeRevision uint64,
) error {
	var granted []string
	for _, authority := range names {
		if !settled[authority].document.Removed {
			granted = append(granted, authority)
		}
	}
	if len(granted) == 0 {
		// Withdrawals activate nothing and have nothing to match.
		return nil
	}
	if envelopeRevision == 0 {
		return fmt.Errorf(
			"module %s renders the authority documents %s for %s, but the environment declares no host now, so there is no envelope revision to hold them against; render again against the composition as it is",
			module, strings.Join(granted, ", "), environment)
	}
	presence, err := renderedBindings(filepath.Join(target, filepath.FromSlash(solutionHostBindingOverlay(environment))))
	if err != nil {
		return err
	}
	for _, authority := range granted {
		entry := settled[authority]
		binding := entry.document.PresenceBinding
		over, present := presence[binding]
		if !present {
			return fmt.Errorf("authority %s is granted over binding %s, which this publish does not deliver", authority, binding)
		}
		var applied solutionhost.AppliedAuthority
		if previous, delivered := prior[authority]; delivered {
			if applied, err = solutionhost.AppliedAuthorityFrom(previous.document); err != nil {
				return fmt.Errorf("record the delivered authority %s: %w", authority, err)
			}
		}
		if _, err := solutionhost.ActivateRendered(solutionhost.RenderedActivationRequest{
			Authority:        entry.document,
			Presence:         over.document,
			Build:            entry.document.ApprovedBuild,
			EnvelopeRevision: envelopeRevision,
			Applied:          applied,
		}); err != nil {
			return fmt.Errorf("authority %s and binding %s are not a pair the host would activate: %w", authority, binding, err)
		}
	}
	return nil
}

// presenceGenerations indexes the presence generations a publish settled, by
// binding ID.
func presenceGenerations(presence *InventoryDelivery) map[string]uint64 {
	generations := map[string]uint64{}
	if presence == nil {
		return generations
	}
	for _, document := range presence.Documents {
		if document.Kind == deliveredPresence && !document.Removed {
			generations[document.ID] = document.Generation
		}
	}
	return generations
}

// settledAuthoritySet settles each rendered authority document against what
// was delivered, makes it effective from the presence generation settled for
// its module, and tombstones what was delivered and is no longer rendered.
func settledAuthoritySet(
	rendered map[string]deliveredAuthorityDocument,
	prior map[string]deliveredAuthorityDocument,
	generations map[string]uint64,
) (map[string]deliveredAuthorityDocument, error) {
	settled := make(map[string]deliveredAuthorityDocument, len(rendered)+len(prior))
	for authority, entry := range rendered {
		binding, _, _ := strings.Cut(authority, ":")
		effective, present := generations[binding]
		if !present {
			return nil, fmt.Errorf("authority %s is effective from the presence of binding %s, which this publish does not deliver", authority, binding)
		}
		entry.document.EffectiveFrom = effective
		previous, delivered := prior[authority]
		if !delivered {
			entry.document.Generation = 1
			settled[authority] = entry
			continue
		}
		if previous.document.Removed {
			// Terminal, as for presence: a withdrawn authority ID is never
			// granted again, so the instance (and with it the ID) is renamed.
			return nil, fmt.Errorf("%w: authority %q was withdrawn at generation %d, so this render cannot grant it again under that ID; a new instance needs a new name, which gives it a new authority ID",
				solutionhost.ErrTombstoned, authority, previous.document.Generation)
		}
		if err := refuseUnbumpedBindings(previous.document, entry.document); err != nil {
			return nil, err
		}
		at := *entry.document
		at.Generation = previous.document.Generation
		current, err := at.Digest()
		if err != nil {
			return nil, err
		}
		before, err := previous.document.Digest()
		if err != nil {
			return nil, fmt.Errorf("digest the delivered authority %q: %w", authority, err)
		}
		if current == before && !previous.document.Removed {
			entry.document.Generation = previous.document.Generation
		} else {
			entry.document.Generation = previous.document.Generation + 1
		}
		settled[authority] = entry
	}
	for authority, previous := range prior {
		if _, present := rendered[authority]; present {
			continue
		}
		tombstone := *previous.document
		if !previous.document.Removed {
			tombstone.Generation = previous.document.Generation + 1
			tombstone.Removed = true
			tombstone.ApprovedBuild, tombstone.EffectiveFrom, tombstone.Principals = "", 0, nil
			if err := tombstone.Validate(); err != nil {
				return nil, fmt.Errorf("withdraw authority %s: %w", authority, err)
			}
		}
		settled[authority] = deliveredAuthorityDocument{document: &tombstone, module: previous.module}
	}
	return settled, nil
}

// refuseUnbumpedBindings refuses an authority document in which a binding's
// content changed while its revision did not. The revision is what a sealed
// credential is held against; a change that keeps it is a change no credential
// can detect, and the author remembering to bump it is not enforcement.
func refuseUnbumpedBindings(prior, candidate *solutionhost.AuthorityDocument) error {
	previous := map[string]solutionhost.AuthorityBinding{}
	for _, principal := range prior.Principals {
		for _, binding := range principal.Bindings {
			previous[binding.ID] = binding
		}
	}
	var unbumped []string
	for _, principal := range candidate.Principals {
		for _, binding := range principal.Bindings {
			before, delivered := previous[binding.ID]
			if !delivered || before.Revision != binding.Revision {
				continue
			}
			if before.Audience != binding.Audience || before.Scope != binding.Scope || before.Queue != binding.Queue || before.Namespace != binding.Namespace {
				unbumped = append(unbumped, binding.ID)
			}
		}
	}
	if len(unbumped) == 0 {
		return nil
	}
	sort.Strings(unbumped)
	return fmt.Errorf(
		"authority %s changes what the bindings %s grant without bumping their revision; a credential sealed to the old revision would keep the old authority undetected, so bump the binding's revision in the module contract",
		candidate.Authority, strings.Join(unbumped, ", "))
}

// signAuthoritySet signs every settled authority document and assembles its
// carrier, recording what was delivered.
func signAuthoritySet(ctx context.Context, settled map[string]deliveredAuthorityDocument, names []string, opts deliveryPublishOptions, environment string) (*InventoryDelivery, error) {
	signer := opts.Signer
	if signer == nil {
		signer = signing.FromEnvironment(nil)
	}
	delivery := &InventoryDelivery{Signed: true}
	var unsigned []string
	for _, authority := range names {
		entry := settled[authority]
		payload, err := solutionhost.SignedPayloadFor(entry.document)
		if err != nil {
			return nil, fmt.Errorf("canonicalize authority %s: %w", authority, err)
		}
		bundle, signErr := signer.Sign(ctx, payload)
		switch {
		case signErr == nil:
			signed, carrierErr := solutionhost.Carrier(payload, bundle)
			if carrierErr != nil {
				return nil, fmt.Errorf("assemble the signed carrier of authority %s: %w", authority, carrierErr)
			}
			carrier, encodeErr := solutionhost.MarshalSigned(signed)
			if encodeErr != nil {
				return nil, fmt.Errorf("encode the signed carrier of authority %s: %w", authority, encodeErr)
			}
			entry.carrier = carrier
			if delivery.Identity == "" {
				if identity, readErr := signing.ReadIdentity(bundle); readErr == nil {
					delivery.Identity = identity.Subject + " (" + identity.Issuer + ")"
				}
			}
		case errors.Is(signErr, signing.ErrNoIdentity):
			unsigned = append(unsigned, authority)
			delivery.Signed = false
		default:
			return nil, fmt.Errorf("sign authority %s: %w", authority, signErr)
		}
		digest, err := entry.document.Digest()
		if err != nil {
			return nil, err
		}
		delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
			Kind: deliveredAuthority, ID: authority, Generation: entry.document.Generation, Removed: entry.document.Removed, Digest: digest,
		})
		settled[authority] = entry
	}
	if len(unsigned) > 0 && !opts.AllowUnsigned {
		return nil, fmt.Errorf(
			"%w: the authority documents %s cannot be delivered unsigned to %s; publish from the release workflow, whose OIDC identity signs them",
			signing.ErrNoIdentity, strings.Join(unsigned, ", "), environment)
	}
	return delivery, nil
}

// writeAuthoritySet writes the settled documents and the Job that POSTs every
// signed carrier from the authority namespace, and returns the overlay's files.
// The account the Job runs as is the platform's: that namespace is not the
// module's to populate.
func writeAuthoritySet(target, overlay string, inventory *Inventory, settled map[string]deliveredAuthorityDocument, names []string, opts deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create authority overlay: %w", err)
	}
	var files []string
	var jobDocuments []deliveryDocument
	for _, authority := range names {
		entry := settled[authority]
		file, err := writeAuthorityDocument(directory, entry.document, entry.module, entry.carrier)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		if len(entry.carrier) > 0 {
			jobDocuments = append(jobDocuments, deliveryDocument{
				ConfigMap: authorityConfigMapName(authority), Key: authorityCarrierKey, Name: strings.TrimSuffix(file, ".yaml") + ".json",
			})
		}
	}
	if opts.Target == nil || len(jobDocuments) == 0 {
		return files, nil
	}
	job, err := renderDeliveryJob(directory, deliveryJobName(deliveryAuthority, inventory.Module), authorityNamespace,
		deliveryServiceAccount, deliveryAuthority, authorityDeliveryPath, opts.Target, jobDocuments)
	if err != nil {
		return nil, err
	}
	return append(files, job), nil
}

// renderedAuthorities reads the authority ConfigMaps a render wrote into its
// overlay, keyed by authority ID.
func renderedAuthorities(directory string) (map[string]deliveredAuthorityDocument, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the rendered authority documents: %w", err)
	}
	rendered := map[string]deliveredAuthorityDocument{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == kustomizationFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // a file of the render this publish stages
		if err != nil {
			return nil, err
		}
		document, module, ok, err := authorityFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if !ok {
			continue
		}
		rendered[document.Authority] = deliveredAuthorityDocument{document: document, module: module}
	}
	return rendered, nil
}

// authorityFromConfigMap parses the document out of a ConfigMap carrying one,
// and reports false for any other manifest in the overlay.
func authorityFromConfigMap(data []byte) (*solutionhost.AuthorityDocument, string, bool, error) {
	var carrier solutionAuthorityConfigMap
	if err := yaml.Unmarshal(data, &carrier); err != nil {
		return nil, "", false, fmt.Errorf("decode: %w", err)
	}
	if carrier.Kind != kindConfigMap {
		return nil, "", false, nil
	}
	encoded, held := carrier.Data[solutionhost.AuthorityFileName]
	if !held {
		return nil, "", false, nil
	}
	document, err := solutionhost.ParseAuthority([]byte(encoded))
	if err != nil {
		return nil, "", false, fmt.Errorf("the delivered authority document is not one this Core reads: %w", err)
	}
	return document, carrier.Metadata.Labels[solutionLabel], true, nil
}

// priorDeliveredAuthorities reads the authority documents the base branch
// delivers under the overlay path, keyed by authority ID.
func priorDeliveredAuthorities(ctx context.Context, repo, baseBranch, overlayPath string) (map[string]deliveredAuthorityDocument, error) {
	ref := "refs/remotes/origin/" + baseBranch
	listing, err := gitCommand(ctx, repo, "ls-tree", "--name-only", ref+":"+overlayPath)
	if err != nil {
		return map[string]deliveredAuthorityDocument{}, nil
	}
	prior := map[string]deliveredAuthorityDocument{}
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, ".yaml") || name == kustomizationFile {
			continue
		}
		data, err := gitCommandBytes(ctx, repo, "show", ref+":"+overlayPath+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read the delivered %s from %s: %w", name, baseBranch, err)
		}
		document, module, ok, err := authorityFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("the delivered %s on %s cannot be read, so no generation can be settled against it: %w", name, baseBranch, err)
		}
		if !ok {
			continue
		}
		prior[document.Authority] = deliveredAuthorityDocument{document: document, module: module}
	}
	return prior, nil
}
