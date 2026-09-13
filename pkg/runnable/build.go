// Package runnable drives language agents over the shared Builder protocol.
package runnable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/agents/manager"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const PackageFile = "runnable-package.json"

// Create scaffolds through the selected agent and rolls back its reference on
// failure. No language-specific template or default handler belongs here.
func Create(ctx context.Context, workspace *resources.Workspace, module *resources.Module, name string, agent *resources.Agent, handler string, logs io.Writer) (*resources.Runnable, error) {
	r, err := module.NewRunnable(ctx, name, agent, handler)
	if err != nil {
		return nil, err
	}
	createErr := create(ctx, workspace, r, logs)
	if createErr == nil {
		return resources.LoadRunnableFromDir(ctx, r.Dir())
	}
	// Keep the directory if reference rollback cannot be persisted: a reference
	// to recoverable source is better than a reference to a deleted directory.
	module.RunnableReferences = slices.DeleteFunc(module.RunnableReferences, func(ref *resources.RunnableReference) bool { return ref.Name == name })
	if err := module.Save(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("creation failed: %w; reference rollback failed: %v; source retained at %s", createErr, err, r.Dir())
	}
	if err := os.RemoveAll(r.Dir()); err != nil {
		return nil, fmt.Errorf("creation failed: %w; remove incomplete source: %v", createErr, err)
	}
	return nil, createErr
}

func create(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, logs io.Writer) error {
	client, closeAgent, err := load(ctx, workspace, r, logs)
	if err != nil {
		return err
	}
	defer closeAgent()
	response, err := client.Create(ctx, &builderv0.CreateRequest{})
	if err != nil {
		return fmt.Errorf("agent Create: %w", err)
	}
	if response.GetState().GetState() != builderv0.CreateStatus_CREATED {
		return fmt.Errorf("agent did not create the Runnable: %s", response.GetState().GetMessage())
	}
	return nil
}

// Build emits one verified native release into a new caller-owned directory.
// This is authoring/building; it does not install a binding or invoke a task.
func Build(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, output string, logs io.Writer) (*basev0.RunnablePackage, error) {
	if !filepath.IsAbs(output) {
		return nil, fmt.Errorf("build output must be absolute")
	}
	if len(r.LibraryDependencies) != 0 {
		return nil, fmt.Errorf("runnable builds with internal library dependencies are not implemented")
	}
	for _, dependency := range r.ServiceDependencies {
		if dependency.Kind == resources.DependencyKindBuild || dependency.Kind == resources.DependencyKindSchema || dependency.Kind == resources.DependencyKindCompletion {
			return nil, fmt.Errorf("runnable build prerequisite %s/%s [%s] is not yet supported", dependency.Module, dependency.Name, dependency.Kind)
		}
	}
	client, closeAgent, err := load(ctx, workspace, r, logs)
	if err != nil {
		return nil, err
	}
	defer closeAgent()
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return nil, fmt.Errorf("build output must be new: %w", err)
	}
	inputs, err := client.RunnableBuildInputs(ctx, &builderv0.RunnableBuildInputsRequest{OutputDirectory: filepath.Join(output, "prepared")})
	if err != nil {
		return nil, fmt.Errorf("agent RunnableBuildInputs: %w", err)
	}
	if inputs.GetState().GetState() != builderv0.RunnableBuildInputsStatus_SUCCESS || inputs.GetBuild() == nil {
		return nil, fmt.Errorf("agent did not prepare build inputs: %s", inputs.GetState().GetMessage())
	}
	artifactsDir := filepath.Join(output, "artifacts")
	response, err := client.Package(ctx, &builderv0.PackageRequest{OutputDirectory: artifactsDir})
	if err != nil {
		return nil, fmt.Errorf("agent Package: %w", err)
	}
	if response.GetState().GetState() != builderv0.PackageStatus_SUCCESS {
		return nil, fmt.Errorf("agent did not package the Runnable: %s", response.GetState().GetMessage())
	}
	pkg, err := assemble(ctx, workspace, r, inputs.GetBuild(), response.GetArtifacts())
	if err != nil {
		return nil, err
	}
	if err = VerifyBuild(ctx, workspace, r, pkg, response.GetArtifacts(), artifactsDir); err != nil {
		return nil, err
	}
	encoded, err := protojson.MarshalOptions{Indent: "  ", UseProtoNames: true}.Marshal(pkg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, PackageFile), append(encoded, '\n'), 0600); err != nil {
		return nil, err
	}
	return pkg, nil
}

func load(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, logs io.Writer) (builderv0.BuilderClient, func(), error) {
	location, err := workspace.RunnableLocationOf(r)
	if err != nil {
		return nil, nil, err
	}
	wire, err := location.Proto()
	if err != nil {
		return nil, nil, err
	}
	connection, err := manager.Load(ctx, r.Agent, manager.WithoutSandbox(), manager.WithoutPrincipal(), manager.WithLogWriter(logs))
	if err != nil {
		return nil, nil, fmt.Errorf("start Runnable agent: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			connection.Close()
		}
	}()
	info, err := agentv0.NewAgentClient(connection.GRPCConn()).GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return nil, nil, fmt.Errorf("agent metadata: %w", err)
	}
	if !slices.ContainsFunc(info.GetCapabilities(), func(c *agentv0.Capability) bool { return c.GetType() == agentv0.Capability_BUILDER }) {
		return nil, nil, fmt.Errorf("runnable agent %s does not advertise Builder; install a compatible agent", r.Agent.Identifier())
	}
	client := builderv0.NewBuilderClient(connection.GRPCConn())
	loaded, err := client.Load(ctx, &builderv0.LoadRequest{Runnable: wire})
	if err != nil {
		return nil, nil, fmt.Errorf("agent Load: %w", err)
	}
	if loaded.GetState().GetState() != builderv0.LoadStatus_READY {
		return nil, nil, fmt.Errorf("agent did not load the Runnable: %s", loaded.GetState().GetMessage())
	}
	failed = false
	return client, func() { connection.Close() }, nil
}

