// Package runnable drives language agents over the shared Builder protocol.
package runnable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/agents/contract"
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
	if err := removeRunnableReference(module, name); err != nil {
		return nil, fmt.Errorf("creation failed: %w; reference rollback failed: %v; source retained at %s", createErr, err, r.Dir())
	}
	if err := module.Save(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("creation failed: %w; reference rollback failed: %v; source retained at %s", createErr, err, r.Dir())
	}
	if err := os.RemoveAll(r.Dir()); err != nil {
		return nil, fmt.Errorf("creation failed: %w; remove incomplete source: %v", createErr, err)
	}
	return nil, createErr
}

// removeRunnableReference drops exactly one runnable reference from the module.
// Core owns AddRunnableReference but exposes no removal counterpart, so the
// rollback has to reach the slice directly; requiring exactly one removal keeps
// a silent no-op from being mistaken for a completed rollback. Tracked for core
// in https://github.com/codefly-dev/core/issues/472.
func removeRunnableReference(module *resources.Module, name string) error {
	before := len(module.RunnableReferences)
	module.RunnableReferences = slices.DeleteFunc(module.RunnableReferences, func(ref *resources.RunnableReference) bool { return ref.Name == name })
	switch removed := before - len(module.RunnableReferences); removed {
	case 1:
		return nil
	case 0:
		return fmt.Errorf("module %s carries no reference to runnable %s", module.Name, name)
	default:
		return fmt.Errorf("module %s carried %d references to runnable %s", module.Name, removed, name)
	}
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
	// Claim the output directory before paying for an agent process: a reused
	// directory is the common retry mistake and must not cost a subprocess, a
	// gRPC handshake and a Load first.
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return nil, fmt.Errorf("build output must be new: %w", err)
	}
	client, closeAgent, err := load(ctx, workspace, r, logs)
	if err != nil {
		// Nothing has written to the directory yet, so removing it keeps a
		// failed agent start from poisoning the output path for the retry.
		return nil, errors.Join(err, os.Remove(output))
	}
	defer closeAgent()
	preparedDir := filepath.Join(output, "prepared")
	inputs, err := client.RunnableBuildInputs(ctx, &builderv0.RunnableBuildInputsRequest{OutputDirectory: preparedDir})
	if err != nil {
		return nil, fmt.Errorf("agent RunnableBuildInputs: %w", err)
	}
	if inputs.GetState().GetState() != builderv0.RunnableBuildInputsStatus_SUCCESS || inputs.GetBuild() == nil {
		return nil, fmt.Errorf("agent did not prepare build inputs: %s", inputs.GetState().GetMessage())
	}
	if err = verifyPrepared(preparedDir); err != nil {
		return nil, err
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

// buildRoot is the workspace subtree the CLI owns for scratch Runnable builds.
var buildRoot = filepath.Join(".codefly", "build", "runnables")

// DefaultOutput is the CLI-owned scratch build directory for r. It is
// deterministic, so callers must reset it before reusing it; see ResetOutput.
func DefaultOutput(workspace *resources.Workspace, r *resources.Runnable) (string, error) {
	output := filepath.Join(workspace.Dir(), buildRoot, r.Module(), r.Name, r.Version)
	if err := ownedOutput(workspace, output); err != nil {
		return "", err
	}
	return output, nil
}

// ResetOutput removes a CLI-owned scratch build directory so a rebuild can
// recreate it. Release immutability is enforced by the package digest and
// core's CompareRelease, never by refusing to reuse a scratch directory, so
// the edit/rebuild loop must not strand itself on a deterministic path. It
// refuses any path outside the workspace's Runnable build root, so an explicit
// --output can never be removed through here.
func ResetOutput(workspace *resources.Workspace, output string) error {
	if err := ownedOutput(workspace, output); err != nil {
		return err
	}
	return os.RemoveAll(output)
}

func ownedOutput(workspace *resources.Workspace, output string) error {
	root := filepath.Join(workspace.Dir(), buildRoot)
	relative, err := filepath.Rel(root, output)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(relative) {
		return fmt.Errorf("build directory %s is not inside the workspace Runnable build root %s", output, root)
	}
	return nil
}

// verifyPrepared holds the agent to the directory contract the CLI hands it:
// a SUCCESS from RunnableBuildInputs must have materialized build inputs at
// the requested path, otherwise the CLI defined a location nothing wrote to.
func verifyPrepared(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("agent reported prepared build inputs but %s is unusable: %w", directory, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("agent reported prepared build inputs but wrote nothing to %s", directory)
	}
	return nil
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
	if err = contract.Check(info.GetContract()); err != nil {
		return nil, nil, fmt.Errorf("incompatible Runnable agent %s: %w", r.Agent.Identifier(), err)
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
//
// Only build evidence and artifacts are agent-sourced. Build() constructs
// identity, agent, contract, execution and dependencies from the declaration
// itself, so those comparisons re-check the CLI's own construction there; they
// do real work only when VerifyBuild re-verifies a package it did not build,
// such as one read back from disk. Making them meaningful in the build path
// needs the agent to return them over the wire, which PackageResponse does not
// yet carry (codefly-dev/core#472).
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
		return fmt.Errorf("agent package identity, agent or contract differs from the declaration")
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
	if err := verifyBuildInputs(r.Dir(), pkg.GetBuild(), r.Entrypoint.Handler, r.Entrypoint.Inputs); err != nil {
		return err
	}
	return verifyArtifacts(pkg.GetArtifacts(), emittedFiles, directory)
}

// verifyBuildInputs holds the agent's build evidence against the author's
// source tree. The evidence digests are what make the package digest a content
// address, and core's CompareRelease uses that digest to tell an idempotent
// re-registration from a conflict. Checking only the paths would leave every
// digest in the descriptor self-consistent within the agent's own output: an
// agent that packaged a stale snapshot would report a stale handler digest
// inside a stale archive whose digest matches it, and nothing would notice the
// author's current source never shipped. The CLI holds that source, so it
// hashes it here rather than trusting the process it exists to verify.
func verifyBuildInputs(dir string, build *basev0.RunnableBuild, handler string, inputs []string) error {
	if dir == "" {
		return fmt.Errorf("runnable was not loaded from a directory; cannot verify build inputs")
	}
	if build.GetHandler().GetPath() != handler {
		return fmt.Errorf("agent package handler differs from the declaration")
	}
	declaredInputs := slices.Clone(inputs)
	slices.Sort(declaredInputs)
	packagedInputs := make([]string, 0, len(build.GetInputs()))
	digests := make(map[string]string, len(build.GetInputs()))
	for _, input := range build.GetInputs() {
		packagedInputs = append(packagedInputs, input.GetPath())
		digests[input.GetPath()] = input.GetDigest()
	}
	slices.Sort(packagedInputs)
	if !slices.Equal(declaredInputs, packagedInputs) {
		return fmt.Errorf("agent package build input paths differ from the declaration")
	}
	// Hash the declared path, never the agent-supplied string: the two are
	// equal by the check above, and the declared one is the path core already
	// confined to the runnable directory.
	if err := verifyInputDigest(dir, handler, build.GetHandler().GetDigest(), "handler"); err != nil {
		return err
	}
	for _, input := range declaredInputs {
		if err := verifyInputDigest(dir, input, digests[input], "build input"); err != nil {
			return err
		}
	}
	return nil
}

func verifyInputDigest(dir string, relative string, claimed string, kind string) error {
	actual, err := digestFile(filepath.Join(dir, relative))
	if err != nil {
		return fmt.Errorf("verify %s %s against the source: %w", kind, relative, err)
	}
	if actual != claimed {
		return fmt.Errorf("agent package %s %s digest %s does not match the source on disk (%s); the agent packaged different bytes than the declaration references", kind, relative, claimed, actual)
	}
	return nil
}

// digestFile returns the "sha256:<hex>" content digest of one regular file, the
// same form the shared descriptor uses for build evidence and artifacts.
func digestFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
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
			if canonicalPath(emitted.GetPath()) == file && emitted.GetKind() == builderv0.PackageArtifact_ARCHIVE && emitted.GetTarget().GetOs()+"/"+emitted.GetTarget().GetArchitecture() == artifact.GetPlatform() && "sha256:"+emitted.GetSha256() == artifact.GetDigest() && slices.Equal(emitted.GetCommand(), artifact.GetCommand()) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("native artifact %s does not match the emitted file metadata", name)
		}
		actual, err := digestFile(file)
		if err != nil {
			return err
		}
		if actual != artifact.GetDigest() {
			return fmt.Errorf("native artifact %s content digest does not match", name)
		}
	}
	return verifyNoUnverifiedFiles(root, seen)
}

// verifyNoUnverifiedFiles rejects anything in the artifact directory the agent
// did not report. Everything the descriptor names has just been digested, so
// an unreported file is content the build presents as part of a verified
// package without any evidence behind it. The descriptor itself is allowed:
// Build writes it after verification, and a re-verification of an existing
// build directory legitimately finds it already there.
func verifyNoUnverifiedFiles(root string, verified map[string]bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if verified[entry.Name()] || entry.Name() == PackageFile {
			continue
		}
		return fmt.Errorf("artifact directory contains %q, which the agent did not report as an artifact", entry.Name())
	}
	return nil
}

// canonicalPath puts an agent-reported path in the same form as the resolved
// artifact root, so a build output reached through a symlink (macOS /tmp ->
// /private/tmp) compares equal to the path the agent echoes back. Only the
// directory is resolved: the final component must still be the regular file
// Lstat accepted, never a link aliasing it.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return p
	}
	return filepath.Join(dir, filepath.Base(p))
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
