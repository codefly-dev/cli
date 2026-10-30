package gitops

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	delivery "github.com/codefly-dev/authority/libraries/delivery/go"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// CompositionDelivery delivers through codefly.dev/composition: the signed
// carrier and the module's delivery credential go to ApplyManifest, which
// answers synchronously with the generation the host holds. The run then knows
// nothing of the host but the interface it declared.
type CompositionDelivery struct {
	client compositionv1.CompositionServiceClient
	// signer is the run's release key, which signs the core manifest too.
	signer solutionhost.Signer
	// sources are the module's own declarations, read where they live. One
	// value because a publish preparing a document for a cell to send reads the
	// very same five (AuthoritySources), and two copies of that list is two
	// ways for the pair the host compares to disagree.
	sources AuthoritySources

	mu      sync.Mutex
	applied map[string]AppliedManifest
}

// NewCompositionDelivery speaks the interface over conn. With each manifest it
// delivers the authority's own document, signed by signer: what each method of
// the module declares, read by methods from the module's package, and the
// actions on each of its own resource kinds, read by ceilings from its module
// contract (core's manifest names the kinds alone), and the roles a solution
// declares, read by roles from its module contract too (core's manifest
// carries none), and its explicit owner-aware declaration, and the
// installation declaration read by installation from that contract — the
// selector and person-context edges, carried verbatim because the paired
// agreement rule compares this document against the Core signed manifest
// byte-for-byte.
func NewCompositionDelivery(conn grpc.ClientConnInterface, signer solutionhost.Signer,
	methods func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error),
	ceilings func(ctx context.Context, module string) ([]*authorityv1.WorkScopeV1, error),
	roles func(ctx context.Context, module string) ([]*compositionv1.RoleV1, error),
	ownerAware func(ctx context.Context, module string) (bool, error),
	installation func(ctx context.Context, module string) (string, []*compositionv1.EdgeDependencyV1, error)) *CompositionDelivery {
	return &CompositionDelivery{
		client: compositionv1.NewCompositionServiceClient(conn), signer: signer,
		sources: AuthoritySources{Methods: methods, Ceilings: ceilings, Roles: roles, OwnerAware: ownerAware, Installation: installation},
		applied: map[string]AppliedManifest{},
	}
}

func (c *CompositionDelivery) post(ctx context.Context, module string, signed *solutionhost.Signed, token string, declaration []byte) (Receipt, error) {
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return Receipt{}, err
	}
	request := &compositionv1.ApplyManifestRequest{
		Carrier:    &compositionv1.SignedManifestV1{Document: signed.Document, Envelope: signed.Envelope},
		Credential: token,
	}
	if request.Authority, err = c.authority(manifest, signed.Document, declaration); err != nil {
		return Receipt{}, err
	}
	out, err := c.client.ApplyManifest(ctx, request)
	if err != nil {
		return Receipt{}, refusal(module, manifest, err)
	}
	if out.GetGeneration() != manifest.Generation {
		return Receipt{}, fmt.Errorf("the composition host acknowledged generation %d for binding %s, sent %d", out.GetGeneration(), manifest.Binding, manifest.Generation)
	}
	digest, err := manifest.Digest()
	if err != nil {
		return Receipt{}, err
	}
	c.mu.Lock()
	if held, ok := c.applied[manifest.Binding]; !ok || held.Generation <= manifest.Generation {
		c.applied[manifest.Binding] = AppliedManifest{Binding: manifest.Binding, Generation: manifest.Generation, Digest: digest, Release: manifest.Release.Identity(), Removed: manifest.Removed}
	}
	c.mu.Unlock()
	return Receipt{Disposition: "accepted", Generation: out.GetGeneration()}, nil
}

// Prepare captures one declaration. Deterministic bytes both identify its
// generation and supply the signed sidecar; caller-owned protobufs cannot
// change the captured policy during credential acquisition or signing.
func (c *CompositionDelivery) Prepare(ctx context.Context, module string, removed bool) (*PreparedDelivery, error) {
	var declaration []byte
	if !removed && c.signer != nil {
		var err error
		declaration, err = c.sources.Declaration(ctx, module)
		if err != nil {
			return nil, err
		}
	}
	// Presence is significant even when the declaration serializes empty.
	content := []byte{0}
	if declaration != nil {
		content = append([]byte{1}, declaration...)
	}
	return &PreparedDelivery{Content: content, Post: func(ctx context.Context, module string, signed *solutionhost.Signed, token string) (Receipt, error) {
		return c.post(ctx, module, signed, token, declaration)
	}}, nil
}

