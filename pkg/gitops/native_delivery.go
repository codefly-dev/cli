package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	runcomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/workloadidentity"
	"google.golang.org/grpc/codes"
)

// Native delivery: a laptop run delivers to the host what a cell's delivery
// delivers — the same module manifest, signed with the developer key, carried
// with a delivery credential the run issues through the codefly.dev/composition
// interface the host declares — so the host's registry, mint and installation
// run their one code path. What differs is where the manifest's facts come
// from: its workloads are the identity services this run started, each with
// the account a cell would give it, and its endpoints the addresses this run
// allocated.

// NativeIdentity is one workload a native run issues a credential for and
// lists in its module's manifest: a service that declares module-identity,
// the account it would run as in a cell, and the SPIFFE ID the cell's mesh
// issues to that account, which the manifest names.
type NativeIdentity struct {
	Module  string
	Service string
}

// Workload is the identity as the host and the run name it, under trustDomain.
func (identity NativeIdentity) Workload(trustDomain string) workloadidentity.Workload {
	return workloadidentity.Workload{TrustDomain: trustDomain, Module: identity.Module, Service: identity.Service}
}

// ResolveNativeIdentities plans the identities a native run projects, by the
// rule a cell's render names them: every module-identity service of the
// composition, each in its module's namespace. A native run has no cluster
// to require.
func ResolveNativeIdentities(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string]NativeIdentity, error) {
	if env.Host == nil {
		return nil, fmt.Errorf("environment %s declares no host", env.Name)
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	identities := map[string]NativeIdentity{}
	for _, ref := range refs {
		service, loadErr := workspace.LoadService(ctx, ref)
		if loadErr != nil {
			return nil, loadErr
		}
		if !service.ModuleIdentity {
			continue
		}
		identities[resources.ServiceUnique(ref.Module, ref.Name)] = NativeIdentity{Module: ref.Module, Service: ref.Name}
	}
	return identities, nil
}

// NativeWorkload is one module a native run delivers a manifest for: the
// module, its services, its started identity workloads, and their routed
// endpoints.
type NativeWorkload struct {
	Module   *resources.Module
	Services []*resources.Service
	// Started are the identity workloads whose processes run: the ones the
	// manifest lists.
	Started []NativeIdentity
	// Carriers are the addresses of the module's routed endpoints, keyed as
	// the gateway reads them.
	Carriers map[string]string
	// FrontendBundle is the identity of the complete frontend bundle this run
	// built and retained in the workspace's artifact cache: what the
	// manifest's frontend payload names, since what runs is what is
	// delivered. Nil when the component declares no page.
	FrontendBundle *solutionhost.FrontendPayload
}

// NativeDeliverer renders, signs and delivers a native run's manifests.
type NativeDeliverer struct {
	Workspace *resources.Workspace
	Env       *environments.Environment
	Signer    solutionhost.Signer
	// StateDir keeps the generations reserved so far: changed content is the
	// next generation, unchanged content is replayed.
	StateDir string
	// Token issues the delivery account's credential in a namespace.
	Token func(module string) (string, error)
	// Prepare captures transport-owned content before generation reservation.
	// Its Post sends exactly that captured content with the settled carrier:
	// a host that implements codefly.dev/composition receives it through
	// ApplyManifest (CompositionDelivery.Prepare).
	Prepare func(ctx context.Context, module string, removed bool) (*PreparedDelivery, error)
	// Applied answers the host's applied state of a binding, the same way.
	Applied func(ctx context.Context, binding string) (*AppliedManifest, bool, error)
}

// DeliveryPost sends a carrier whose generation has already been settled.
type DeliveryPost func(context.Context, string, *solutionhost.Signed, string) (Receipt, error)

// PreparedDelivery binds generation content to its immutable send operation.
type PreparedDelivery struct {
	Content []byte
	Post    DeliveryPost
}

// Render builds the manifest of a module's started identity workloads,
// by the rules a cell's render follows (moduleInstanceOf,
// ModuleManifestOf), at generation 1.
func (d *NativeDeliverer) Render(ctx context.Context, workload *NativeWorkload) (*solutionhost.ModuleManifest, error) {
	if len(workload.Started) == 0 {
		return nil, fmt.Errorf("module %s has no started identity workload to deliver", workload.Module.Name)
	}
	manifest, err := composition.LoadPackageManifest(workload.Module.Dir())
	if err != nil {
		return nil, fmt.Errorf("module %s declares no package manifest, so it names no release: %w", workload.Module.Name, err)
	}
	instance, undeclared, err := moduleInstanceOf(ctx, d.Workspace, workload.Module, workload.Services, d.Env,
		&InventoryPackage{ID: manifest.ID, Version: manifest.Version}, manifest.ReleaseArtifacts, nil)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, errors.New(undeclared)
	}
	// The bundle this run built outranks the release's artifact metadata: a
	// native run delivers what runs, and the host reads the bytes from the
	// cache by the digest named here. A declared page without it is refused:
	// a host admits no frontend without its signed payload.
	switch {
	case workload.FrontendBundle != nil && instance.Frontend == nil:
		return nil, fmt.Errorf("module %s built a frontend bundle and declares no frontend; a bundle is the output of a declared page", workload.Module.Name)
	case workload.FrontendBundle != nil:
		payload := *workload.FrontendBundle
		instance.Frontend.Payload = &payload
	case instance.Frontend != nil:
		return nil, fmt.Errorf("module %s declares a frontend and this run built no bundle for it; a native delivery names the bundle it built, and a host admits no frontend without its signed payload", workload.Module.Name)
	}
	identities := map[string]bool{}
	for _, identity := range instance.Identities {
		identities[identity.Service] = true
	}
	started := map[string]bool{}
	workloads := make([]solutionhost.Workload, 0, len(workload.Started))
	for _, identity := range workload.Started {
		if identity.Module != workload.Module.Name || !identities[identity.Service] {
			return nil, fmt.Errorf("module %s workload %s/%s does not declare module-identity in this module", workload.Module.Name, identity.Module, identity.Service)
		}
		if started[identity.Service] {
			return nil, fmt.Errorf("module %s lists workload %s more than once", workload.Module.Name, identity.Service)
		}
		started[identity.Service] = true
		// A native workload declares no cell: the run vouches for (module,
		// service) directly, and a manifest names only what the module is.
		workloads = append(workloads, solutionhost.Workload{Service: identity.Service, Container: identity.Service})
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Service < workloads[j].Service })
	// An identity workload this run has not started yet routes nothing yet:
	// its endpoints join a later generation once the workload is started.
	if instance.Endpoints, err = routedEndpoints(workload.Module.Name, workload.Services, workload.Carriers, identities, started); err != nil {
		return nil, err
	}
	binding, err := bindingID(d.Workspace.Name, d.Env.Name, instance.Name)
	if err != nil {
		return nil, err
	}
	return ModuleManifestOf(binding, d.Env.Host, instance, workloads)
}

