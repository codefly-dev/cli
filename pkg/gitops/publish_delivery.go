package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/core/solutionhost"
)

// --- Settling module manifests at publish ---
//
// A render declares; publish settles. Two things about a manifest are only
// knowable where the delivery repository's base branch is in hand, which is
// publish and not render:
//
//   - The GENERATION. It is monotonic per binding across everything ever
//     delivered, and the only record of what was delivered is the delivered
//     tree on the base branch. Unchanged canonical content keeps the delivered
//     generation; changed content is the next one.
//   - REMOVAL. A binding delivered before and absent from this render is, within
//     this module's own delivery, removed — and removal is a generation, never
//     an absence: publish writes the manifest with `removed: true` at the next
//     generation, and keeps carrying a withdrawal already delivered so the
//     host keeps seeing it.
//
// And the SIGNATURE is made here, because the generation is inside the signed
// bytes and release custody belongs to the platform's Vault Transit. A publish
// with no signing identity delivers unsigned manifests to a local
// qualification environment only, and says so; everywhere else it refuses,
// naming them, because an unsigned manifest reaching a real host fails at the
// host, far from the cause.

// deliveredManifestKind is a delivered manifest's kind in the inventory's
// delivery record and in a carrier's reuse key.
const deliveredManifestKind = "manifest"

// deliveryPublishOptions is what settling needs beyond the tree.
type deliveryPublishOptions struct {
	// Signer signs each manifest's canonical bytes. nil means the process
	// environment decides (signing.FromEnvironment).
	Signer signing.Signer
	// Coordinate and TrustDomain name the host the environment declares at
	// publish; every rendered manifest is stamped with the coordinate, and the
	// render's recorded mesh edges with the trust domain. Empty when the
	// environment declares no host.
	Coordinate, TrustDomain string
	// ReuseCheck verifies a carrier signed BEFORE this publish — by the base
	// branch's release, or by the plan this publish executes — under the
	// release policy: the declared key and ownership domain. A carrier it
	// refuses is re-signed, never reused. nil skips the policy.
	ReuseCheck signing.SelfCheck
	// Reuse holds the carriers an inspected plan signed, keyed by
	// carrierKey, for the publish that executes the plan to deliver exactly
	// those bytes rather than signing again.
	Reuse map[string][]byte
	// Executing marks the publish that executes an inspected plan: it signs
	// nothing the plan did not, and refuses rather than produce a signature
	// the plan compared without.
	Executing bool
	// Resign lets a plan sign a manifest again whose delivered carrier the
	// release policy no longer admits — a rotated signing identity — which
	// is deliberate, never implied: without it such a carrier refuses.
	Resign bool
	// Carriers collects every carrier this settlement delivers, keyed by
	// carrierKey, so a plan can hand them to the publish that executes it.
	Carriers map[string][]byte
	// AllowUnsigned lets a publish deliver unsigned manifests — a local
	// qualification environment only.
	AllowUnsigned bool
	// Target is the host's delivery API; the Job that POSTs the carriers is
	// written against it once the set is settled.
	Target *DeliveryTarget
	// Domain is the ownership domain the environment declares at publish. A
	// withdrawal is authored by the domain that delivered the binding. Empty
	// when the environment declares no host.
	Domain string
	// Module is the module being published.
	Module string
	// SelfCheck verifies each carrier right after it is signed, as a host
	// would — offline, against the custody key and the declared ownership
	// domain — so a carrier the host would refuse never leaves the
	// publisher. nil runs no check.
	SelfCheck signing.SelfCheck
	// Envelope is the module envelope the composition states, set only for
	// the publish that delivers it (declareEnvelope); nil delivers none.
	Envelope *declaredEnvelope
	// Signs names who signs this publish's manifests, for the refusal a
	// publish that may not deliver unsigned gives when its signer had no
	// identity. Empty means the release workflow.
	Signs string
}

