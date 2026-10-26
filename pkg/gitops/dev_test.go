package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

var (
	devOldDigest = "sha256:" + strings.Repeat("a", 64)
	devNewDigest = "sha256:" + strings.Repeat("b", 64)
)

func devDeployment(name, image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
spec:
  template:
    spec:
      containers:
        - name: ` + name + `
          image: ` + image + `
`
}

func devServiceYAML(name string) string {
	return `kind: service
name: ` + name + `
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`
}

// writeDevWorkspace lays down a workspace composing module "shop" with services
// "api" and "worker", and returns it with the module loaded.
func writeDevWorkspace(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                                       "name: acme\nlayout: modules\nmodules:\n  - name: shop\nenvironments:\n  - name: staging\n  - name: production\n",
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                        "kind: module\nname: shop\nservices:\n  - name: api\n  - name: worker\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName):    devServiceYAML("api"),
		filepath.Join("modules", "shop", "services", "worker", resources.ServiceConfigurationName): devServiceYAML("worker"),
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

// renderDevFixture performs a full render of module "shop" for staging.
func renderDevFixture(t *testing.T, workspace *resources.Workspace) RenderResult {
	t.Helper()
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop",
		Units: promotableServiceGraph("shop", []string{"api", "worker"}),
	}, func(_ context.Context, root string) error {
		for rel, content := range map[string]string{
			"services/api/deployment.yaml":    devDeployment("api", "registry.example.com/acme/api:0.0.1@"+devOldDigest),
			"services/api/config.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\ndata:\n  mode: prod\n",
			"services/worker/deployment.yaml": devDeployment("worker", "registry.example.com/acme/worker:0.0.1@"+devOldDigest),
			// A longer repository sharing the api prefix must not be touched.
			"services/worker/sidecar.yaml": devDeployment("sidecar", "registry.example.com/acme/api-sidecar@"+devOldDigest),
		} {
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	return result
}

// stubBuild replaces the shared build/push boundary for the test's duration and
// records what the dev deployment asked it to do.
func stubBuild(t *testing.T, images []string, seen *string) *int {
	t.Helper()
	return stubBuildRecordingRebuild(t, images, seen, nil)
}

func stubBuildRecordingRebuild(t *testing.T, images []string, seen *string, rebuilt *bool) *int {
	t.Helper()
	calls := 0
	previous := buildServiceImage
	buildServiceImage = func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service, _ *environments.Environment, rebuild bool, _ orchestration.OutputSink) ([]string, error) {
		calls++
		if seen != nil {
			*seen = service.Dir()
		}
		if rebuilt != nil {
			*rebuilt = rebuild
		}
		return images, nil
	}
	t.Cleanup(func() { buildServiceImage = previous })
	return &calls
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		data, err := os.ReadFile(path)
		files[filepath.ToSlash(relative)] = string(data)
		return err
	}))
	return files
}

func environmentNamed(name string) *environments.Environment {
	return &environments.Environment{Name: name}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=Jane Doe", "-c", "user.email=user@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestResolveDevSourcePrecedence(t *testing.T) {
	path := t.TempDir()
	override := &resources.ServiceResolution{Kind: resources.ResolutionLocalPath, Dir: "/src/api"}

	source, err := ResolveDevSource("shop", "api", path, override)
	require.NoError(t, err)
	require.Equal(t, DevSource{Dir: path, Origin: DevSourceFlag}, source, "--path wins over the override")

	source, err = ResolveDevSource("shop", "api", "", override)
	require.NoError(t, err)
	require.Equal(t, DevSource{Dir: "/src/api", Origin: DevSourceOverride}, source)

	_, err = ResolveDevSource("shop", "api", "", nil)
	require.ErrorContains(t, err, "nothing to deploy for shop/api: give --path <dir> or set an override")

	_, err = ResolveDevSource("shop", "api", "", &resources.ServiceResolution{Kind: resources.ResolutionPinned, Version: "1.2.3"})
	require.ErrorContains(t, err, "give --path")

	file := filepath.Join(path, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, err = ResolveDevSource("shop", "api", file, nil)
	require.ErrorContains(t, err, "is not a directory")
}

func TestDeployDevRepinsOnlyTheServiceImage(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	root := moduleRenderDestination(workspace, "shop")
	before := snapshotTree(t, root)

	// A source checkout of the api service with an uncommitted change.
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, resources.ServiceConfigurationName), []byte(devServiceYAML("api")), 0o644))
	runGit(t, source, "init", "-q")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-q", "-m", "init")
	head := runGit(t, source, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n"), 0o644))

	newImage := "registry.example.com/acme/api:0.0.1@" + devNewDigest
	var built string
	calls := stubBuild(t, []string{newImage}, &built)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	result, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		AppProject: "acme-staging", Source: DevSource{Dir: source, Origin: DevSourceFlag}, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	require.Equal(t, 1, *calls)
	require.Equal(t, source, built, "the build runs from the dev source")

	require.Equal(t, []string{"deployments/modules/shop/services/api/deployment.yaml", "deployments/modules/shop/" + InventoryFilename}, result.Changed)
	after := snapshotTree(t, root)
	for path, content := range before {
		switch path {
		case "services/api/deployment.yaml":
			require.Equal(t, strings.Replace(content, devOldDigest, devNewDigest, 1), after[path], "only the digest changes")
		case InventoryFilename:
		default:
			require.Equal(t, content, after[path], "%s must be byte-identical", path)
		}
	}
	require.Len(t, after, len(before))

	inventory, err := LoadInventory(root)
	require.NoError(t, err)
	require.Equal(t, []InventoryDevDeployment{{
		Service: "api", Origin: DevSourceFlag, Source: source, Commit: head, Dirty: true,
		Image: newImage, Digest: devNewDigest, DeployedAt: now,
	}}, inventory.Dev)
	require.Equal(t, inventory.Dev[0], result.Entry)
	// The re-derived inventory still describes the tree exactly.
	_, _, err = loadDevTarget(root, "shop", "api", "staging", "")
	require.NoError(t, err)
	require.Equal(t, "dev: shop/api from "+source+"@"+head[:12]+"-dirty", DevCommitTitle("shop", &result.Entry))
}

func TestDeployDevRefusesWithoutAFullRenderOfThatEnvironment(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	calls := stubBuild(t, []string{"registry.example.com/acme/api@" + devNewDigest}, nil)
	request := &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: t.TempDir(), Origin: DevSourceFlag},
	}

	_, err := DeployDev(context.Background(), request)
	require.ErrorContains(t, err, "module shop has never been rendered")
	require.ErrorContains(t, err, "run `codefly deploy gitops render shop --env staging` first")

	renderDevFixture(t, workspace)
	request.Environment = environmentNamed("production")
	_, err = DeployDev(context.Background(), request)
	require.ErrorContains(t, err, "rendered for environment staging, not production")

	request.Environment = environmentNamed("staging")
	request.AppProject = "other"
	_, err = DeployDev(context.Background(), request)
	require.ErrorContains(t, err, `rendered for AppProject "acme-staging", not "other"`)

	request.AppProject = ""
	request.Service = "ghost"
	_, err = DeployDev(context.Background(), request)
	require.ErrorContains(t, err, "does not contain service ghost")

	require.Zero(t, *calls, "nothing is built for a refused deployment")
}

func TestDeployDevRefusesAnImageTheRenderDoesNotPin(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	root := moduleRenderDestination(workspace, "shop")
	before := snapshotTree(t, root)
	stubBuild(t, []string{"registry.example.com/acme/renamed@" + devNewDigest}, nil)

	source := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api")
	_, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: source, Origin: DevSourceFlag},
	})
	require.ErrorContains(t, err, "pins none of the images the build produced")
	require.Equal(t, before, snapshotTree(t, root), "a refused deployment leaves the tree untouched")
}

func TestFullRenderClearsDevDeployment(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	stubBuild(t, []string{"registry.example.com/acme/worker@" + devNewDigest}, nil)
	source := filepath.Join(workspace.Dir(), "modules", "shop", "services", "worker")
	_, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "worker", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: source, Origin: DevSourceOverride},
	})
	require.NoError(t, err)

	result := renderDevFixture(t, workspace)
	require.Len(t, result.ClearedDev, 1)
	require.Equal(t, "worker", result.ClearedDev[0].Service)
	require.Empty(t, result.Inventory.Dev)
	inventory, err := LoadInventory(moduleRenderDestination(workspace, "shop"))
	require.NoError(t, err)
	require.Empty(t, inventory.Dev)

	again := renderDevFixture(t, workspace)
	require.Empty(t, again.ClearedDev, "a render of a clean tree clears nothing")
}

func TestStageDevDeploymentStagesAndCommitsWithoutPushing(t *testing.T) {
	// The commit goes through the user's git; isolate it from this machine's.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Jane Doe")
	t.Setenv("GIT_AUTHOR_EMAIL", "user@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Jane Doe")
	t.Setenv("GIT_COMMITTER_EMAIL", "user@example.com")
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	runGit(t, workspace.Dir(), "init", "-q")
	runGit(t, workspace.Dir(), "add", ".")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "render")
	stubBuild(t, []string{"registry.example.com/acme/api@" + devNewDigest}, nil)
	source := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api")
	result, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: source, Origin: DevSourceOverride},
	})
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, StageDevDeployment(ctx, workspace.Dir(), "shop", &result, false, false))
	staged := runGit(t, workspace.Dir(), "diff", "--cached", "--name-only")
	require.ElementsMatch(t, result.Changed, strings.Split(staged, "\n"))

	require.NoError(t, StageDevDeployment(ctx, workspace.Dir(), "shop", &result, true, false))
	require.Equal(t, DevCommitTitle("shop", &result.Entry), runGit(t, workspace.Dir(), "log", "-1", "--format=%s"))
	require.Empty(t, runGit(t, workspace.Dir(), "status", "--porcelain", "--", "deployments"))
}

func TestDigestImagesReadsWhatTheServiceRenderPinned(t *testing.T) {
	root := t.TempDir()
	image := "registry.example.com/acme/api:0.0.1@" + devNewDigest
	require.NoError(t, os.MkdirAll(filepath.Join(root, "overlays"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "overlays", "deployment.yaml"), []byte(devDeployment("api", image)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "job.yaml"), []byte(devDeployment("migrate", `"`+image+`"`)), 0o644))
	images, err := digestImages(root)
	require.NoError(t, err)
	require.Equal(t, []string{image}, images)

	empty := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(empty, "deployment.yaml"), []byte(devDeployment("api", "registry.example.com/acme/api:latest")), 0o644))
	_, err = digestImages(empty)
	require.ErrorContains(t, err, "pinned no image by digest")
}

func TestDeployDevRefusesATreeEditedSinceItsRender(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	edited := filepath.Join(moduleRenderDestination(workspace, "shop"), "services", "api", "config.yaml")
	require.NoError(t, os.WriteFile(edited, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n"), 0o644))
	calls := stubBuild(t, []string{"registry.example.com/acme/api@" + devNewDigest}, nil)
	_, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: t.TempDir(), Origin: DevSourceFlag},
	})
	require.ErrorContains(t, err, "the tree was edited since it was rendered")
	require.Zero(t, *calls)
}

// A dev deployment exists to carry local code into an environment, so the
// operator who suspects the image does not hold their change has to be able to
// force the build. The request carries that decision to the build boundary.
func TestDeployDevCarriesRebuildToTheBuild(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%t", rebuild), func(t *testing.T) {
			workspace, module := writeDevWorkspace(t)
			renderDevFixture(t, workspace)
			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, resources.ServiceConfigurationName), []byte(devServiceYAML("api")), 0o644))

			var asked bool
			calls := stubBuildRecordingRebuild(t, []string{"registry.example.com/acme/api:0.0.1@" + devNewDigest}, nil, &asked)
			_, err := DeployDev(context.Background(), &DevRequest{
				Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
				AppProject: "acme-staging", Source: DevSource{Dir: source, Origin: DevSourceFlag},
				Rebuild: rebuild,
			})
			require.NoError(t, err)
			require.Equal(t, 1, *calls)
			require.Equal(t, rebuild, asked, "the build must be told whether to rebuild")
		})
	}
}

// hostedDevFixture is the cell fixture (an environment with a host) with both
// modules rendered and the cell file written, ready for a dev deployment of
// shop/api from a source checkout.
func hostedDevFixture(t *testing.T) (*resources.Workspace, *environments.Environment, *DevRequest, string) {
	t.Helper()
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	cell, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(context.Background(), "shop")
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, resources.ServiceConfigurationName), []byte(exposedCellServiceYAML("api")), 0o644))
	return workspace, env, &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: env,
		AppProject: "acme-staging", Source: DevSource{Dir: source, Origin: DevSourceFlag},
	}, cell.Path
}

// cellWorkloadImage reads the first container image of one workload of the
// cell file at path.
func cellWorkloadImage(t *testing.T, path, workload string) cellfile.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := cellfile.Parse(data)
	require.NoError(t, err)
	for _, namespace := range file.Namespaces {
		for _, candidate := range namespace.Workloads {
			if candidate.Name == workload {
				return candidate.Containers[0].Image
			}
		}
	}
	t.Fatalf("the cell file has no %s workload", workload)
	return cellfile.Image{}
}

// isolateGit keeps a test's commits away from this machine's git
// configuration.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Jane Doe")
	t.Setenv("GIT_AUTHOR_EMAIL", "user@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Jane Doe")
	t.Setenv("GIT_COMMITTER_EMAIL", "user@example.com")
}

// A hosted cell admits only the builds its cell file names, so a dev
// deployment into an environment with a host re-derives that file with the
// new digest and reports it among what it changed, for the commit to carry.
func TestDeployDevIntoAHostedEnvironmentRederivesTheCellFile(t *testing.T) {
	isolateGit(t)
	workspace, _, request, cellPath := hostedDevFixture(t)
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), cellWorkloadImage(t, cellPath, "api").Digest)
	runGit(t, workspace.Dir(), "init", "-q")
	runGit(t, workspace.Dir(), "add", ".")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "render")
	stubBuild(t, []string{"registry.example.test/acme/api:1.2.0@" + devNewDigest}, nil)

	result, err := DeployDev(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, []string{
		"deployments/modules/shop/services/api/overlays/staging/deployment.yaml",
		"deployments/modules/shop/" + InventoryFilename,
		"deployments/cells/staging/cell.yaml",
	}, result.Changed, "the cell file is committed with the digest it now admits")
	require.Equal(t, cellfile.Image{Repository: "registry.example.test/acme/api", Digest: devNewDigest}, cellWorkloadImage(t, cellPath, "api"))
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), cellWorkloadImage(t, cellPath, "ledger").Digest, "no other workload moves")
	require.NotNil(t, result.Cell)
	require.Equal(t, cellPath, result.Cell.Path)

	require.NoError(t, StageDevDeployment(context.Background(), workspace.Dir(), "shop", &result, true, false))
	committed := runGit(t, workspace.Dir(), "show", "--name-only", "--format=", "HEAD")
	require.ElementsMatch(t, result.Changed, strings.Split(committed, "\n"), "the commit carries the cell file with the re-pin")
}

// The cell file is re-derived from the whole workspace, so a dev deployment
// whose re-derived file would approve anything the branch's file does not —
// beyond the one service's new build — is refused before anything is built,
// naming what moved. The branch's file is the committed one, not the one on
// disk.
func TestDeployDevRefusesACellFileThatMovesMoreThanItsService(t *testing.T) {
	isolateGit(t)
	workspace, env, request, cellPath := hostedDevFixture(t)
	calls := stubBuild(t, []string{"registry.example.test/acme/api:1.2.0@" + devNewDigest}, nil)

	// The branch approves a ledger build the workspace's billing tree does not
	// pin, while the file on disk is re-derived from the workspace.
	data, err := os.ReadFile(cellPath)
	require.NoError(t, err)
	file, err := cellfile.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "acme-billing", file.Namespaces[0].Name)
	require.Equal(t, "ledger", file.Namespaces[0].Workloads[0].Name)
	file.Namespaces[0].Workloads[0].Containers[0].Image.Digest = "sha256:" + strings.Repeat("e", 64)
	approved, err := file.Encode()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cellPath, approved, 0o600))
	runGit(t, workspace.Dir(), "init", "-q")
	runGit(t, workspace.Dir(), "add", ".")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "render")
	_, err = RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)

	_, err = DeployDev(context.Background(), request)
	require.ErrorContains(t, err, "changes more than shop/api's images — namespace acme-billing differ from the committed cell file")
	require.ErrorContains(t, err, "commit them first")
	require.Zero(t, *calls, "nothing is built for a refused deployment")
}

// Only the digests of the images the dev build produced may move: a sidecar
// of the same service the branch pins differently is not the dev
// deployment's to approve.
func TestDevCellMovesOnlyTheBuiltRepositories(t *testing.T) {
	_, _, _, cellPath := hostedDevFixture(t)
	data, err := os.ReadFile(cellPath)
	require.NoError(t, err)
	start, err := cellfile.Parse(data)
	require.NoError(t, err)
	derived, err := cellfile.Parse(data)
	require.NoError(t, err)
	shop := &derived.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	shop.Workloads[0].Containers[0].Image.Digest = devNewDigest
	built := map[string]bool{"registry.example.test/acme/api": true}
	moved, err := cellMovesBeyond(start, derived, "shop/api", built)
	require.NoError(t, err)
	require.Empty(t, moved, "the built image's digest moves")

	require.Equal(t, "proxy", shop.Workloads[0].Containers[1].Name)
	shop.Workloads[0].Containers[1].Image.Digest = "sha256:" + strings.Repeat("f", 64)
	moved, err = cellMovesBeyond(start, derived, "shop/api", built)
	require.NoError(t, err)
	require.Equal(t, []string{"namespace acme-shop"}, moved, "the proxy sidecar was not built")
	moved, err = cellMovesBeyond(start, derived, "shop/api", nil)
	require.NoError(t, err)
	require.Empty(t, moved, "before the build, any of the service's digests may move")
}

// Without a committed cell file the branch approves no build to replace, so a
// dev deployment into a git checkout refuses rather than committing a whole
// approval set.
func TestDeployDevRefusesAnUncommittedCellFile(t *testing.T) {
	isolateGit(t)
	workspace, _, request, _ := hostedDevFixture(t)
	runGit(t, workspace.Dir(), "init", "-q")
	runGit(t, workspace.Dir(), "add", "workspace.codefly.yaml")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "workspace")
	calls := stubBuild(t, []string{"registry.example.test/acme/api:1.2.0@" + devNewDigest}, nil)
	_, err := DeployDev(context.Background(), request)
	require.ErrorContains(t, err, "environment staging's cell file deployments/cells/staging/cell.yaml is not committed")
	require.Zero(t, *calls)
}

// The delivery overlay (`module-manifests/`) is publish's: after a render,
// publish settles and signs the manifest there, adds the delivery Job and
// rewrites the kustomization, and the branch carries that version while the
// render record still lists the provisional one. A dev deployment re-pins one
// service's image and leaves the overlay as it finds it, so a tree whose only
// difference from its render is that overlay is not "edited since its render":
// the deployment proceeds and the re-derived inventory lists the overlay as
// found. A moved service file still refuses (the test above).
func TestDeployDevCarriesThePublishedDeliveryOverlayAsFound(t *testing.T) {
	workspace, module := writeDevWorkspace(t)
	renderDevFixture(t, workspace)
	root := moduleRenderDestination(workspace, "shop")
	inventory, err := LoadInventory(root)
	require.NoError(t, err)
	inventory.ModuleManifestPath = moduleManifestDir
	require.NoError(t, writeCanonicalInventory(filepath.Join(root, InventoryFilename), &inventory))
	overlay := filepath.Join(root, moduleManifestDir, "overlays", "staging")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	delivered := filepath.Join(overlay, "deliver-manifest.yaml")
	require.NoError(t, os.WriteFile(delivered, []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: deliver-manifest\n"), 0o644))
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, resources.ServiceConfigurationName), []byte(devServiceYAML("api")), 0o644))
	stubBuild(t, []string{"registry.example.com/acme/api@" + devNewDigest}, nil)
	result, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		Source: DevSource{Dir: source, Origin: DevSourceFlag},
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Changed)
	after, err := LoadInventory(root)
	require.NoError(t, err)
	var listed bool
	for _, file := range after.Files {
		listed = listed || file.Path == moduleManifestDir+"/overlays/staging/deliver-manifest.yaml"
	}
	require.True(t, listed, "the re-derived inventory lists the delivery overlay as found: %+v", after.Files)
	require.Equal(t, moduleManifestDir, after.ModuleManifestPath)
}
