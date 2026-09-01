package gitops

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codefly-dev/core/agents/manager"
	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution"
	"google.golang.org/grpc"
)

// SolutionRenderRequest names a codefly:solution executor and the environment
// whose packaged anatomy it renders into the owned gitops tree.
//
// A solution is deployed like a single-unit module: it renders to
// deployments/modules/<name> and carries one solution unit, so the existing
// publish/observe/ApplicationSet pipeline stamps its Application and syncs its
// namespace without any transport changes.
//
// Each solution renders into its own namespace, derived from its id, rather than
// the shared host namespace, so solutions are isolatable from the platform and
// from one another.
type SolutionRenderRequest struct {
	Workspace   *resources.Workspace
	Environment *resources.Environment
	// Agent is the resolved codefly:solution executor that packages and renders
	// the solution's anatomy.
	Agent *resources.Agent
	// Name is the solution's deploy identity: its module, unit, and render
	// subdirectory name.
	Name string
	// Source is the solution source directory the executor packages into an OCI
	// artifact.
	Source string
	// Reference is the target OCI reference the executor pushes the package to.
	Reference  string
	AppProject string
	// Values are passed verbatim to the executor's Render so a solution can
	// specialize its manifests per invocation.
	Values map[string]string
}

// RenderSolution packages a solution into an OCI artifact and renders its
// manifests into the owned gitops tree, driving the codefly:solution executor
// through the same promotable render pipeline services and modules use.
func RenderSolution(ctx context.Context, req *SolutionRenderRequest) (RenderResult, error) {
	if err := req.Workspace.ValidateEnvironments(ctx); err != nil {
		return RenderResult{}, err
	}
	env := req.Environment
	namespace, err := solutionNamespace(req.Name, env.Namespace)
	if err != nil {
		return RenderResult{}, err
	}
	destination := filepath.Join(req.Workspace.Dir(), "deployments", "modules", req.Name)
	ownedPath := filepath.ToSlash(filepath.Join("deployments", "modules", req.Name))
	gitopsPath := ""
	if env.Gitops != nil {
		gitopsPath = env.Gitops.Path
	} else if req.Workspace.Gitops != nil {
		gitopsPath = req.Workspace.Gitops.Path
	}
	if gitopsPath != "" {
		ownedPath = filepath.ToSlash(filepath.Join(gitopsPath, ownedPath))
	}
	options := &RenderOptions{
		Destination: destination,
		Module:      req.Name,
		Environment: env.Name,
		Namespace:   namespace,
		AppProject:  req.AppProject,
		Promotable:  true,
		OwnedPath:   ownedPath,
	}
	return RenderOwnedTree(ctx, options, func(ctx context.Context, stage string) error {
		executor, release, err := connectSolutionExecutor(ctx, req.Workspace.Dir(), req.Agent)
		if err != nil {
			return err
		}
		defer release()

		artifact := &solutionv0.SolutionArtifact{
			Publisher: req.Agent.Publisher,
			Name:      req.Agent.Name,
			Version:   req.Agent.Version,
		}
		solutionContext := &solutionv0.SolutionContext{
			Workspace:   req.Workspace.Name,
			Environment: env.Name,
			Artifact:    artifact,
		}

		packaged, err := executor.Package(ctx, solution.CeilingPublish(), &solutionv0.PackageRequest{
			Context:   solutionContext,
			Source:    req.Source,
			Reference: req.Reference,
		})
		if err != nil {
			return fmt.Errorf("package solution %s: %w", req.Name, err)
		}
		if err = solutionDiagnostics("package", req.Name, packaged.GetDiagnostics()); err != nil {
			return err
		}
		artifact.ArtifactDigest = packaged.GetArtifactDigest()

		rendered, err := executor.Render(ctx, solution.CeilingRender(), &solutionv0.RenderRequest{
			Context:           solutionContext,
			ArtifactReference: packaged.GetReference(),
			Destination:       filepath.Join(stage, solutionUnitDir, req.Name),
			Values:            solutionRenderValues(req.Values, namespace),
		})
		if err != nil {
			return fmt.Errorf("render solution %s: %w", req.Name, err)
		}
		if err = solutionDiagnostics("render", req.Name, rendered.GetDiagnostics()); err != nil {
			return err
		}
		if len(rendered.GetRenderedPaths()) == 0 {
			return fmt.Errorf("solution %s rendered no manifests", req.Name)
		}

		options.Units = []InventoryUnit{{
			Kind:   UnitKindSolution,
			Module: req.Name,
			Name:   req.Name,
			Path:   filepath.ToSlash(filepath.Join(solutionUnitDir, req.Name)),
			Output: solutionRenderAttestation(),
		}}
		return nil
	})
}

// SolutionNamespaceValue is the Render value key through which the CLI tells the
// solution executor the exact namespace its manifests must target. Publish binds
// the rendered namespace to the solution's own namespace — the AppProject's sole
// destination — so the executor cannot choose it and must render into this one.
const SolutionNamespaceValue = "codefly.namespace"