// unsignedRefusal is the refusal of a publish that may not deliver unsigned
// and could not sign: the documents left unsigned, why the signer had no
// identity, and who signs instead.
func unsignedRefusal(unsigned, environment string, cause error, opts *deliveryPublishOptions) error {
	signs := opts.Signs
	if signs == "" {
		signs = "configure VAULT_ADDR, VAULT_TOKEN and CODEFLY_RELEASE_TRANSIT_KEY (Vault Transit) or CODEFLY_RELEASE_KMS_KEY (Cloud KMS) to sign with the platform release key"
	}
	return fmt.Errorf("%w: %s cannot be delivered unsigned to %s (%v); %s",
		signing.ErrNoIdentity, unsigned, environment, cause, signs)
}

// deliveredManifest is one manifest after settling, with the carrier it is
// delivered as.
type deliveredManifest struct {
	manifest *solutionhost.ModuleManifest
	carrier  []byte
}

// settleManifestDelivery settles the delivery documents of the tree staged at
// target against the base branch — the module's manifests and, for the module
// that hosts the delivery API, the environment's module envelope — writes the
// overlay's kustomization over everything they deliver, and returns what was
// delivered for the inventory.
func settleManifestDelivery(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	opts *deliveryPublishOptions,
) (*InventoryDelivery, error) {
	delivery, files, err := settleManifests(ctx, repo, baseBranch, target, targetPath, environment, inventory, opts)
	if err != nil {
		return nil, err
	}
	if delivery, files, err = settleEnvelope(ctx, repo, baseBranch, target, targetPath, environment, inventory, delivery, files, opts); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return delivery, nil
	}
	if err := writeModuleManifestKustomization(target, environment, files); err != nil {
		return nil, err
	}
	inventory.ModuleManifestPath = moduleManifestDir
	return delivery, nil
}

