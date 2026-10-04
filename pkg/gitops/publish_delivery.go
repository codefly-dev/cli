package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// Coordinate, Component, TrustDomain and Audience name the host the
	// environment declares at publish; every rendered document, and every
	// workload identity in it, must be stamped with them. Empty when the
	// environment declares no host.
	Coordinate, Component, TrustDomain, Audience string
	// ReuseCheck verifies a carrier signed BEFORE this publish — by the base
	// branch's release, or by the plan this publish executes — under the
	// release policy: the same workflow under any release tag. A carrier it
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
	// Resign lets a plan sign a document again whose delivered carrier the
	// release policy no longer admits — a rotated signing identity — which
	// is deliberate, never implied: without it such a carrier refuses.
	Resign bool
	// Carriers collects every carrier this settlement delivers, keyed by
	// carrierKey, so a plan can hand them to the publish that executes it.
	Carriers map[string][]byte
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
	// Domain is the ownership domain the environment declares at publish. A
	// withdrawal is authored by the domain that delivered the binding: a prior
	// document under another domain is never tombstoned by this publish, since
	// a composition whose domain changed would otherwise sign away everything
	// the old domain delivered the moment it rendered nothing. Empty when the
	// environment declares no host.
	Domain string
	// Module is the module being published.
	Module string
	// SelfCheck verifies each carrier right after it is signed, as a host
	// would — offline, against the trusted root, under the workflow's own
	// identity — so a carrier the host would refuse never leaves the
	// publisher. nil runs no check: a local publish signs nothing, and a test
	// signer produces no bundle a root could verify.
	SelfCheck signing.SelfCheck
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
// refuseUnaddressedDocuments refuses a render that declares documents for a
// host the environment no longer names: nothing would deliver them, and
// signing them would still record them as delivered.
func refuseUnaddressedDocuments(what string, names []string, environment string, opts *deliveryPublishOptions) error {
	if opts.Target != nil {
		return nil
	}
	return fmt.Errorf("the render declares the %s documents %s for %s but the environment names no host now; render again against the environment as it is, or declare the host they are delivered to", what, strings.Join(names, ", "), environment)
}

// refuseAnotherHostsRecord holds what the base branch delivered under this
// environment's name to the host the environment declares now: a document
// delivered to another host coordinate is another host's record, and a
// publish never settles over it — a host change is a new environment, not a
// generation of the old one's bindings.
func refuseAnotherHostsRecord(what, id string, host solutionhost.HostTarget, opts *deliveryPublishOptions) error {
	if opts.Coordinate == "" || (host.Coordinate == opts.Coordinate && host.Component == opts.Component) {
		return nil
	}
	return fmt.Errorf("%s %s was delivered to host %s/%s and this environment delivers to %s/%s now; a host change is a new environment with a delivery path of its own, never a publish over another host's record", what, id, host.Coordinate, host.Component, opts.Coordinate, opts.Component)
}

// refuseMovedDocument holds a rendered document to the host the environment
// declares at publish: the ownership domain, coordinate and component it was
// stamped with must be the ones declared now. A render made before the host
// block changed is refused here, named, rather than signed under a
// declaration it does not describe.
func refuseMovedDocument(what, id string, stamp *hostStamp, opts *deliveryPublishOptions) error {
	if stamp.domain != opts.Domain {
		return fmt.Errorf("%s %s was rendered under ownership domain %q and the environment declares %q now; render again before publishing", what, id, stamp.domain, opts.Domain)
	}
	if opts.Coordinate != "" && (stamp.host.Coordinate != opts.Coordinate || stamp.host.Component != opts.Component) {
		return fmt.Errorf("%s %s was rendered for host %s/%s and the environment declares %s/%s now; render again before publishing", what, id, stamp.host.Coordinate, stamp.host.Component, opts.Coordinate, opts.Component)
	}
	if opts.EnvelopeRevision != 0 && stamp.envelope != opts.EnvelopeRevision {
		return fmt.Errorf("%s %s was rendered against envelope revision %d and the environment declares revision %d now; render again before publishing", what, id, stamp.envelope, opts.EnvelopeRevision)
	}
	if opts.TrustDomain != "" {
		for _, trust := range stamp.trusts {
			if trust != opts.TrustDomain {
				return fmt.Errorf("%s %s was rendered for trust domain %q and the environment declares %q now; render again before publishing", what, id, trust, opts.TrustDomain)
			}
		}
	}
	if opts.Audience != "" {
		for _, audience := range stamp.audiences {
			if audience != opts.Audience {
				return fmt.Errorf("%s %s was rendered for audience %q and the environment declares %q now; render again before publishing", what, id, audience, opts.Audience)
			}
		}
	}
	return nil
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
// workflow identity that rotated, or by a run the policy never admitted) is
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
		if err = opts.ReuseCheck(ctx, signed.Bundle, payload); err != nil {
			return "", false, nil
		}
	}
	named, err := signing.ReadIdentity(signed.Bundle)
	if err != nil {
		return "", true, nil
	}
	return named.Subject + " (" + named.Issuer + ")", true, nil
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
	// The delivered carrier is held to the release policy; the plan's was
	// signed by this release and checked as it was signed, so it is held to
	// the document only.
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