// AuthoritySources are a module's own declarations, each read where it lives:
// the method policies its package declares, the actions on its own resource
// kinds, the roles a solution declares, its explicit owner-aware declaration,
// and its installation declaration — the standing role selector and the
// person-context edges it reaches.
//
// It is one type because there are two producers of the authority half: a
// native run sending it over the composition interface, and a publish
// preparing a document for a cell's delivery to send later. They must read the
// same five things and marshal them the same way, or the pair the host
// compares byte-for-byte disagrees for a reason neither producer can see. A
// nil reader declares nothing, which is how a caller that has only some of
// them (a local qualification) is not forced to invent the rest.
type AuthoritySources struct {
	// Methods reads what each method of a module declares, from its package;
	// ok false means the module declares no method policy.
	Methods func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error)
	// Ceilings reads the module's own resource kinds with their actions, from
	// its module contract; none when it publishes no contract.
	Ceilings func(ctx context.Context, module string) ([]*authorityv1.WorkScopeV1, error)
	// Roles reads the roles a solution declares, from its module contract;
	// none for a module, which declares no roles. Core's own manifest has no
	// roles field at all, so this is the only way a role reaches a host.
	Roles func(ctx context.Context, module string) ([]*compositionv1.RoleV1, error)
	// OwnerAware reads the module's explicit owner-selector enforcement declaration.
	OwnerAware func(ctx context.Context, module string) (bool, error)
	// Installation reads the installation declaration. Carried verbatim — the
	// paired agreement rule compares this document against the Core signed
	// manifest byte-for-byte.
	Installation func(ctx context.Context, module string) (string, []*compositionv1.EdgeDependencyV1, error)
}