// VerifyBuild accepts only an identified package matching the declaration and
// the files actually emitted by the agent. It never imports agent code.
func VerifyBuild(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, pkg *basev0.RunnablePackage, emittedFiles []*builderv0.PackageArtifact, directory string) error {
	if err := corerunnable.VerifyPackage(pkg); err != nil {
		return fmt.Errorf("verify Runnable package: %w", err)
	}
	location, err := workspace.RunnableLocationOf(r)
	if err != nil {
		return err
	}
	identity, err := location.Identity.Proto()
	if err != nil {
		return err
	}
	declaration, err := r.Proto(ctx)
	if err != nil {
		return err
	}
	if !proto.Equal(identity, pkg.GetIdentity()) || !proto.Equal(declaration.GetAgent(), pkg.GetAgent()) || !proto.Equal(declaration.GetContract(), pkg.GetContract()) {
		return fmt.Errorf("agent package identity, agent, contract or execution policy differs from the declaration")
	}
	// Canonicalize declared dependencies using the same core preparation rules,
	// so ordering is irrelevant but omission or addition is still an error.
	expected := &basev0.RunnablePackage{}
	proto.Merge(expected, pkg)
	expected.Digest = ""
	expected.Execution = declaration.GetExecution()
	expected.ServiceDependencies = declaration.GetServiceDependencies()
	expected.WorkspaceConfigurationDependencies = declaration.GetWorkspaceConfigurationDependencies()
	expected, err = corerunnable.PreparePackage(expected)
	if err != nil {
		return err
	}
	if expected.GetDigest() != pkg.GetDigest() {
		return fmt.Errorf("agent package dependencies differ from the declaration")
	}
	if pkg.GetBuild().GetHandler().GetPath() != r.Entrypoint.Handler {
		return fmt.Errorf("agent package handler differs from the declaration")
	}
	declaredInputs := slices.Clone(r.Entrypoint.Inputs)
	slices.Sort(declaredInputs)
	var packagedInputs []string
	for _, input := range pkg.GetBuild().GetInputs() {
		packagedInputs = append(packagedInputs, input.GetPath())
	}
	slices.Sort(packagedInputs)
	if !slices.Equal(declaredInputs, packagedInputs) {
		return fmt.Errorf("agent package build input paths differ from the declaration")
	}
	return verifyArtifacts(pkg.GetArtifacts(), emittedFiles, directory)
}

func verifyArtifacts(artifacts []*basev0.RunnableArtifact, emittedFiles []*builderv0.PackageArtifact, directory string) error {
	if len(artifacts) != len(emittedFiles) {
		return fmt.Errorf("package descriptor and emitted artifacts differ")
	}
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		name := artifact.GetReference()
		if filepath.Base(name) != name || name == "." || name == ".." || name == PackageFile || strings.ContainsAny(name, "/\\") || seen[name] {
			return fmt.Errorf("invalid or duplicate native artifact filename %q", name)
		}
		seen[name] = true
		if artifact.GetKind() != basev0.RunnableArtifact_NATIVE {
			return fmt.Errorf("native build returned an unsupported artifact kind")
		}
		file := filepath.Join(root, name)
		info, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact %s is not a regular file", name)
		}
		matched := false
		for _, emitted := range emittedFiles {
			if filepath.Clean(emitted.GetPath()) == file && emitted.GetKind() == builderv0.PackageArtifact_ARCHIVE && emitted.GetTarget().GetOs()+"/"+emitted.GetTarget().GetArchitecture() == artifact.GetPlatform() && "sha256:"+emitted.GetSha256() == artifact.GetDigest() && slices.Equal(emitted.GetCommand(), artifact.GetCommand()) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("native artifact %s does not match the emitted file metadata", name)
		}
		reader, err := os.Open(file)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != artifact.GetDigest() {
			return fmt.Errorf("native artifact %s content digest does not match", name)
		}
	}
	return nil
}

func assemble(ctx context.Context, workspace *resources.Workspace, r *resources.Runnable, build *basev0.RunnableBuild, files []*builderv0.PackageArtifact) (*basev0.RunnablePackage, error) {
	location, err := workspace.RunnableLocationOf(r)
	if err != nil {
		return nil, err
	}
	identity, err := location.Identity.Proto()
	if err != nil {
		return nil, err
	}
	declaration, err := r.Proto(ctx)
	if err != nil {
		return nil, err
	}
	pkg := &basev0.RunnablePackage{
		Schema: corerunnable.PackageSchemaV1, Identity: identity, Agent: declaration.GetAgent(),
		Contract: declaration.GetContract(), Execution: declaration.GetExecution(), Build: build,
		ServiceDependencies:                declaration.GetServiceDependencies(),
		WorkspaceConfigurationDependencies: declaration.GetWorkspaceConfigurationDependencies(),
	}
	for _, file := range files {
		if file.GetKind() != builderv0.PackageArtifact_ARCHIVE || len(file.GetCommand()) == 0 {
			return nil, fmt.Errorf("agent must return a native archive with its launch command")
		}
		pkg.Artifacts = append(pkg.Artifacts, &basev0.RunnableArtifact{
			Kind: basev0.RunnableArtifact_NATIVE, Platform: file.GetTarget().GetOs() + "/" + file.GetTarget().GetArchitecture(),
			Reference: filepath.Base(file.GetPath()), Digest: "sha256:" + file.GetSha256(), Command: slices.Clone(file.GetCommand()),
		})
	}
	return corerunnable.PreparePackage(pkg)
}