// hostStamp is what a rendered document says about the host it was rendered
// for, held to the environment's declaration at publish.
type hostStamp struct {
	domain   string
	host     solutionhost.HostTarget
	envelope uint64
	// trusts and audiences are what every workload identity of the document
	// is issued under, one entry per workload — each is held, not the first.
	trusts    []string
	audiences []string
}

// presenceStamp reads the host stamp off a presence document: its ownership
// domain, host and envelope revision, and the trust domain and audience each
// of its workload identities is issued under.
func presenceStamp(document *solutionhost.SolutionHostBinding) *hostStamp {
	stamp := &hostStamp{domain: document.OwnershipDomain, host: document.Host, envelope: document.EnvelopeRevision}
	for i := range document.Workloads {
		identity := &document.Workloads[i].Identity
		if trust, ok := strings.CutPrefix(identity.SPIFFEID, "spiffe://"); ok {
			trust, _, _ = strings.Cut(trust, "/")
			stamp.trusts = append(stamp.trusts, trust)
		}
		if identity.Audience != "" {
			stamp.audiences = append(stamp.audiences, identity.Audience)
		}
	}
	return stamp
}

func settlePresenceDelivery(
	ctx context.Context,
	repo, baseBranch, target, targetPath, environment string,
	inventory *Inventory,
	opts *deliveryPublishOptions,
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
	// What the base branch delivered under this environment's name is held
	// to the host declared now before anything else: another host's record
	// is never published over.
	for binding, previous := range prior {
		if err = refuseAnotherHostsRecord("binding", binding, previous.document.Host, opts); err != nil {
			return nil, err
		}
	}
	if len(rendered) > 0 {
		if err = refuseUnaddressedDocuments("binding", sortedBindingNames(rendered), environment, opts); err != nil {
			return nil, err
		}
		for binding, entry := range rendered {
			if err = refuseMovedDocument("binding", binding, presenceStamp(entry.document), opts); err != nil {
				return nil, err
			}
		}
	}
	for binding, previous := range prior {
		if _, present := rendered[binding]; present {
			continue
		}
		if err = refuseForeignWithdrawal("binding", binding, previous.document.OwnershipDomain, opts.Domain); err != nil {
			return nil, err
		}
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
	// The whole set is held at its settled generations against what the base
	// branch delivered, with core's rendered fold: the zero host's checks over
	// the set (a tombstone and a present document never collide), and per
	// document the fold against its applied record — a withdrawn binding
	// presented again, a domain the record was not applied under, a
	// generation behind or rewritten — or the stated absence of one. This is
	// the last check before the host's own, which runs the same fold over the
	// same record from its own store.
	documents := make([]*solutionhost.SolutionHostBinding, 0, len(settled))
	sets := make([]solutionhost.RenderedSet, 0, len(settled))
	for _, binding := range names {
		documents = append(documents, settled[binding].document)
		set := solutionhost.RenderedSet{Document: settled[binding].document}
		if previous, delivered := prior[binding]; delivered {
			if set.Applied, err = solutionhost.AppliedFrom(previous.document); err != nil {
				return nil, fmt.Errorf("record the delivered binding %s: %w", binding, err)
			}
		} else {
			set.FirstRecord = true
		}
		sets = append(sets, set)
	}
	if oneErr := solutionhost.OneDelivery(documents...); oneErr != nil {
		return nil, fmt.Errorf("settled solution host bindings are not one delivery: %w", oneErr)
	}
	if _, admitErr := solutionhost.AdmitRenderedSets(sets...); admitErr != nil {
		if errors.Is(admitErr, solutionhost.ErrTombstoned) {
			// The ID is the handle every other system holds, so the fix is
			// one line away in the composition and is named here.
			return nil, fmt.Errorf("settled solution host bindings are not admissible: %w; a withdrawn binding is never presented again under its ID — a new instance needs a new name, which gives it a new binding ID", admitErr)
		}
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

// refuseForeignWithdrawal refuses to withdraw a document delivered under
// another ownership domain than the one this publish declares. The tombstone
// would be signed by an identity the host may let speak for both domains, so
// agreement among the generated tombstones proves nothing; the scope has to be
// held explicitly, here, against what the environment declares now.
func refuseForeignWithdrawal(what, id, delivered, declared string) error {
	if delivered == declared {
		return nil
	}
	return fmt.Errorf("%w: %s %s was delivered under domain %q and this environment declares %q, so this publish cannot withdraw it; a withdrawal is authored by the domain that delivered the document",
		solutionhost.ErrWrongDomain, what, id, delivered, declared)
}

func sortedBindingNames(documents map[string]deliveredPresenceDocument) []string {
	names := make([]string, 0, len(documents))
	for binding := range documents {
		names = append(names, binding)
	}
	sort.Strings(names)
	return names
}

// settledPresenceSet settles each rendered document against what was
// delivered, and tombstones what was delivered and is no longer rendered.
func settledPresenceSet(rendered map[string]deliveredPresenceDocument, prior map[string]deliveredPresenceDocument) (map[string]deliveredPresenceDocument, error) {
	settled := make(map[string]deliveredPresenceDocument, len(rendered)+len(prior))
	for binding, entry := range rendered {
		previous, delivered := prior[binding]
		if delivered {
			generation, err := settledGeneration(previous.document, entry.document)
			if err != nil {
				return nil, err
			}
			entry.document.Generation = generation
			// Unchanged, the document keeps the carrier it was delivered as:
			// the same bytes, the same signature, no new log entry, and a
			// re-sync that finds nothing to do. Re-signing changed the carrier
			// on every publish and made a no-op promotion impossible.
			if generation == previous.document.Generation {
				entry.carrier = previous.carrier
			}
		} else {
			entry.document.Generation = 1
		}
		settled[binding] = entry
	}
	for binding, previous := range prior {
		if _, present := rendered[binding]; present {
			continue
		}
		tombstone, err := tombstoneOf(previous.document)
		if err != nil {
			return nil, fmt.Errorf("withdraw binding %s: %w", binding, err)
		}
		entry := deliveredPresenceDocument{document: tombstone}
		if previous.document.Removed {
			// A tombstone carried forward verbatim keeps its carrier too.
			entry.carrier = previous.carrier
		}
		settled[binding] = entry
	}
	return settled, nil
}

// signPresenceSet signs every settled document's canonical bytes and assembles
// its carrier, recording what was delivered. Documents the signer cannot sign
// are left unsigned only when the publish allows it.
func signPresenceSet(ctx context.Context, settled map[string]deliveredPresenceDocument, names []string, opts *deliveryPublishOptions, environment string) (*InventoryDelivery, error) {
	signer := opts.Signer
	if signer == nil {
		signer = signing.FromEnvironment(nil)
	}
	delivery := &InventoryDelivery{Signed: true}
	var unsigned []string
	if opts.Carriers == nil {
		opts.Carriers = map[string][]byte{}
	}
	for _, binding := range names {
		entry := settled[binding]
		key := carrierKey(deliveredPresence, binding, entry.document.Generation)
		payload, err := entry.document.CanonicalBytes()
		if err != nil {
			return nil, fmt.Errorf("canonicalize binding %s: %w", binding, err)
		}
		identity, reused, err := reuseCarrier(ctx, "binding "+binding, &entry.carrier, opts.Reuse[key], payload, opts)
		if err != nil {
			return nil, err
		}
		if reused {
			if delivery.Identity == "" {
				delivery.Identity = identity
			}
			digest, digestErr := entry.document.Digest()
			if digestErr != nil {
				return nil, digestErr
			}
			delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
				Kind: deliveredPresence, ID: binding, Generation: entry.document.Generation, Removed: entry.document.Removed, Digest: digest,
			})
			opts.Carriers[key] = entry.carrier
			settled[binding] = entry
			continue
		}
		bundle, signErr := signer.Sign(ctx, payload)
		switch {
		case signErr == nil:
			if opts.SelfCheck != nil {
				if checkErr := opts.SelfCheck(ctx, bundle, payload); checkErr != nil {
					return nil, fmt.Errorf("binding %s: %w", binding, checkErr)
				}
			}
			signed, carrierErr := solutionhost.Carrier(payload, bundle)
			if carrierErr != nil {
				return nil, fmt.Errorf("assemble the signed carrier of binding %s: %w", binding, carrierErr)
			}
			carrier, encodeErr := solutionhost.MarshalSigned(signed)
			if encodeErr != nil {
				return nil, fmt.Errorf("encode the signed carrier of binding %s: %w", binding, encodeErr)
			}
			entry.carrier = carrier
			opts.Carriers[key] = carrier
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
func writePresenceSet(target, overlay string, inventory *Inventory, settled map[string]deliveredPresenceDocument, names []string, opts *deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create delivery overlay: %w", err)
	}
	var files []string
	var jobDocuments []deliveryDocument
	var carriers [][]byte
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
			carriers = append(carriers, entry.carrier)
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
		job, err := renderDeliveryJob(directory, deliveryJobName(deliveryPresence, inventory.Module, inventory.Environment, carriers), inventory.Namespace,
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
	// Only the NUMBER is chosen here. Whether the document may carry it — a
	// withdrawn binding presented again (terminal), a domain the record was
	// not applied under, a generation behind or rewritten — is core's fold,
	// run over the whole settled set with each document's record below
	// (AdmitRenderedSets); this package no longer restates those rules.
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
		data, err := readWithin(directory, entry.Name())
		if err != nil {
			return nil, err
		}
		document, alias, _, ok, err := bindingFromConfigMap(data)
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

// bindingFromConfigMap parses the document out of a ConfigMap carrying one,
// with its signed carrier when it has one, and reports false for any other
// manifest in the overlay (the Job, the account). A presence ConfigMap that
// lost its document key is a corrupt carrier, not another manifest.
func bindingFromConfigMap(data []byte) (*solutionhost.SolutionHostBinding, string, []byte, bool, error) {
	var carrier solutionHostBindingConfigMap
	if err := yaml.Unmarshal(data, &carrier); err != nil {
		return nil, "", nil, false, fmt.Errorf("decode: %w", err)
	}
	if carrier.Kind != kindConfigMap {
		return nil, "", nil, false, nil
	}
	encoded, held := carrier.Data[solutionhost.FileName]
	if !held {
		if carrier.Metadata.Labels[solutionHostBindingLabel] == solutionHostBindingKind {
			return nil, "", nil, false, fmt.Errorf("the delivered ConfigMap %s is labelled a presence carrier and holds no %s", carrier.Metadata.Name, solutionhost.FileName)
		}
		return nil, "", nil, false, nil
	}
	document, err := solutionhost.Parse([]byte(encoded))
	if err != nil {
		return nil, "", nil, false, fmt.Errorf("the delivered document is not one this Core reads: %w", err)
	}
	var signed []byte
	if raw, present := carrier.Data[presenceCarrierKey]; present {
		signed = []byte(raw)
	}
	return document, carrier.Metadata.Labels[solutionLabel], signed, true, nil
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

// priorDeliveredBindings reads the presence documents the base branch delivers
// under the overlay path, keyed by binding ID, each with the carrier it was
// delivered as.
func priorDeliveredBindings(ctx context.Context, repo, baseBranch, overlayPath string) (map[string]deliveredPresenceDocument, error) {
	ref := "refs/remotes/origin/" + baseBranch
	files, err := deliveredOverlayFiles(ctx, repo, baseBranch, overlayPath)
	if err != nil {
		return nil, err
	}
	prior := map[string]deliveredPresenceDocument{}
	for _, name := range files {
		data, err := gitCommandBytes(ctx, repo, "show", ref+":"+overlayPath+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read the delivered %s from %s: %w", name, baseBranch, err)
		}
		document, alias, carrier, ok, err := bindingFromConfigMap(data)
		if err != nil {
			return nil, fmt.Errorf("the delivered %s on %s cannot be read, so no generation can be settled against it: %w", name, baseBranch, err)
		}
		if !ok {
			continue
		}
		prior[document.Binding] = deliveredPresenceDocument{document: document, alias: alias, carrier: carrier}
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
	if err := os.WriteFile(filepath.Join(directory, file), body, 0o600); err != nil {
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
	opts *deliveryPublishOptions,
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
	for authority, previous := range prior {
		if err = refuseAnotherHostsRecord("authority", authority, previous.document.Host, opts); err != nil {
			return nil, err
		}
	}
	if len(rendered) > 0 {
		declared := make([]string, 0, len(rendered))
		for authority := range rendered {
			declared = append(declared, authority)
		}
		sort.Strings(declared)
		if err = refuseUnaddressedDocuments("authority", declared, environment, opts); err != nil {
			return nil, err
		}
		for authority, entry := range rendered {
			if err = refuseMovedDocument("authority", authority, &hostStamp{domain: entry.document.OwnershipDomain, host: entry.document.Host, envelope: entry.document.EnvelopeRevision}, opts); err != nil {
				return nil, err
			}
		}
	}
	for authority, previous := range prior {
		if _, present := rendered[authority]; present {
			continue
		}
		if err = refuseForeignWithdrawal("authority", authority, previous.document.OwnershipDomain, opts.Domain); err != nil {
			return nil, err
		}
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
	// The presence half's applied record comes from the same place the
	// authority half's does — what the base branch delivered — because that is
	// the record the host folds the pair against, and the one publish can see.
	priorPresence, err := priorDeliveredBindings(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, solutionHostBindingOverlay(environment))))
	if err != nil {
		return nil, err
	}
	if err = refuseUnmatchedPairs(target, environment, inventory.Module, settled, names, prior, priorPresence, opts.EnvelopeRevision); err != nil {
		return nil, err
	}
	delivery, err := signAuthoritySet(ctx, settled, names, opts, environment)
	if err != nil {
		return nil, err
	}
	ledger, err := holdBindingLedger(ctx, repo, baseBranch, filepath.ToSlash(filepath.Join(targetPath, overlay)), settled, names)
	if err != nil {
		return nil, err
	}
	files, err := writeAuthoritySet(target, overlay, inventory, settled, names, opts)
	if err != nil {
		return nil, err
	}
	if err = writeAuthorityKustomization(target, environment, files); err != nil {
		return nil, err
	}
	if err = recordBindingLedger(filepath.Join(target, filepath.FromSlash(overlay)), ledger, settled, names); err != nil {
		return nil, err
	}
	inventory.SolutionAuthorityPath = solutionAuthorityDir
	return delivery, nil
}

// refuseUnmatchedPairs holds every settled authority document against the
// presence document it is granted over — the one this publish just wrote to the
// staged tree — with core's ActivateRendered: the renderer's share of
// activation, as AdmitRenderedSets is its share of admission. It answers what needs
// no host state: the fold of BOTH halves against what the base branch delivered
// (a domain or binding either half would migrate across, a withdrawal, a
// rewritten generation), the target binding, host and domain agreeing, both
// halves naming the envelope revision the environment declares NOW, the
// approved build being one the presence says the binding runs, and the
// effective-from generation. What it cannot answer stays the host's: who signed
// either half, and whether the authority fits the ceiling — the envelope is the
// host's record and a renderer holds only its revision, so a RenderedMatch is
// not an Activation.
//
// Each half's applied record is the base branch's, and its absence is stated
// rather than left to a zero value: core refuses a fold that was given neither
// a record nor the statement that there is none, so a publish that forgot to
// look is refused instead of passing as a first delivery.
func refuseUnmatchedPairs(
	target, environment, module string,
	settled map[string]deliveredAuthorityDocument,
	names []string,
	prior map[string]deliveredAuthorityDocument,
	priorPresence map[string]deliveredPresenceDocument,
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
		request := solutionhost.RenderedActivationRequest{
			Authority:        entry.document,
			Presence:         over.document,
			Build:            entry.document.ApprovedBuild,
			EnvelopeRevision: envelopeRevision,
		}
		// The authority record is looked up by the binding the authority is
		// granted over, not by its ID, because that is how the host folds: a
		// withdrawal over a binding is terminal for every later authority ID
		// over it, and a record found by ID would never see one.
		previous, delivered, err := priorAuthorityOver(prior, binding)
		if err != nil {
			return err
		}
		if delivered {
			if request.Applied, err = solutionhost.AppliedAuthorityFrom(previous.document); err != nil {
				return fmt.Errorf("record the delivered authority %s: %w", previous.document.Authority, err)
			}
		} else {
			request.FirstAuthorityRecord = true
		}
		if applied, present := priorPresence[binding]; present {
			if request.AppliedPresence, err = solutionhost.AppliedFrom(applied.document); err != nil {
				return fmt.Errorf("record the delivered binding %s: %w", binding, err)
			}
		} else {
			request.FirstPresenceRecord = true
		}
		if _, err := solutionhost.ActivateRendered(request); err != nil {
			return fmt.Errorf("authority %s and binding %s are not a pair the host would activate: %w", authority, binding, err)
		}
	}
	return nil
}

// priorAuthorityOver is the authority document the base branch delivered over
// a binding, if any. More than one is a tree no host could hold a record for,
// since the host keeps one authority record per binding, and is refused rather
// than folded against whichever came first. The render never writes two, so
// this is a hand-edited tree; and since a withdrawal is terminal for the
// binding, the way out is not to withdraw one but to grant over a new binding.
func priorAuthorityOver(prior map[string]deliveredAuthorityDocument, binding string) (deliveredAuthorityDocument, bool, error) {
	var found []string
	for authority, entry := range prior {
		if entry.document.PresenceBinding == binding {
			found = append(found, authority)
		}
	}
	switch len(found) {
	case 0:
		return deliveredAuthorityDocument{}, false, nil
	case 1:
		return prior[found[0]], true, nil
	default:
		sort.Strings(found)
		return deliveredAuthorityDocument{}, false, fmt.Errorf("the base branch delivers the authority documents %s over binding %s, and a host holds one authority record per binding, so nothing settles against them; a new grant needs a new instance, which gives it a new binding ID", strings.Join(found, ", "), binding)
	}
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
		binding := entry.document.PresenceBinding
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
			// Unchanged: delivered as the carrier it already has, unsigned anew.
			entry.carrier = previous.carrier
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
		entry := deliveredAuthorityDocument{document: &tombstone, module: previous.module}
		if previous.document.Removed {
			entry.carrier = previous.carrier
		} else {
			tombstone.Generation = previous.document.Generation + 1
			tombstone.Removed = true
			tombstone.ApprovedBuild, tombstone.EffectiveFrom, tombstone.Principals = "", 0, nil
			if err := tombstone.Validate(); err != nil {
				return nil, fmt.Errorf("withdraw authority %s: %w", authority, err)
			}
		}
		settled[authority] = entry
	}
	return settled, nil
}

// refuseUnbumpedBindings refuses an authority document in which a binding's
// revision moved backwards, or its content changed while its revision did not.
// The revision is what a sealed credential is held against: a change that
// keeps it is a change no credential can detect, a revision that decreases
// gives an old number a new meaning, and the author remembering to bump it is
// not enforcement.
func refuseUnbumpedBindings(prior, candidate *solutionhost.AuthorityDocument) error {
	previous := map[string]solutionhost.AuthorityBinding{}
	for _, principal := range prior.Principals {
		for _, binding := range principal.Bindings {
			previous[binding.ID] = binding
		}
	}
	var unbumped, rewound []string
	for _, principal := range candidate.Principals {
		for _, binding := range principal.Bindings {
			before, delivered := previous[binding.ID]
			if !delivered {
				continue
			}
			if binding.Revision < before.Revision {
				rewound = append(rewound, fmt.Sprintf("%s (%d, delivered at %d)", binding.ID, binding.Revision, before.Revision))
				continue
			}
			if before.Revision != binding.Revision {
				continue
			}
			if before.Audience != binding.Audience || before.Scope != binding.Scope || before.Queue != binding.Queue || before.Namespace != binding.Namespace {
				unbumped = append(unbumped, binding.ID)
			}
		}
	}
	if len(rewound) > 0 {
		sort.Strings(rewound)
		return fmt.Errorf(
			"authority %s moves the revision of the bindings %s backwards; a binding's revision only increases, since a credential sealed to a revision holds it against the live one and an old number must not acquire a new meaning",
			candidate.Authority, strings.Join(rewound, ", "))
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
func signAuthoritySet(ctx context.Context, settled map[string]deliveredAuthorityDocument, names []string, opts *deliveryPublishOptions, environment string) (*InventoryDelivery, error) {
	signer := opts.Signer
	if signer == nil {
		signer = signing.FromEnvironment(nil)
	}
	delivery := &InventoryDelivery{Signed: true}
	var unsigned []string
	if opts.Carriers == nil {
		opts.Carriers = map[string][]byte{}
	}
	for _, authority := range names {
		entry := settled[authority]
		key := carrierKey(deliveredAuthority, authority, entry.document.Generation)
		payload, err := solutionhost.SignedPayloadFor(entry.document)
		if err != nil {
			return nil, fmt.Errorf("canonicalize authority %s: %w", authority, err)
		}
		identity, reused, err := reuseCarrier(ctx, "authority "+authority, &entry.carrier, opts.Reuse[key], payload, opts)
		if err != nil {
			return nil, err
		}
		if reused {
			if delivery.Identity == "" {
				delivery.Identity = identity
			}
			digest, digestErr := entry.document.Digest()
			if digestErr != nil {
				return nil, digestErr
			}
			delivery.Documents = append(delivery.Documents, InventoryDeliveredDocument{
				Kind: deliveredAuthority, ID: authority, Generation: entry.document.Generation, Removed: entry.document.Removed, Digest: digest,
			})
			opts.Carriers[key] = entry.carrier
			settled[authority] = entry
			continue
		}
		bundle, signErr := signer.Sign(ctx, payload)
		switch {
		case signErr == nil:
			if opts.SelfCheck != nil {
				if checkErr := opts.SelfCheck(ctx, bundle, payload); checkErr != nil {
					return nil, fmt.Errorf("authority %s: %w", authority, checkErr)
				}
			}
			signed, carrierErr := solutionhost.Carrier(payload, bundle)
			if carrierErr != nil {
				return nil, fmt.Errorf("assemble the signed carrier of authority %s: %w", authority, carrierErr)
			}
			carrier, encodeErr := solutionhost.MarshalSigned(signed)
			if encodeErr != nil {
				return nil, fmt.Errorf("encode the signed carrier of authority %s: %w", authority, encodeErr)
			}
			entry.carrier = carrier
			opts.Carriers[key] = carrier
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
func writeAuthoritySet(target, overlay string, inventory *Inventory, settled map[string]deliveredAuthorityDocument, names []string, opts *deliveryPublishOptions) ([]string, error) {
	directory := filepath.Join(target, filepath.FromSlash(overlay))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create authority overlay: %w", err)
	}
	var files []string
	var jobDocuments []deliveryDocument
	var carriers [][]byte
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
			carriers = append(carriers, entry.carrier)
		}
	}
	if opts.Target == nil || len(jobDocuments) == 0 {
		return files, nil
	}
	job, err := renderDeliveryJob(directory, deliveryJobName(deliveryAuthority, inventory.Module, inventory.Environment, carriers), authorityNamespace,
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
		data, err := readWithin(directory, entry.Name())
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
		if carrier.Metadata.Labels[solutionHostBindingLabel] == solutionAuthorityKind {
			return nil, "", false, fmt.Errorf("the delivered ConfigMap %s is labelled an authority carrier and holds no %s", carrier.Metadata.Name, solutionhost.AuthorityFileName)
		}
		return nil, "", false, nil
	}
	document, err := solutionhost.ParseAuthority([]byte(encoded))
	if err != nil {
		return nil, "", false, fmt.Errorf("the delivered authority document is not one this Core reads: %w", err)
	}
	return document, carrier.Metadata.Labels[solutionLabel], true, nil
}

// authorityCarrierOf reads the signed carrier out of a delivered authority
// ConfigMap, nil when it was delivered unsigned.
func authorityCarrierOf(data []byte) []byte {
	var carrier solutionAuthorityConfigMap
	if err := yaml.Unmarshal(data, &carrier); err != nil {
		return nil
	}
	if raw, present := carrier.Data[authorityCarrierKey]; present {
		return []byte(raw)
	}
	return nil
}

// priorDeliveredAuthorities reads the authority documents the base branch
// delivers under the overlay path, keyed by authority ID.
// bindingLedgerFile keeps, beside the delivered authority documents, the
// highest revision every operation binding ID was ever delivered at under
// this environment, with the meaning it carried then. The documents hold only
// what is granted NOW, and a withdrawal is terminal for an authority, not for
// a binding ID: a binding removed from one generation and reintroduced later
// would otherwise be new to the guard and free to come back at a lower
// revision — or at the same revision with another meaning — which is the
// rewind a credential sealed to a revision must never meet. The ledger is the
// publisher's own record under the repository's trust, like the cell; nothing
// signs it, no Job reads it, and no reader of the overlay's YAML sees it.
const bindingLedgerFile = "bindings.ledger"

type ledgerEntry struct {
	Authority string `json:"authority"`
	Revision  uint64 `json:"revision"`
	Audience  string `json:"audience"`
	Scope     string `json:"scope"`
	Queue     string `json:"queue"`
	Namespace string `json:"namespace"`
}

type bindingLedger map[string]ledgerEntry

// priorBindingLedger reads the ledger the base branch delivered; a base with
// none is an empty ledger, a base whose ledger cannot be read is an error.
func priorBindingLedger(ctx context.Context, repo, baseBranch, overlayPath string) (bindingLedger, error) {
	data, err := gitCommandBytes(ctx, repo, "show", "refs/remotes/origin/"+baseBranch+":"+overlayPath+"/"+bindingLedgerFile)
	if err != nil {
		if gitSaysAbsent(err) {
			return bindingLedger{}, nil
		}
		return nil, fmt.Errorf("read the delivered binding ledger from %s: %w", baseBranch, err)
	}
	ledger := bindingLedger{}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("the delivered binding ledger on %s cannot be read, so no binding revision can be held against it: %w", baseBranch, err)
	}
	return ledger, nil
}

// refuseRewinds holds every binding a document grants to the ledger: a
// revision below the mark, or at the mark with another meaning, is refused
// whether or not the binding is in the document delivered just before.
func (ledger bindingLedger) refuseRewinds(document *solutionhost.AuthorityDocument) error {
	var rewound, remeant []string
	for _, principal := range document.Principals {
		for _, binding := range principal.Bindings {
			mark, known := ledger[binding.ID]
			if !known {
				continue
			}
			switch {
			case binding.Revision < mark.Revision:
				rewound = append(rewound, fmt.Sprintf("%s (%d, delivered at %d under %s)", binding.ID, binding.Revision, mark.Revision, mark.Authority))
			case binding.Revision == mark.Revision && (binding.Audience != mark.Audience || binding.Scope != mark.Scope || binding.Queue != mark.Queue || binding.Namespace != mark.Namespace):
				remeant = append(remeant, fmt.Sprintf("%s (revision %d under %s)", binding.ID, binding.Revision, mark.Authority))
			}
		}
	}
	sort.Strings(rewound)
	sort.Strings(remeant)
	switch {
	case len(rewound) > 0:
		return fmt.Errorf("authority %s reintroduces the bindings %s below the revision they were delivered at; a binding's revision only increases across its whole history under this environment, removal and reintroduction included, since a credential sealed to a revision holds it against the live one",
			document.Authority, strings.Join(rewound, ", "))
	case len(remeant) > 0:
		return fmt.Errorf("authority %s gives the bindings %s another meaning at the revision they were delivered with; a change of audience, scope, queue or namespace is a new revision",
			document.Authority, strings.Join(remeant, ", "))
	}
	return nil
}

// record raises the ledger's mark for every binding a live document grants.
func (ledger bindingLedger) record(document *solutionhost.AuthorityDocument) {
	for _, principal := range document.Principals {
		for _, binding := range principal.Bindings {
			if mark, known := ledger[binding.ID]; known && mark.Revision > binding.Revision {
				continue
			}
			ledger[binding.ID] = ledgerEntry{Authority: document.Authority, Revision: binding.Revision, Audience: binding.Audience, Scope: binding.Scope, Queue: binding.Queue, Namespace: binding.Namespace}
		}
	}
}

// holdBindingLedger reads the ledger the base branch delivered and holds every
// live settled document's bindings to it.
func holdBindingLedger(ctx context.Context, repo, baseBranch, overlayPath string, settled map[string]deliveredAuthorityDocument, names []string) (bindingLedger, error) {
	ledger, err := priorBindingLedger(ctx, repo, baseBranch, overlayPath)
	if err != nil {
		return nil, err
	}
	for _, authority := range names {
		if entry := settled[authority]; !entry.document.Removed {
			if err := ledger.refuseRewinds(entry.document); err != nil {
				return nil, err
			}
		}
	}
	return ledger, nil
}

// recordBindingLedger raises the ledger's marks for every live document and
// writes it beside the overlay it travels with; a withdrawn document leaves
// its marks standing.
func recordBindingLedger(directory string, ledger bindingLedger, settled map[string]deliveredAuthorityDocument, names []string) error {
	for _, authority := range names {
		if entry := settled[authority]; !entry.document.Removed {
			ledger.record(entry.document)
		}
	}
	return writeBindingLedger(directory, ledger)
}

func writeBindingLedger(directory string, ledger bindingLedger) error {
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the binding ledger: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, bindingLedgerFile), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the binding ledger: %w", err)
	}
	return nil
}

func priorDeliveredAuthorities(ctx context.Context, repo, baseBranch, overlayPath string) (map[string]deliveredAuthorityDocument, error) {
	ref := "refs/remotes/origin/" + baseBranch
	files, err := deliveredOverlayFiles(ctx, repo, baseBranch, overlayPath)
	if err != nil {
		return nil, err
	}
	prior := map[string]deliveredAuthorityDocument{}
	for _, name := range files {
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
		prior[document.Authority] = deliveredAuthorityDocument{document: document, module: module, carrier: authorityCarrierOf(data)}
	}
	return prior, nil
}