// Declaration captures one module's declaration as the deterministic bytes
// both identify its generation and are signed as the authority half. A module
// that declares nothing at all answers nil, not an empty document: absence and
// an empty declaration are different things to the host, and a nil here is
// what keeps a withdrawal Core-only.
func (s AuthoritySources) Declaration(ctx context.Context, module string) ([]byte, error) {
	var methods []*compositionv1.MethodPolicyV1
	declared := false
	if s.Methods != nil {
		var err error
		if methods, declared, err = s.Methods(ctx, module); err != nil {
			return nil, err
		}
	}
	var ceilings []*authorityv1.WorkScopeV1
	if s.Ceilings != nil {
		var err error
		if ceilings, err = s.Ceilings(ctx, module); err != nil {
			return nil, err
		}
	}
	var roles []*compositionv1.RoleV1
	if s.Roles != nil {
		var err error
		if roles, err = s.Roles(ctx, module); err != nil {
			return nil, err
		}
	}
	var ownerAware bool
	if s.OwnerAware != nil {
		var err error
		if ownerAware, err = s.OwnerAware(ctx, module); err != nil {
			return nil, err
		}
	}
	var installationRole string
	var edges []*compositionv1.EdgeDependencyV1
	if s.Installation != nil {
		var err error
		if installationRole, edges, err = s.Installation(ctx, module); err != nil {
			return nil, err
		}
	}
	if !declared && len(ceilings) == 0 && len(roles) == 0 && !ownerAware &&
		installationRole == "" && len(edges) == 0 {
		return nil, nil
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(&compositionv1.AuthorityManifestV1{
		Methods: methods, ScopeCeilings: ceilings, Roles: roles, OwnerAware: ownerAware,
		InstallationRole: installationRole, EdgeDependencies: edges,
	})
}

// authority binds the captured declaration to the final Core bytes and signs it.
func (c *CompositionDelivery) authority(manifest *solutionhost.ModuleManifest, core, declaration []byte) (*compositionv1.SignedManifestV1, error) {
	return SignedAuthorityDocument(manifest, core, declaration, c.signer)
}

// SignedAuthorityDocument binds a captured declaration to the final Core bytes
// and signs it: the authority half of a paired delivery.
//
// It is ONE function because there are two producers of that half — a native
// run sending it over the composition interface, and a publish preparing it
// for a cell's delivery to send later — and the host compares the pair under a
// single rule. Two implementations of this binding would be two ways to
// disagree with it. The module, the generation and the digest of the exact
// Core bytes are stamped here rather than taken from the declaration, so the
// signed authority cannot be replayed against another manifest, another
// generation or another module. A nil declaration carries nothing, which is
// how a withdrawal keeps its Core-only identity.
func SignedAuthorityDocument(manifest *solutionhost.ModuleManifest, core, declaration []byte, signer solutionhost.Signer) (*compositionv1.SignedManifestV1, error) {
	hasResolved := !manifest.Removed && manifest.Authority != nil && (len(manifest.Authority.Setup) > 0 || len(manifest.Authority.OwnerAwareResourceKinds) > 0)
	if declaration == nil && !hasResolved {
		return nil, nil
	}
	if signer == nil {
		return nil, errors.New("the authority half of a paired delivery is signed by the release key, and none was given")
	}
	var policy compositionv1.AuthorityManifestV1
	if err := proto.Unmarshal(declaration, &policy); err != nil {
		return nil, err
	}
	// Setup requirements need resolved callees. Use the Core-resolved carrier
	// already captured by native run or publish, never reparse or re-resolve here.
	policy.Setup, policy.OwnerAwareResourceKinds = nil, nil
	if hasResolved {
		policy.OwnerAwareResourceKinds = append([]string(nil), manifest.Authority.OwnerAwareResourceKinds...)
		for _, entry := range manifest.Authority.Setup {
			policy.Setup = append(policy.Setup, &compositionv1.SetupGrantV1{Binding: entry.Binding, Methods: append([]string(nil), entry.Methods...), Scopes: append([]string(nil), entry.Scopes...)})
		}
	}
	policy.Module, policy.Generation, policy.CoreManifestDigest = manifest.Alias, manifest.Generation, delivery.Digest(core)
	document, err := proto.MarshalOptions{Deterministic: true}.Marshal(&policy)
	if err != nil {
		return nil, err
	}
	envelope, err := delivery.Sign(document, signer.KeyID(), signer.Sign)
	if err != nil {
		return nil, err
	}
	return &compositionv1.SignedManifestV1{Document: document, Envelope: envelope}, nil
}

// Applied is NativeDeliverer.Applied over the interface: ApplyManifest applies
// synchronously. Retain the acknowledged request identity for completion in
// this delivery attempt; this is not a remote state query across host restarts.
func (c *CompositionDelivery) Applied(_ context.Context, binding string) (*AppliedManifest, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	applied, ok := c.applied[binding]
	if !ok {
		return nil, false, nil
	}
	return &applied, true, nil
}

// refusal is the host's answer to ApplyManifest as the run reports it: its
// code kept for the retry classification (HostStatusError.Transient), and its
// reason. A host that still holds the binding's withdrawal keeps it terminal
// (core's admission, solutionhost.ErrWithdrawn): the refusal then says what
// the person can do, since the binding is derived from the workspace, the
// environment and the module and is not theirs to choose.
// The withdrawal is recognized by a stopgap (withdrawnRefusal).
func refusal(module string, manifest *solutionhost.ModuleManifest, err error) error {
	code := status.Code(err)
	if code == codes.FailedPrecondition && withdrawnRefusal(status.Convert(err).Message()) {
		return &HostStatusError{Code: code, Reason: solutionhost.ErrWithdrawn, Message: fmt.Sprintf(
			"the host refused module %s at generation %d: it still holds the withdrawal of binding %s from earlier in its life and keeps a withdrawal terminal (%v). "+
				"The local authority holds applied deliveries in memory only: stop the run and start it again, and the host accepts the module afresh", module, manifest.Generation, manifest.Binding, err)}
	}
	return &HostStatusError{Code: code, Message: fmt.Sprintf("the host refused the module manifest of %s through the composition interface: %v", module, err)}
}
