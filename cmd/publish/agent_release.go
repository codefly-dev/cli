package publish

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// platform names a release target the agent loader can install.
type platform struct {
	os   string
	arch string
}

func (p platform) target() string { return p.os + "/" + p.arch }

// loaderPlatforms are the targets every published agent release must
// carry. `codefly agent ci` on a darwin/arm64 host emits exactly these
// (native darwin/arm64 + container linux/amd64), so publishing from any
// other host is missing a platform and is rejected — see
// collectLoaderAssets.
var loaderPlatforms = []platform{
	{os: "darwin", arch: "arm64"},
	{os: "linux", arch: "amd64"},
}

// checkAgentReleasePreconditions fails fast on the two things that would
// otherwise only surface AFTER the expensive CI run (or, in `publish
// all`, after earlier repos already shipped): a host that can't build
// every loader platform, and a missing gh CLI. Both are deterministic and
// side-effect free, so they are safe to run during the validate phase.
func checkAgentReleasePreconditions() error {
	if err := hostBuildsLoaderPlatforms(); err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("the gh CLI is required to upload agent release assets but is not on PATH: %w", err)
	}
	return nil
}

// hostBuildsLoaderPlatforms rejects a host that cannot produce every
// required platform. `codefly agent ci` emits exactly the native
// (host os/arch) build plus the linux/amd64 container build, so the set of
// producible targets is fully determined by the host — no need to run CI
// to discover an incapable host.
func hostBuildsLoaderPlatforms() error {
	missing := missingLoaderPlatforms(runtime.GOOS, runtime.GOARCH)
	if len(missing) > 0 {
		return fmt.Errorf(
			"this host (%s/%s) cannot build required loader platform(s) %s; publish agents from a host that can — e.g. darwin/arm64 covers darwin/arm64 plus the linux/amd64 container build",
			runtime.GOOS, runtime.GOARCH, strings.Join(missing, ", "))
	}
	return nil
}

// missingLoaderPlatforms returns the required loader platforms a host with
// the given os/arch cannot produce. CI emits the native (host) build plus
// the linux/amd64 container build, so those two targets are all a host can
// cover.
func missingLoaderPlatforms(hostOS, hostArch string) []string {
	producible := map[string]bool{
		hostOS + "/" + hostArch: true,
		"linux/amd64":           true,
	}
	var missing []string
	for _, p := range loaderPlatforms {
		if !producible[p.target()] {
			missing = append(missing, p.target())
		}
	}
	return missing
}

// loaderArchiveName is the release asset name core's downloader requests.
// It MUST stay byte-for-byte identical to manager.DownloadURL's asset
// segment; verifyReleaseAssets asserts that for the host platform.
func loaderArchiveName(name, version string, p platform) string {
	return fmt.Sprintf("service-%s_%s_%s_%s.tar.gz", name, version, p.os, p.arch)
}

func loaderSBOMName(name, version string, p platform) string {
	return fmt.Sprintf("service-%s_%s_%s_%s.cdx.json", name, version, p.os, p.arch)
}

// loaderDownloadURL mirrors manager.DownloadURL for an arbitrary platform
// (the resolver only knows the host platform). Consistency with the real
// resolver is asserted in verifyReleaseAssets for the host target.
func loaderDownloadURL(publisher, name, version string, p platform) string {
	owner := strings.ReplaceAll(publisher, ".", "-")
	return fmt.Sprintf("https://github.com/%s/service-%s/releases/download/v%s/%s",
		owner, name, version, loaderArchiveName(name, version, p))
}

// loaderAsset is one platform's staged release upload.
type loaderAsset struct {
	platform    platform
	archivePath string
	sbomPath    string // empty when CI produced no SBOM for this platform
}

// writeLoaderArchive packs the agent binary into a gzip tar at outPath
// with the single entry service-<name> — the exact path core's downloader
// extracts and executes.
func writeLoaderArchive(binaryPath, agentName, outPath string) (err error) {
	in, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open agent binary: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "service-" + agentName,
		Mode: 0o755,
		Size: info.Size(),
	}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, in); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// ciReport is the minimal slice of `codefly agent ci`'s report.json we
// need: the artifacts it produced, keyed by platform.
type ciReport struct {
	Artifacts []struct {
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		Target string `json:"target"`
	} `json:"artifacts"`
}