// nativeGenerations is the run's record of what it reserved: per binding,
// the generation and the content it carried. It outlives a run, so a restart
// that delivers the same content replays its generation.
type nativeGenerations struct {
	Entries map[string]nativeGeneration `json:"entries"`
}

type nativeGeneration struct {
	Generation uint64 `json:"generation"`
	Content    string `json:"content"`
	Removed    bool   `json:"removed,omitempty"`
}

func (d *NativeDeliverer) generationsPath() string {
	return filepath.Join(d.StateDir, "generations.json")
}

func (d *NativeDeliverer) loadGenerations() (*nativeGenerations, error) {
	record := &nativeGenerations{Entries: map[string]nativeGeneration{}}
	data, err := os.ReadFile(d.generationsPath())
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, record); err != nil {
		return nil, fmt.Errorf("read %s: %w", d.generationsPath(), err)
	}
	if record.Entries == nil {
		record.Entries = map[string]nativeGeneration{}
	}
	return record, nil
}

func (d *NativeDeliverer) saveGenerations(ctx context.Context, record *nativeGenerations) error {
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, d.generationsPath(), data, 0o600)
}

// settle assigns a manifest its generation: the one it had when its content is
// unchanged, the next one otherwise. Content includes the canonical Core payload
// at generation 1 and any prepared transport declaration. A delivery after a
// withdrawal is a new generation like any other: whether a withdrawal is
// terminal is the host's to say, since only the host knows whether it still
// holds it (the local authority keeps applied deliveries in memory, so a host
// started again accepts the binding afresh). The record never refuses.
func settle(record *nativeGenerations, key string, content []byte, removed bool) uint64 {
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	entry, known := record.Entries[key]
	if known && entry.Content == digest {
		entry.Removed = removed
		record.Entries[key] = entry
		return entry.Generation
	}
	next := uint64(1)
	if known {
		next = entry.Generation + 1
	}
	record.Entries[key] = nativeGeneration{Generation: next, Content: digest, Removed: removed}
	return next
}