// settleManifests settles the manifests of the staged tree against the base
// branch, writes the withdrawal of every binding this module delivered before
// and no longer declares, signs every manifest, writes the carriers and the
// delivery Job, and returns what it delivered and the files it wrote.
func settleManifests(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	opts *deliveryPublishOptions,
) (*InventoryDelivery, []string, error) {
	overlay := moduleManifestOverlay(environment)
	prior, err := priorDeliveredManifests(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, overlay)))
	if err != nil {
		return nil, nil, err
	}
	rendered := map[string]deliveredManifest{}
	if inventory.ModuleManifestPath != "" {
		if rendered, err = renderedManifests(filepath.Join(target, filepath.FromSlash(overlay))); err != nil {
			return nil, nil, err
		}
	}
	if len(rendered) == 0 && len(prior) == 0 {
		return nil, nil, nil
	}
	if len(rendered) == 0 && opts.Target == nil {
		// Withdrawing on the removal of a host declaration would withdraw
		// every binding; writing nothing would leave the host holding what it
		// can no longer be told about. Neither is a publish to make silently.
		return nil, nil, fmt.Errorf(
			"module %s delivered the bindings %s to %s before and this render declares none and names no host; to withdraw them, render with the environment's host block in place and without the instances, so publish writes their withdrawals",
			inventory.Module, strings.Join(sortedBindings(prior), ", "), environment)
	}
	for binding, previous := range prior {
		if err = refuseAnotherHostsRecord(binding, previous.manifest.Host, opts); err != nil {
			return nil, nil, err
		}
		if _, present := rendered[binding]; !present {
			if err = refuseForeignWithdrawal(binding, previous.manifest.Host.Domain, opts.Domain); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(rendered) > 0 {
		if opts.Target == nil {
			return nil, nil, fmt.Errorf("the render declares the module manifests %s for %s but the environment names no host now; render again against the environment as it is, or declare the host they are delivered to", strings.Join(sortedBindings(rendered), ", "), environment)
		}
		for binding, entry := range rendered {
			if err = refuseMovedManifest(binding, entry.manifest, opts); err != nil {
				return nil, nil, err
			}
		}
		if err = refuseMovedTrustDomain(inventory, opts); err != nil {
			return nil, nil, err
		}
	}
	settled, err := settledManifestSet(rendered, prior)
	if err != nil {
		return nil, nil, err
	}
	names := sortedBindings(settled)
	// Each manifest at its settled generation is held against what the base
	// branch delivered for its binding with core's own admission — a
	// withdrawn binding presented again, a domain the binding was not
	// delivered under, another host — the last check before the host's own,
	// which runs the same decision over its own record.
	for _, binding := range names {
		var previous *solutionhost.ModuleManifest
		if delivered, ok := prior[binding]; ok {
			previous = delivered.manifest
		}
		candidate := settled[binding].manifest
		if _, admitErr := solutionhost.Admit(candidate.Host.Coordinate, previous, candidate); admitErr != nil {
			if errors.Is(admitErr, solutionhost.ErrWithdrawn) {
				return nil, nil, fmt.Errorf("module manifest %s is not admissible: %w; a new instance needs a new name, which gives it a new binding", binding, admitErr)
			}
			return nil, nil, fmt.Errorf("module manifest %s is not admissible: %w", binding, admitErr)
		}
	}
	delivery, err := signManifestSet(ctx, settled, names, opts, environment)
	if err != nil {
		return nil, nil, err
	}
	files, err := writeManifestSet(target, overlay, inventory, settled, names, opts)
	if err != nil {
		return nil, nil, err
	}
	return delivery, files, nil
}

// refuseAnotherHostsRecord holds what the base branch delivered under this
// environment's name to the host the environment declares now: a manifest
// delivered to another host coordinate is another host's record, and a host
// change is a new environment, never a generation of the old one's bindings.
func refuseAnotherHostsRecord(binding string, host solutionhost.HostTarget, opts *deliveryPublishOptions) error {
	if opts.Coordinate == "" || host.Coordinate == opts.Coordinate {
		return nil
	}
	return fmt.Errorf("module manifest %s was delivered to host %s and this environment delivers to %s now; a host change is a new environment with a delivery path of its own, never a publish over another host's record", binding, host.Coordinate, opts.Coordinate)
}

// refuseMovedManifest holds a rendered manifest to the host the environment
// declares at publish: its ownership domain, coordinate and every workload's
// trust domain must be the ones declared now, or it was rendered against
// another declaration and is rendered again before it is signed.
func refuseMovedManifest(binding string, manifest *solutionhost.ModuleManifest, opts *deliveryPublishOptions) error {
	if manifest.Host.Domain != opts.Domain {
		return fmt.Errorf("module manifest %s was rendered under ownership domain %q and the environment declares %q now; render again before publishing", binding, manifest.Host.Domain, opts.Domain)
	}
	if opts.Coordinate != "" && manifest.Host.Coordinate != opts.Coordinate {
		return fmt.Errorf("module manifest %s was rendered for host %s and the environment declares %s now; render again before publishing", binding, manifest.Host.Coordinate, opts.Coordinate)
	}
	return nil
}

// refuseMovedTrustDomain holds the render to the trust domain the environment
// declares now. A manifest workload names no trust domain (the cell spells
// its identity by rule), but the render baked the trust domain into every
// principal it wrote — the mesh edges the cell file is read from, each
// module-authority PRINCIPAL — so a render made before the host's trust domain
// moved would deliver identities the mesh no longer issues. The render
// records the trust domain it used with its mesh edges.
func refuseMovedTrustDomain(inventory *Inventory, opts *deliveryPublishOptions) error {
	if opts.TrustDomain == "" || inventory.MeshEdges == nil || inventory.MeshEdges.TrustDomain == opts.TrustDomain {
		return nil
	}
	return fmt.Errorf("module %s was rendered for trust domain %q and the environment declares %q now; render again before publishing",
		inventory.Module, inventory.MeshEdges.TrustDomain, opts.TrustDomain)
}

// refuseForeignWithdrawal refuses to withdraw a binding delivered under
// another ownership domain than the one this publish declares: a withdrawal
// is authored by the domain that delivered the binding.
func refuseForeignWithdrawal(binding, delivered, declared string) error {
	if delivered == declared {
		return nil
	}
	return fmt.Errorf("%w: module manifest %s was delivered under domain %q and this environment declares %q, so this publish cannot withdraw it; a withdrawal is authored by the domain that delivered the binding",
		solutionhost.ErrWrongDomain, binding, delivered, declared)
}

func sortedBindings(manifests map[string]deliveredManifest) []string {
	names := make([]string, 0, len(manifests))
	for binding := range manifests {
		names = append(names, binding)
	}
	sort.Strings(names)
	return names
}

// settledManifestSet settles each rendered manifest against what was
// delivered, and withdraws what was delivered and is no longer rendered.
// Unchanged, a manifest keeps the carrier it was delivered as: the same bytes,
// the same signature, and a re-sync that finds nothing to do.
func settledManifestSet(rendered, prior map[string]deliveredManifest) (map[string]deliveredManifest, error) {
	settled := make(map[string]deliveredManifest, len(rendered)+len(prior))
	for binding, entry := range rendered {
		entry.carrier = nil
		entry.manifest.Generation = 1
		if previous, delivered := prior[binding]; delivered {
			generation, err := settledGeneration(previous.manifest, entry.manifest)
			if err != nil {
				return nil, err
			}
			entry.manifest.Generation = generation
			if generation == previous.manifest.Generation {
				entry.carrier = previous.carrier
			}
		}
		settled[binding] = entry
	}
	for binding, previous := range prior {
		if _, present := rendered[binding]; present {
			continue
		}
		withdrawal, err := WithdrawalOf(previous.manifest)
		if err != nil {
			return nil, fmt.Errorf("withdraw binding %s: %w", binding, err)
		}
		entry := deliveredManifest{manifest: withdrawal}
		if previous.manifest.Removed {
			// A withdrawal carried forward verbatim keeps its carrier too.
			entry.carrier = previous.carrier
		}
		settled[binding] = entry
	}
	return settled, nil
}

// settledGeneration is the generation of a candidate against the manifest
// delivered for the same binding: the same canonical content keeps it, other
// content is the next one. Only the NUMBER is chosen here; whether the
// candidate may carry it is core's Admit.
func settledGeneration(prior, candidate *solutionhost.ModuleManifest) (uint64, error) {
	at := *candidate
	at.Generation = prior.Generation
	current, err := at.Digest()
	if err != nil {
		return 0, err
	}
	delivered, err := prior.Digest()
	if err != nil {
		return 0, fmt.Errorf("digest the delivered module manifest %q: %w", candidate.Binding, err)
	}
	if current == delivered {
		return prior.Generation, nil
	}
	return prior.Generation + 1, nil
}

// WithdrawalOf is the generation that withdraws a delivered binding: its
// identity alone, `removed: true`, at the next generation. A withdrawal
// already delivered is carried forward verbatim, so the host keeps being told,
// at the same generation, that the binding is gone.
func WithdrawalOf(prior *solutionhost.ModuleManifest) (*solutionhost.ModuleManifest, error) {
	if prior.Removed {
		copied := *prior
		return &copied, nil
	}
	withdrawal := &solutionhost.ModuleManifest{
		Schema: prior.Schema, Binding: prior.Binding, Alias: prior.Alias, Kind: prior.Kind,
		Generation: prior.Generation + 1, Removed: true, Host: prior.Host, Release: prior.Release,
	}
	if err := withdrawal.Validate(); err != nil {
		return nil, err
	}
	return withdrawal, nil
}

// signManifestSet signs every settled manifest's canonical bytes and assembles
// its carrier, recording what was delivered. Manifests the signer cannot sign
// are left unsigned only when the publish allows it.
// resolvedSigner is the custody a publish signs with: the one the publish was
// held to, or the environment's. Both document kinds resolve it here, so the
// manifests and the envelope of one publish can never be signed under
// different defaults.
func resolvedSigner(opts *deliveryPublishOptions) signing.Signer {
	if opts.Signer != nil {
		return opts.Signer
	}
	return signing.FromEnvironment(nil)
}

func signManifestSet(ctx context.Context, settled map[string]deliveredManifest, names []string, opts *deliveryPublishOptions, environment string) (*InventoryDelivery, error) {
	signer := resolvedSigner(opts)
	delivery := &InventoryDelivery{Signed: true}
	var unsigned []string
	var unsignedCause error
	if opts.Carriers == nil {
		opts.Carriers = map[string][]byte{}
	}
	for _, binding := range names {
		entry := settled[binding]
		key := carrierKey(deliveredManifestKind, binding, entry.manifest.Generation)
		payload, err := solutionhost.SignedPayload(entry.manifest)
		if err != nil {
			return nil, fmt.Errorf("canonicalize module manifest %s: %w", binding, err)
		}
		identity, unsignedErr, err := signCarrier(ctx, "module manifest "+binding, payload, &entry.carrier, opts.Reuse[key], signer, opts)
		if err != nil {
			return nil, err
		}
		if unsignedErr != nil {
			unsigned = append(unsigned, binding)
			delivery.Signed = false
			if unsignedCause == nil {
				unsignedCause = unsignedErr
			}
		}
		if len(entry.carrier) > 0 {
			opts.Carriers[key] = entry.carrier
		}
		if delivery.Identity == "" {
			delivery.Identity = identity
		}
		digest, err := entry.manifest.Digest()
		if err != nil {
			return nil, err
		}
		delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
			Kind: deliveredManifestKind, ID: binding, Generation: entry.manifest.Generation, Removed: entry.manifest.Removed, Digest: digest,
		})
		settled[binding] = entry
	}
	if len(unsigned) > 0 && !opts.AllowUnsigned {
		return nil, unsignedRefusal("the module manifests "+strings.Join(unsigned, ", "), environment, unsignedCause, opts)
	}
	return delivery, nil
}