// collectLoaderAssets reads the agent CI report and, for every required
// platform, packages a loader-named archive and stages the SBOM into
// stageDir. It fails if CI produced no binary for a required platform —
// the guard that rejects a publish from a host that cannot build every
// loader target.
func collectLoaderAssets(ciOutput, name, version, stageDir string) ([]loaderAsset, error) {
	raw, err := os.ReadFile(filepath.Join(ciOutput, "report.json"))
	if err != nil {
		return nil, fmt.Errorf("read agent CI report: %w", err)
	}
	var report ciReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("parse agent CI report: %w", err)
	}

	binaries := map[string]string{}
	sboms := map[string]string{}
	for _, artifact := range report.Artifacts {
		abs := filepath.Join(ciOutput, filepath.FromSlash(artifact.Path))
		switch artifact.Kind {
		case "agent-binary":
			binaries[artifact.Target] = abs
		case "cyclonedx-sbom":
			sboms[artifact.Target] = abs
		}
	}

	var assets []loaderAsset
	var missing []string
	for _, p := range loaderPlatforms {
		binary, ok := binaries[p.target()]
		if !ok {
			missing = append(missing, p.target())
			continue
		}
		archivePath := filepath.Join(stageDir, loaderArchiveName(name, version, p))
		if err := writeLoaderArchive(binary, name, archivePath); err != nil {
			return nil, fmt.Errorf("package %s archive: %w", p.target(), err)
		}
		asset := loaderAsset{platform: p, archivePath: archivePath}
		if sbom, ok := sboms[p.target()]; ok {
			sbomPath := filepath.Join(stageDir, loaderSBOMName(name, version, p))
			if err := copyFile(sbom, sbomPath); err != nil {
				return nil, fmt.Errorf("stage %s SBOM: %w", p.target(), err)
			}
			asset.sbomPath = sbomPath
		}
		assets = append(assets, asset)
	}
	if len(missing) > 0 {
		produced := make([]string, 0, len(binaries))
		for target := range binaries {
			produced = append(produced, target)
		}
		sort.Strings(produced)
		return nil, fmt.Errorf(
			"agent CI did not build required platform(s) %s (produced: %s); publish must run on a host that builds every loader platform",
			strings.Join(missing, ", "), strings.Join(produced, ", "))
	}
	return assets, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// runReleaseAgentCI runs release-grade agent CI (native + linux/amd64
// build, audit, conformance) against the bumped working tree, writing
// artifacts to output. Output streams live so the operator sees progress;
// a non-zero exit fails the publish before any tag is pushed.
func runReleaseAgentCI(ctx context.Context, self, agentDir, output string) error {
	cmd := exec.CommandContext(ctx, self,
		"--timestamps=false",
		"agent", "ci",
		"--dir", agentDir,
		"--output", output,
	)
	cmd.Dir = agentDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("release-grade agent CI failed: %w", err)
	}
	return nil
}

// createAndUploadRelease publishes every staged loader archive and SBOM to
// the GitHub release for tag. gh runs from workDir so it resolves the
// repository from the origin remote.
//
// Idempotent by design: it creates the release on the first publish, or
// uploads into an existing one (clobbering same-named assets) on a retry
// or `re-tag`. Without this a re-run after a partial upload would error on
// the already-existing release, stranding a half-uploaded release.
func createAndUploadRelease(ctx context.Context, workDir, tag string, assets []loaderAsset) error {
	files := make([]string, 0, len(assets)*2)
	for _, asset := range assets {
		files = append(files, asset.archivePath)
		if asset.sbomPath != "" {
			files = append(files, asset.sbomPath)
		}
	}
	var args []string
	if releaseExists(ctx, workDir, tag) {
		args = append([]string{"release", "upload", tag}, files...)
		args = append(args, "--clobber")
	} else {
		args = append([]string{"release", "create", tag, "--title", tag, "--notes", "Release " + tag}, files...)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("publish GitHub release %s: %w", tag, err)
	}
	return nil
}

// releaseExists reports whether a GitHub release already exists for tag.
func releaseExists(ctx context.Context, workDir, tag string) bool {
	cmd := exec.CommandContext(ctx, "gh", "release", "view", tag)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// verifyReleaseAssets confirms every uploaded loader archive resolves
// through the exact URL `codefly agent install` requests. For the host
// platform it asserts the URL matches manager.DownloadURL byte-for-byte,
// so the upload names can never silently drift from the resolver.
func verifyReleaseAssets(ctx context.Context, publisher, name, version string, assets []loaderAsset) error {
	for _, asset := range assets {
		url := loaderDownloadURL(publisher, name, version, asset.platform)
		if asset.platform.os == runtime.GOOS && asset.platform.arch == runtime.GOARCH {
			agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: publisher, Name: name, Version: version}
			resolver, err := manager.DownloadURL(agent)
			if err != nil {
				return fmt.Errorf("cannot resolve install URL for %s/%s@%s: %w", publisher, name, version, err)
			}
			if resolver != url {
				return fmt.Errorf("uploaded asset URL %s does not match install resolver %s", url, resolver)
			}
		}
		if err := assertAssetReachable(ctx, url); err != nil {
			return err
		}
	}
	return nil
}

// assertAssetReachable GETs url and expects 200. GitHub can take a moment
// to make a freshly uploaded asset downloadable, so it retries a few times
// with backoff before giving up.
func assertAssetReachable(ctx context.Context, url string) error {
	const attempts = 5
	var lastErr error
	for attempt := range attempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
		}
		if attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return fmt.Errorf("release asset not reachable: %w", lastErr)
}

// agentReleaser wires the loader-asset release flow onto an Engine for a
// single agent repo. It owns the temporary CI-output and asset-staging
// directories; the caller defers cleanup after Engine.Release returns.
type agentReleaser struct {
	self      string
	agentDir  string
	workDir   string
	publisher string
	name      string
	ciOutput  string
	stageDir  string
	assets    []loaderAsset
}