// Receipt is the host's acknowledgement of one delivered manifest: the
// disposition and the generation it holds.
type Receipt struct {
	Disposition string
	Generation  uint64
}

// Deliver settles the manifest's generation, signs it and sends it to the
// host as the delivery account of namespace — the namespace of the manifest's
// workloads, or for a withdrawal the one its binding was delivered in.
func (d *NativeDeliverer) Deliver(ctx context.Context, manifest *solutionhost.ModuleManifest) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if d.Prepare == nil {
		return Receipt{}, errors.New("a native delivery is carried through the host's codefly.dev/composition interface, and none was prepared")
	}
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return Receipt{}, err
	}
	var post DeliveryPost
	// Source preparation and reservation share one short transaction. No
	// credential acquisition, signing or network operation holds this lock.
	err := runcomposition.WithFileLockContext(ctx, d.generationsPath()+".lock", 30*time.Second, func() error {
		record, err := d.loadGenerations()
		if err != nil {
			return err
		}
		manifest.Generation = 1
		content, err := solutionhost.SignedPayload(manifest)
		if err != nil {
			return err
		}
		prepared, err := d.Prepare(ctx, manifest.Alias, manifest.Removed)
		if err != nil {
			return err
		}
		if prepared == nil || prepared.Post == nil {
			return errors.New("delivery preparation supplied no send operation")
		}
		post = prepared.Post
		// Withdrawals retain their Core-only identity: a withdrawal carries no
		// authority declaration.
		if !manifest.Removed {
			content, err = json.Marshal(struct {
				Version      int
				Core, Policy []byte
			}{1, content, prepared.Content})
			if err != nil {
				return err
			}
		}
		manifest.Generation = settle(record, manifest.Binding, content, manifest.Removed)
		if err := ctx.Err(); err != nil {
			return err
		}
		// A lost acknowledgement must never permit different content to
		// reuse this generation, including after process restart.
		return d.saveGenerations(ctx, record)
	})
	if err != nil {
		return Receipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return Receipt{}, err
	}
	payload, err := solutionhost.SignedPayload(manifest)
	if err != nil {
		return Receipt{}, err
	}
	return d.post(ctx, manifest.Alias, payload, post)
}

func (d *NativeDeliverer) post(ctx context.Context, module string, payload []byte, post DeliveryPost) (Receipt, error) {
	envelope, err := solutionhost.SignEnvelope(payload, d.Signer)
	if err != nil {
		return Receipt{}, err
	}
	signed, err := solutionhost.Carrier(payload, envelope)
	if err != nil {
		return Receipt{}, err
	}
	token, err := d.Token(module)
	if err != nil {
		return Receipt{}, err
	}
	return post(ctx, module, signed, token)
}

// HostStatusError is a refusal the host answered, with its gRPC code kept, not
// only printed, so the caller that retries can tell a host that is not serving
// yet from one that refused for good (a policy refusal, a stale or mismatched
// document, an interface it does not serve).
type HostStatusError struct {
	Code    codes.Code
	Message string
	// Reason is the refusal's kind when the run recognizes it
	// (solutionhost.ErrWithdrawn), for errors.Is.
	Reason error
}

func (e *HostStatusError) Error() string { return e.Message }

func (e *HostStatusError) Unwrap() error { return e.Reason }

// Transient reports whether the refusal only says the host is not serving
// yet: unavailable, out of time or out of capacity. Every other code is the
// host's answer, and asking again gets the same one.
func (e *HostStatusError) Transient() bool {
	switch e.Code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	}
	return false
}