// dns1123Label is the RFC 1123 label grammar Kubernetes enforces on a namespace
// name: lowercase alphanumerics and '-', beginning and ending alphanumeric.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// solutionNamespace derives a solution's own Kubernetes namespace from its deploy
// id, isolating each solution from the platform host namespace and from sibling
// solutions. The id is only a validated path component upstream (it names the
// render subdirectory and module), which is far looser than a namespace name, so
// the two invariants a namespace carries are enforced here rather than left to
// fail at cluster apply:
//
//   - It must be a valid RFC 1123 label. The render authorizes the Namespace under
//     a declared AppProject that skips per-name checks, and publish stamps it into
//     the AppProject destination unvalidated, so an id like "My_Solution" would
//     otherwise sail through render and publish and fail only when ArgoCD applies
//     it — a late, opaque error far from the deploy command.
//   - It must differ from the host namespace. A solution renders and owns a
//     cluster-scoped Namespace object that its ArgoCD Application prunes under the
//     promotable sync policy (Automated.Prune); were that the shared platform
//     namespace, the solution would own it and, on any later namespace change,
//     prune it — cascade-deleting the platform. A solution is isolated by
//     definition, so this is a hard error, not a warning.
func solutionNamespace(name, hostNamespace string) (string, error) {
	if len(name) > 63 || !dns1123Label.MatchString(name) {
		return "", fmt.Errorf("solution %q is not a valid Kubernetes namespace: it must be a lowercase RFC 1123 label (a-z, 0-9, '-') of at most 63 characters", name)
	}
	if name == hostNamespace {
		return "", fmt.Errorf("solution %q would render into the host namespace %q; a solution must be isolated in its own namespace, not the shared platform namespace", name, hostNamespace)
	}
	return name, nil
}

// solutionRenderValues layers the CLI-authoritative namespace over the caller's
// values. The namespace wins on collision: a value that disagreed with the
// solution namespace would render a Namespace the publish AppProject rejects.
func solutionRenderValues(values map[string]string, namespace string) map[string]string {
	merged := make(map[string]string, len(values)+1)
	for key, value := range values {
		merged[key] = value
	}
	merged[SolutionNamespaceValue] = namespace
	return merged
}

// solutionExecutor is the subset of the ceiling-enforcing solution client
// RenderSolution drives. It is an interface so a test can inject a client backed
// by an in-process executor without spawning a verified-artifact plugin.
type solutionExecutor interface {
	Package(context.Context, solution.Ceiling, *solutionv0.PackageRequest, ...grpc.CallOption) (*solutionv0.PackageResponse, error)
	Render(context.Context, solution.Ceiling, *solutionv0.RenderRequest, ...grpc.CallOption) (*solutionv0.RenderResponse, error)
}

// connectSolutionExecutor resolves and loads the codefly:solution executor,
// returning a ceiling-enforcing client and a release function that tears the
// agent connection down. It is a package variable so tests can substitute an
// in-process executor.
var connectSolutionExecutor = func(ctx context.Context, workDir string, agent *resources.Agent) (solutionExecutor, func(), error) {
	if _, err := manager.ResolveLatest(ctx, agent); err != nil {
		return nil, nil, fmt.Errorf("resolve solution agent %s: %w", agent.Name, err)
	}
	conn, err := manager.Load(ctx, agent, manager.WithWorkDir(workDir), manager.WithoutSandbox(), manager.WithoutPrincipal())
	if err != nil {
		return nil, nil, fmt.Errorf("load solution agent %s: %w", agent.Name, err)
	}
	return solution.NewClient(conn.GRPCConn()), conn.Close, nil
}

func solutionDiagnostics(phase, name string, diagnostics []*basev0.FailureDiagnostic) error {
	var failures []string
	for _, diagnostic := range diagnostics {
		if diagnostic.GetSeverity() == basev0.FailureDiagnostic_ERROR {
			failures = append(failures, diagnostic.GetMessage())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s solution %s: %s", phase, name, strings.Join(failures, "; "))
}

// solutionRenderAttestation records that a solution unit's manifests cleared the
// promotable ruleset. The codefly:solution Render contract returns manifest paths
// rather than the builder's Kubernetes deployment evidence, so the attestation is
// the CLI's own render-time validateTree(Promotable) pass — server-side
// validation stays NOT_RUN because the CLI runs only the static ruleset.
func solutionRenderAttestation() *InventoryKubernetesOutput {
	return &InventoryKubernetesOutput{
		Kind: builderv0.KubernetesDeploymentOutput_KUSTOMIZE.String(),
		//nolint:staticcheck // validateInventoryKubernetesOutput accepts exactly this profile; matching it is required, not optional.
		Profile:         builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1.String(),
		ContractVersion: coreservices.KubernetesManifestContractVersion,
		Validation: &InventoryKubernetesValidation{
			StaticValidation:     builderv0.KubernetesManifestValidation_STATUS_PASSED.String(),
			ServerSideValidation: builderv0.KubernetesManifestValidation_STATUS_NOT_RUN.String(),
			Promotable:           true,
			Violations:           []string{},
		},
	}
}