type releaseGate interface {
	attach(*Engine)
	cleanup()
}

type agentIdentity struct {
	Publisher string `yaml:"publisher"`
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
}

// newAgentReleaseGate selects release behavior from the manifest kind. Service
// agents ship executable loader assets. Module agents are immutable source
// releases consumed by `codefly sync module`, so they run source/build/audit
// CI but deliberately publish only the signed Git tag.
func newAgentReleaseGate(agentDir, workDir string) (releaseGate, error) {
	identity, err := readAgentIdentity(filepath.Join(agentDir, "agent.codefly.yaml"))
	if err != nil {
		return nil, err
	}
	switch identity.Kind {
	case "", "codefly:service":
		return newAgentReleaser(agentDir, workDir)
	case "codefly:module":
		return newModuleAgentReleaser(agentDir)
	default:
		return nil, fmt.Errorf("publish does not yet define release semantics for agent kind %q", identity.Kind)
	}
}

func checkAgentReleasePreconditionsForManifest(path string) error {
	identity, err := readAgentIdentity(path)
	if err != nil {
		return err
	}
	switch identity.Kind {
	case "", "codefly:service":
		return checkAgentReleasePreconditions()
	case "codefly:module":
		return nil
	default:
		return fmt.Errorf("publish does not yet define release semantics for agent kind %q", identity.Kind)
	}
}

func newAgentReleaser(agentDir, workDir string) (*agentReleaser, error) {
	if err := checkAgentReleasePreconditions(); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve codefly executable: %w", err)
	}
	identity, err := readAgentIdentity(filepath.Join(agentDir, "agent.codefly.yaml"))
	if err != nil {
		return nil, err
	}
	ciOutput, err := os.MkdirTemp("", "codefly-publish-ci-*")
	if err != nil {
		return nil, err
	}
	stageDir, err := os.MkdirTemp("", "codefly-publish-assets-*")
	if err != nil {
		os.RemoveAll(ciOutput)
		return nil, err
	}
	return &agentReleaser{
		self:      self,
		agentDir:  agentDir,
		workDir:   workDir,
		publisher: identity.Publisher,
		name:      identity.Name,
		ciOutput:  ciOutput,
		stageDir:  stageDir,
	}, nil
}

func (r *agentReleaser) attach(engine *Engine) {
	engine.BeforeCommit = r.beforeCommit
	engine.AfterPush = r.afterPush
}

func (r *agentReleaser) cleanup() {
	os.RemoveAll(r.ciOutput)
	os.RemoveAll(r.stageDir)
}

func (r *agentReleaser) beforeCommit(ctx context.Context, newTag string) error {
	version := strings.TrimPrefix(newTag, "v")
	if err := runReleaseAgentCI(ctx, r.self, r.agentDir, r.ciOutput); err != nil {
		return err
	}
	assets, err := collectLoaderAssets(r.ciOutput, r.name, version, r.stageDir)
	if err != nil {
		return err
	}
	r.assets = assets
	return nil
}

func (r *agentReleaser) afterPush(ctx context.Context, newTag string) error {
	version := strings.TrimPrefix(newTag, "v")
	if err := createAndUploadRelease(ctx, r.workDir, newTag, r.assets); err != nil {
		return err
	}
	return verifyReleaseAssets(ctx, r.publisher, r.name, version, r.assets)
}

type moduleAgentReleaser struct {
	self     string
	agentDir string
	output   string
}

func newModuleAgentReleaser(agentDir string) (*moduleAgentReleaser, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve codefly executable: %w", err)
	}
	output, err := os.MkdirTemp("", "codefly-publish-module-ci-*")
	if err != nil {
		return nil, err
	}
	return &moduleAgentReleaser{self: self, agentDir: agentDir, output: output}, nil
}

func (r *moduleAgentReleaser) attach(engine *Engine) {
	engine.BeforeCommit = r.beforeCommit
}

func (r *moduleAgentReleaser) cleanup() {
	os.RemoveAll(r.output)
}

func (r *moduleAgentReleaser) beforeCommit(ctx context.Context, _ string) error {
	cmd := exec.CommandContext(ctx, r.self,
		"--timestamps=false",
		"agent", "ci",
		"--dir", r.agentDir,
		"--output", r.output,
		"--native-only",
		"--skip-conformance",
	)
	cmd.Dir = r.agentDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("release-grade module-agent CI failed: %w", err)
	}
	return nil
}

func readAgentIdentity(path string) (agentIdentity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentIdentity{}, fmt.Errorf("read agent manifest: %w", err)
	}
	var manifest agentIdentity
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return agentIdentity{}, fmt.Errorf("parse agent manifest: %w", err)
	}
	if manifest.Publisher == "" || manifest.Name == "" {
		return agentIdentity{}, fmt.Errorf("agent manifest %s missing publisher or name", path)
	}
	return manifest, nil
}