// signCarrier settles what carries one document's canonical payload: the
// carrier delivered before or planned (reuseCarrier), or a fresh signature,
// checked as the host would check it before it is kept. A signer with no
// identity leaves the document uncarried and answers why in unsigned, for the
// caller to allow (local qualification) or refuse by name.
func signCarrier(ctx context.Context, what string, payload []byte, carrier *[]byte, planned []byte, signer signing.Signer, opts *deliveryPublishOptions) (identity string, unsigned, err error) {
	identity, reused, err := reuseCarrier(ctx, what, carrier, planned, payload, opts)
	if err != nil || reused {
		return identity, nil, err
	}
	envelope, err := signing.SignEnvelope(payload, signer)
	switch {
	case errors.Is(err, signing.ErrNoIdentity):
		return "", err, nil
	case err != nil:
		return "", nil, fmt.Errorf("sign %s: %w", what, err)
	}
	if opts.SelfCheck != nil {
		if err = opts.SelfCheck(ctx, envelope, payload); err != nil {
			return "", nil, fmt.Errorf("%s: %w", what, err)
		}
	}
	signed, err := solutionhost.Carrier(payload, envelope)
	if err != nil {
		return "", nil, fmt.Errorf("assemble the signed carrier of %s: %w", what, err)
	}
	if *carrier, err = solutionhost.MarshalSigned(signed); err != nil {
		return "", nil, fmt.Errorf("encode the signed carrier of %s: %w", what, err)
	}
	if read, readErr := signing.ReadIdentity(envelope); readErr == nil {
		identity = read.KeyID
	}
	return identity, nil, nil
}

// writeManifestSet writes the settled manifests, the delivery account and the
// Job that POSTs every signed carrier, and returns the overlay's file list.
func writeManifestSet(target, overlay string, inventory *Inventory, settled map[string]deliveredManifest, names []string, opts *deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create delivery overlay: %w", err)
	}
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	var files []string
	var documents []deliveryDocument
	var carriers [][]byte
	for _, binding := range names {
		entry := settled[binding]
		file, writeErr := writeModuleManifestInTree(tree, directory, inventory.Namespace, entry.manifest, entry.carrier)
		if writeErr != nil {
			return nil, writeErr
		}
		files = append(files, file)
		if len(entry.carrier) > 0 {
			documents = append(documents, deliveryDocument{
				ConfigMap: moduleManifestConfigMapName(binding), Key: solutionhost.SignedFileName, Name: binding + ".json",
			})
			carriers = append(carriers, entry.carrier)
		}
	}
	if opts.Target == nil {
		return files, nil
	}
	account, err := renderDeliveryServiceAccountInTree(tree, directory, inventory.Namespace)
	if err != nil {
		return nil, err
	}
	files = append(files, account)
	if len(documents) > 0 {
		job, err := renderDeliveryJob(directory, deliveryJobName(deliveryManifest, inventory.Module, inventory.Environment, carriers), inventory.Namespace,
			deliveryManifest, manifestDeliveryPath, opts.Target, documents)
		if err != nil {
			return nil, err
		}
		files = append(files, job)
	}
	return files, nil
}

// renderedManifests reads the manifest ConfigMaps a render wrote into its
// overlay, keyed by binding.
func renderedManifests(directory string) (map[string]deliveredManifest, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the rendered module manifests: %w", err)
	}
	rendered := map[string]deliveredManifest{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == kustomizationFile {
			continue
		}
		data, err := readWithin(directory, entry.Name())
		if err != nil {
			return nil, err
		}
		manifest, _, held, err := manifestFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if held {
			rendered[manifest.Binding] = deliveredManifest{manifest: manifest}
		}
	}
	return rendered, nil
}

// priorDeliveredManifests reads the manifests the base branch delivers under
// the overlay path, keyed by binding, each with the carrier it was delivered
// as.
func priorDeliveredManifests(ctx context.Context, repo, baseBranch, overlayPath string) (map[string]deliveredManifest, error) {
	ref := "refs/remotes/origin/" + baseBranch
	files, err := deliveredOverlayFiles(ctx, repo, baseBranch, overlayPath)
	if err != nil {
		return nil, err
	}
	prior := map[string]deliveredManifest{}
	for _, name := range files {
		data, err := gitCommandBytes(ctx, repo, "show", ref+":"+overlayPath+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read the delivered %s from %s: %w", name, baseBranch, err)
		}
		manifest, carrier, held, err := manifestFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("the delivered %s on %s cannot be read, so no generation can be settled against it: %w", name, baseBranch, err)
		}
		if held {
			prior[manifest.Binding] = deliveredManifest{manifest: manifest, carrier: carrier}
		}
	}
	return prior, nil
}

// carrierKey names one delivered document's carrier: its kind, ID and
// generation, which is what a carrier signs.
func carrierKey(kind, id string, generation uint64) string {
	return kind + ":" + id + ":" + strconv.FormatUint(generation, 10)
}

// verifyReusedCarrier holds a carrier signed before this publish — by the
// base branch's release or by the plan being executed — to the document it
// is about to carry: it must parse, and its signed bytes must be exactly the
// document's canonical bytes; either failing refuses the publish, since the
// record and its carrier disagree. Whether it still passes the release policy
// decides reuse only: a carrier the policy no longer admits (signed by a
// key that rotated, or by a key the policy never admitted) is
// re-signed by this release rather than delivered. It answers the signer
// identity the carrier names, when it names one.
func verifyReusedCarrier(ctx context.Context, what string, carrier, payload []byte, opts *deliveryPublishOptions) (identity string, verified bool, err error) {
	signed, err := solutionhost.ParseSigned(carrier)
	if err != nil {
		return "", false, fmt.Errorf("the carrier delivered before for %s cannot be read, so it is not reused: %w", what, err)
	}
	if !bytes.Equal(signed.Document, payload) {
		return "", false, fmt.Errorf("the carrier delivered before for %s signs other bytes than the document being delivered now; the delivered tree and its carrier disagree, so neither is trusted", what)
	}
	if opts.ReuseCheck != nil {
		if err = opts.ReuseCheck(ctx, signed.Envelope, payload); err != nil {
			return "", false, nil
		}
	}
	named, err := signing.ReadIdentity(signed.Envelope)
	if err != nil {
		return "", true, nil
	}
	return named.KeyID, true, nil
}

// reuseCarrier decides what carries a settled document before any signature
// is produced: the carrier the base branch delivered, then the one the
// inspected plan signed, each held to the document and to the release policy.
// A local publish ignores the base branch's carrier (it delivers what its own
// signer gives). When neither carries it, the publish that executes a plan
// refuses rather than sign what the plan compared without, and a plan whose
// delivered carrier the policy no longer admits signs again only when told to
// (Resign): a signature is produced once, deliberately, and reused after.
// The chosen carrier is left in *carrier; reused reports whether one was.
func reuseCarrier(ctx context.Context, what string, carrier *[]byte, planned, payload []byte, opts *deliveryPublishOptions) (identity string, reused bool, err error) {
	delivered := *carrier
	if opts.AllowUnsigned {
		delivered = nil
	}
	// The plan's carrier is held to the release policy AS IT IS NOW, like a
	// delivered one: a plan records reused carriers as well as fresh
	// signatures, and a policy tightened between the plan and the publish
	// must reach both. A plan the policy no longer admits is stale, and the
	// publish refuses it rather than signing anything.
	planChecked := *opts
	for _, candidate := range []struct {
		carrier []byte
		under   *deliveryPublishOptions
	}{{delivered, opts}, {planned, &planChecked}} {
		if len(candidate.carrier) == 0 {
			continue
		}
		identity, verified, verifyErr := verifyReusedCarrier(ctx, what, candidate.carrier, payload, candidate.under)
		if verifyErr != nil {
			return "", false, verifyErr
		}
		if verified {
			*carrier = candidate.carrier
			return identity, true, nil
		}
	}
	*carrier = nil
	switch {
	case opts.Executing && !opts.AllowUnsigned:
		return "", false, fmt.Errorf("%s has no carrier the release policy admits now — not the one delivered, not the one the inspected plan carries; the plan is stale under the policy as it is, and a publish signs nothing its plan did not: plan again", what)
	case len(delivered) > 0 && !opts.Resign:
		return "", false, fmt.Errorf("%s was signed by an identity the release policy no longer admits; signing it again is deliberate: plan and publish with --resign", what)
	}
	return "", false, nil
}

// deliveredOverlayFiles lists the YAML files an overlay holds on the base
// branch. "Nothing delivered" is answered only by a base branch that exists
// and holds no such path — the ordinary first publish — never by a listing
// that failed for any other reason: an unreadable history read as empty would
// restart the generation at 1 and lose every tombstone.
func deliveredOverlayFiles(ctx context.Context, repo, baseBranch, overlayPath string) ([]string, error) {
	ref := "refs/remotes/origin/" + baseBranch
	if _, err := gitCommand(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return nil, fmt.Errorf("the base branch %s is not available in the publication checkout (%s), so what it delivers cannot be read and no generation can be settled: %w", baseBranch, ref, err)
	}
	if _, err := gitCommand(ctx, repo, "rev-parse", "--verify", "--quiet", ref+":"+overlayPath); err != nil {
		// The branch exists and the path is not on it: nothing was delivered
		// there. (--verify --quiet fails only for an unresolvable object.)
		return nil, nil
	}
	listing, err := gitCommand(ctx, repo, "ls-tree", "--name-only", ref+":"+overlayPath)
	if err != nil {
		return nil, fmt.Errorf("list what %s delivers under %s: %w", baseBranch, overlayPath, err)
	}
	var files []string
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, ".yaml") || name == kustomizationFile {
			continue
		}
		files = append(files, name)
	}
	return files, nil
}
