package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/monorepo"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

type agentYAML struct {
	Publisher string `yaml:"publisher"`
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`

	// Quarantine marks an agent as NOT ready for the new-style architecture.
	// `agent build --all` (and `self build --with-agents`) SKIP quarantined
	// agents so a bulk rebuild only touches agents that have been migrated —
	// you don't ship half-converted plugins. An explicit single build
	// (`agent build --dir X`) still builds it, with a warning, so you can
	// iterate on the fix. QuarantineReason is shown in the skip/warn line.
	Quarantine       bool   `yaml:"quarantine"`
	QuarantineReason string `yaml:"quarantine_reason"`

	// Conformance selects how `codefly agent ci` exercises the agent's
	// Code/Runtime/Tooling lifecycle. Absent means generated-service (the
	// default): CI scaffolds a fresh service through Builder.Create. Attach-only
	// generic agents whose Builder.Create declines to invent a project template
	// declare attach-existing-source and point at a fixture workspace instead.
	Conformance *agentConformance `yaml:"conformance,omitempty"`
}

type agentConformance struct {
	Mode    string `yaml:"mode"`
	Fixture string `yaml:"fixture"`
}

// BuildCmd builds an agent binary from source and installs it locally.
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Compile an agent from source and install it for local use",
	Long: `Build compiles the agent in the current (or specified) directory and
installs the binary to ~/.codefly/agents/ so it can be loaded by the
Gateway daemon.

The directory must contain an agent.codefly.yaml with publisher, kind,
name, and version fields.

When run inside the codefly.dev monorepo, local replace directives for
wool and core are added automatically so go mod tidy succeeds without
requiring published module versions.

Use --all from the agents/services/ directory (or any parent containing
agent directories) to build every agent that has an agent.codefly.yaml.

Examples:
  cd agents/services/go-generic && codefly agent build
  codefly agent build --dir ./agents/services/go-generic
  cd agents/services && codefly agent build --all`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		all, err := cmd.Flags().GetBool("all")
		if err != nil {
			return fmt.Errorf("cannot read --all: %w", err)
		}
		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return fmt.Errorf("cannot read --dir: %w", err)
		}
		skipAudit, err := cmd.Flags().GetBool("skip-audit")
		if err != nil {
			return fmt.Errorf("cannot read --skip-audit: %w", err)
		}
		failOnVuln, err := cmd.Flags().GetBool("fail-on-vuln")
		if err != nil {
			return fmt.Errorf("cannot read --fail-on-vuln: %w", err)
		}
		jobs, err := cmd.Flags().GetInt("jobs")
		if err != nil {
			return fmt.Errorf("cannot read --jobs: %w", err)
		}
		nativeOnly, err := cmd.Flags().GetBool("native-only")
		if err != nil {
			return fmt.Errorf("cannot read --native-only: %w", err)
		}
		opts := buildOptions{skipAudit: skipAudit, failOnVuln: failOnVuln, jobs: jobs, nativeOnly: nativeOnly}

		if all {
			if dir == "" {
				dir, err = os.Getwd()
				if err != nil {
					return fmt.Errorf("cannot get working directory: %w", err)
				}
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("cannot resolve directory: %w", err)
			}
			if err := buildAllAgents(ctx, absDir, opts); err != nil {
				return fmt.Errorf("build --all failed: %w", err)
			}
			return nil
		}

		if dir == "" {
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot get working directory: %w", err)
			}
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("cannot resolve directory: %w", err)
		}

		if err := buildAgent(ctx, absDir, opts); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		return nil
	},
}

type buildOptions struct {
	skipAudit  bool
	failOnVuln bool
	// jobs caps how many agents `--all` builds concurrently. <= 0 means
	// runtime.NumCPU(). Ignored for a single-agent build.
	jobs int
	// nativeOnly skips the Linux/amd64 container cross-build, producing only
	// the host-platform binary. The container binary is only needed for
	// Docker-mode runs, so local mac/native dev doesn't require it.
	nativeOnly bool
}

// BuildOptions is the exported form of buildOptions for callers in other
// packages (e.g. `codefly self build --with-agents`).
type BuildOptions struct {
	SkipAudit  bool
	FailOnVuln bool
	Jobs       int
	NativeOnly bool
}

// BuildAllAgents builds every agent under root. It is the exported entry point
// for `codefly self build --with-agents`, so one command rebuilds the CLI and
// all agents together. root is typically <monorepo>/agents/services.
func BuildAllAgents(ctx context.Context, root string, opts BuildOptions) error {
	return buildAllAgents(ctx, root, buildOptions{skipAudit: opts.SkipAudit, failOnVuln: opts.FailOnVuln, jobs: opts.Jobs, nativeOnly: opts.NativeOnly})
}

func init() {
	BuildCmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	BuildCmd.Flags().Bool("all", false, "Build all agents found in the current directory tree")
	BuildCmd.Flags().Bool("skip-audit", false, "Explicitly skip the post-build vulnerability audit")
	BuildCmd.Flags().Bool("fail-on-vuln", false, "Fail the build if any HIGH/CRITICAL vulnerability is found")
	BuildCmd.Flags().IntP("jobs", "j", 0, "Max agents to build in parallel with --all (default: number of CPUs)")
	BuildCmd.Flags().Bool("native-only", false, "Build only the host-platform binary; skip the Linux/amd64 container cross-build (local dev fast path)")
}

// quarantinedAgent records an agent skipped by a bulk build because its
// manifest sets quarantine: true.
type quarantinedAgent struct {
	name   string
	reason string
}

// buildAllAgents discovers all directories containing agent.codefly.yaml
// under root and builds each one — SKIPPING agents whose manifest marks them
// quarantine: true (not yet migrated to the new style). Quarantined agents are
// reported so a bulk rebuild is honest about what it did and didn't touch.
func buildAllAgents(ctx context.Context, root string, opts buildOptions) error {
	var agents []string
	var quarantined []quarantinedAgent
	var discoveryFailures []error
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		yamlPath := filepath.Join(dir, "agent.codefly.yaml")
		data, rerr := os.ReadFile(yamlPath)
		if rerr != nil {
			if !os.IsNotExist(rerr) {
				discoveryFailures = append(discoveryFailures, fmt.Errorf("read %s: %w", yamlPath, rerr))
			}
			continue // not an agent directory
		}
		var ag agentYAML
		if uerr := yaml.Unmarshal(data, &ag); uerr != nil {
			// A malformed manifest must NOT silently fall through to the
			// buildable list — that could build an agent meant to be
			// quarantined (whose quarantine flag we just failed to read). Skip
			// it loudly so a broken manifest is visible, not built.
			cli.Info("  ⚠ skipping %s — unreadable agent.codefly.yaml: %v", e.Name(), uerr)
			discoveryFailures = append(discoveryFailures, fmt.Errorf("parse %s: %w", yamlPath, uerr))
			continue
		}
		if ag.Quarantine {
			quarantined = append(quarantined, quarantinedAgent{name: e.Name(), reason: ag.QuarantineReason})
			continue
		}
		agents = append(agents, dir)
	}

	if len(quarantined) > 0 {
		cli.Header(2, "Skipping %d quarantined agent(s) (not migrated to the new style):", len(quarantined))
		for _, q := range quarantined {
			if q.reason != "" {
				cli.Info("  - %s — %s", q.name, q.reason)
			} else {
				cli.Info("  - %s", q.name)
			}
		}
		cli.Info("  (build one explicitly with `codefly agent build --dir <path>` to work on it)")
	}

	if len(agents) == 0 {
		if len(quarantined) > 0 {
			return errors.Join(append(discoveryFailures, fmt.Errorf("no buildable agents under %s — all %d are quarantined", root, len(quarantined)))...)
		}
		return errors.Join(append(discoveryFailures, fmt.Errorf("no agent directories found under %s", root))...)
	}
	// Builder.Package is intentionally owned by the Go source plugin, including
	// when that plugin packages its own release. A clean machine cannot enter
	// that cycle until it has one native Go packager, so seed the exact version
	// from the discovered source before starting parallel plugin builds. Every
	// release artifact (including the Go plugin itself) is still produced by the
	// ordinary Builder.Package path below.
	if err := ensureSourcePackager(ctx, agents); err != nil {
		return errors.Join(append(discoveryFailures, err)...)
	}

	cli.Header(1, "Building %d agents", len(agents))

	// Run-wide boilerplate, hoisted out of the per-agent path so it prints once
	// instead of once per agent.
	if monoRoot := findMonorepoRoot(root); monoRoot != "" {
		cli.Info("Monorepo detected at %s", monoRoot)
	}

	jobs := opts.jobs
	if jobs <= 0 {
		jobs = runtime.NumCPU()
	}
	if jobs > len(agents) {
		jobs = len(agents)
	}
	if jobs > 1 {
		cli.Info("Building up to %d agents in parallel", jobs)
	}

	// Each build is dispatched to the source plugin. The plugin owns its
	// language toolchain and Codefly schedules independent resources in parallel.
	results := make([]*agentBuildResult, len(agents))
	var completed atomic.Int64

	started := time.Now()
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for i := range agents {
		if err := groupCtx.Err(); err != nil {
			results[i] = &agentBuildResult{label: filepath.Base(agents[i]), dir: agents[i], err: err}
			continue
		}
		g.Go(func() error {
			log := &agentLogger{}
			res := compileAgent(groupCtx, agents[i], log, opts.nativeOnly)
			results[i] = res
			n := completed.Add(1)
			if res.err != nil {
				cli.Error("[%d/%d] %s ✗ %v", n, len(agents), res.label, res.err)
				log.flush() // surface the buffered build output so failures are debuggable
			} else {
				cli.Info("[%d/%d] %s", n, len(agents), res.summary())
			}
			return nil
		})
	}
	_ = g.Wait()
	elapsed := time.Since(started)

	// Audits stay serialized after plugin-owned packaging. Builder.Package has
	// already emitted and installed release-bound CycloneDX evidence;
	// --skip-audit waives only the vulnerability gate.
	for _, res := range results {
		if res.err != nil {
			continue
		}
		if !opts.skipAudit {
			if err := runAudit(ctx, res.dir, res.ag, opts.failOnVuln); err != nil {
				res.err = err
			}
		}
	}

	var failed []string
	var seq time.Duration
	built := 0
	for _, res := range results {
		if res.err != nil {
			failed = append(failed, res.label)
			continue
		}
		built++
		seq += res.native + res.linux
	}

	cli.Header(1, "Build summary")
	if len(failed) > 0 {
		for _, res := range results {
			if res.err != nil {
				cli.Error("  ✗ %s — %v", res.label, res.err)
			}
		}
	}
	speedup := ""
	if elapsed > 0 && seq > elapsed {
		speedup = fmt.Sprintf(", ~%.1f× faster than sequential (%s)",
			float64(seq)/float64(elapsed), seq.Round(100*time.Millisecond))
	}
	cli.Info("  %d built, %d failed in %s%s", built, len(failed), elapsed.Round(100*time.Millisecond), speedup)

	if len(failed) > 0 {
		discoveryFailures = append(discoveryFailures, fmt.Errorf("%d agent(s) failed to build: %s", len(failed), strings.Join(failed, ", ")))
	}
	if len(discoveryFailures) > 0 {
		return errors.Join(discoveryFailures...)
	}

	if len(quarantined) > 0 {
		cli.Header(1, "%d agents built successfully (%d quarantined, skipped)", len(agents), len(quarantined))
	} else {
		cli.Header(1, "All %d agents built successfully", len(agents))
	}
	return nil
}

// findMonorepoRoot walks up from dir looking for the codefly.dev monorepo
// root, identified by a core/ subdir that is the codefly core module. (The
// standalone top-level wool/ module was removed — core/wool is now a package
// inside core — so requiring a wool/ dir here, as the old code did, made this
// ALWAYS return "" and silently disabled local-core replace detection.)
func findMonorepoRoot(dir string) string {
	return monorepo.FindRoot(dir)
}

// agentLogger collects one agent's build narration. In direct mode (a single
// `agent build`) lines and command output stream straight to the terminal as
// before. In buffered mode (the parallel `--all` path) they are held so each
// agent's output can be flushed as one uninterrupted block on failure, instead
// of interleaving into mush with sibling builds.
type agentLogger struct {
	direct bool

	mu    sync.Mutex
	lines []string
}

func (l *agentLogger) Info(format string, args ...any) {
	if l.direct {
		cli.Info(format, args...)
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *agentLogger) Header(level int, format string, args ...any) {
	if l.direct {
		cli.Header(level, format, args...)
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *agentLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		cli.Info("%s", line)
	}
}

// agentBuildResult is the outcome of compiling one agent.
type agentBuildResult struct {
	label         string // source dir base name, used before the manifest is parsed
	dir           string
	ag            agentYAML
	native        time.Duration
	linux         time.Duration
	linuxFailed   bool
	linuxSkipped  bool
	nativePath    string
	containerPath string
	err           error
}

// summary is the one-line success digest, e.g. "go:0.0.7 ✓ darwin 8.7s · linux 3.6s".
func (r *agentBuildResult) summary() string {
	linux := r.linux.String()
	switch {
	case r.linuxSkipped:
		linux = "skipped"
	case r.linuxFailed:
		linux = "✗"
	}
	return fmt.Sprintf("%s:%s ✓ %s %s · linux %s", r.ag.Name, r.ag.Version, runtime.GOOS, r.native, linux)
}

func buildAgent(ctx context.Context, dir string, opts buildOptions) error {
	res := compileAgent(ctx, dir, &agentLogger{direct: true}, opts.nativeOnly)
	if res.err != nil {
		return res.err
	}
	if !opts.skipAudit {
		return runAudit(ctx, dir, res.ag, opts.failOnVuln)
	}
	return nil
}

// compileAgent asks the selected source plugin to package native and Linux
// artifacts. It never selects a language command and never runs audit; callers
// apply release policy to the separate Builder.Audit response. The returned
// result always carries the outcome. When nativeOnly is set the container
// target is omitted.
func compileAgent(ctx context.Context, dir string, log *agentLogger, nativeOnly bool) *agentBuildResult {
	res := &agentBuildResult{label: filepath.Base(dir), dir: dir}
	if err := ctx.Err(); err != nil {
		res.err = err
		return res
	}

	yamlPath := filepath.Join(dir, "agent.codefly.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		res.err = fmt.Errorf("read agent.codefly.yaml in %s: %w (is this an agent directory?)", dir, err)
		return res
	}

	var ag agentYAML
	if err := yaml.Unmarshal(data, &ag); err != nil {
		res.err = fmt.Errorf("parse agent.codefly.yaml: %w", err)
		return res
	}
	if ag.Name == "" || ag.Version == "" || ag.Publisher == "" {
		res.err = fmt.Errorf("agent.codefly.yaml must have publisher, name, and version")
		return res
	}
	res.ag = ag
	if err := ensureSourcePackager(ctx, sourcePackagerCandidates(dir)); err != nil {
		res.err = err
		return res
	}
	// A quarantined agent is excluded from bulk builds; an explicit single
	// build still proceeds (that's how you fix it) but says so loudly.
	if ag.Quarantine {
		if ag.QuarantineReason != "" {
			log.Info("⚠ %s is QUARANTINED (%s) — building anyway because you asked for it explicitly", ag.Name, ag.QuarantineReason)
		} else {
			log.Info("⚠ %s is QUARANTINED — building anyway because you asked for it explicitly", ag.Name)
		}
	}

	subdir := "services"
	if ag.Kind == "codefly:application" {
		subdir = "applications"
	} else if ag.Kind == "codefly:module" {
		subdir = "modules"
	}

	codeflyHome := resources.CodeflyHomeDir()
	nativeDir := filepath.Join(codeflyHome, "agents", subdir, ag.Publisher)
	binaryName := fmt.Sprintf("%s__%s", ag.Name, ag.Version)
	nativePath := filepath.Join(nativeDir, binaryName)
	res.nativePath = nativePath
	if !nativeOnly {
		res.containerPath = filepath.Join(codeflyHome, "containers", "agents", subdir, ag.Publisher, binaryName)
	} else {
		res.linuxSkipped = true
	}

	temporary, err := os.MkdirTemp("", "codefly-agent-package-*")
	if err != nil {
		res.err = err
		return res
	}
	defer os.RemoveAll(temporary)
	prepared, err := sourceworkspace.Prepare(ctx, dir)
	if err != nil {
		res.err = err
		return res
	}
	defer prepared.Close()
	executable, err := os.Executable()
	if err != nil {
		res.err = fmt.Errorf("resolve Codefly executable: %w", err)
		return res
	}
	packageOutput := filepath.Join(temporary, "artifacts")
	arguments := []string{
		"--timestamps=false",
		"package", "service", "source",
		"--format", "json",
		"--output-dir", packageOutput,
		"--name", binaryName,
		"--publisher", ag.Publisher,
		"--subject-name", ag.Name,
		"--subject-version", ag.Version,
		"--target", runtime.GOOS + "/" + runtime.GOARCH,
	}
	if !nativeOnly {
		arguments = append(arguments, "--target", "linux/amd64")
	}
	started := time.Now()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = prepared.Dir
	command.Env = agentCIChildEnvironment(resolveSourcePluginHome(), "CI=1", "CODEFLY_COLOR=never")
	output, err := command.CombinedOutput()
	if err != nil {
		res.err = fmt.Errorf("plugin-owned agent packaging: %w\n%s", err, boundedAgentCIOutput(output))
		return res
	}
	response := &builderv0.PackageResponse{}
	if err := protojson.Unmarshal(bytes.TrimSpace(output), response); err != nil {
		res.err = fmt.Errorf("decode Builder.Package response: %w\n%s", err, boundedAgentCIOutput(output))
		return res
	}
	if response.GetState().GetState() != builderv0.PackageStatus_SUCCESS {
		res.err = fmt.Errorf("Builder.Package failed: %s", response.GetState().GetMessage())
		return res
	}
	if err := installAgentPackageArtifacts(response.GetArtifacts(), res); err != nil {
		res.err = err
		return res
	}
	elapsed := time.Since(started).Round(100 * time.Millisecond)
	res.native = elapsed
	if !nativeOnly {
		res.linux = elapsed
	}
	log.Info("Builder.Package emitted %d artifacts in %s", len(response.GetArtifacts()), elapsed)
	log.Info("Installed: %s", res.nativePath)
	if res.containerPath != "" {
		log.Info("Installed (container): %s", res.containerPath)
	}
	log.Header(1, "Agent %s:%s packaged successfully through codefly.dev/go", ag.Name, ag.Version)
	return res
}

const genericGoPluginPublisher = "codefly.dev"

var bootstrapSourcePackager = buildSourcePackager

func sourcePackagerPath(home string) string {
	return filepath.Join(
		home,
		"agents", "services", genericGoPluginPublisher,
		"go__"+sourceworkspace.GenericGoPluginVersion,
	)
}

func sourcePackagerCandidates(dir string) []string {
	candidates := []string{dir}
	if root := findMonorepoRoot(dir); root != "" {
		goDir := filepath.Join(root, "service-go")
		if goDir != dir {
			candidates = append(candidates, goDir)
		}
	}
	return candidates
}

// ensureSourcePackager breaks the sole bootstrap cycle in plugin-owned agent
// packaging. It installs only a native seed binary and only when the exact Go
// plugin version selected by sourceworkspace is absent. The caller then uses
// that plugin's normal Builder.Package operation to create the real native,
// Linux, and SBOM release artifacts.
func ensureSourcePackager(ctx context.Context, agentDirs []string) error {
	destination := sourcePackagerPath(resources.CodeflyHomeDir())
	if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return nil
	}

	var found []string
	for _, dir := range agentDirs {
		data, err := os.ReadFile(filepath.Join(dir, "agent.codefly.yaml"))
		if err != nil {
			continue
		}
		var ag agentYAML
		if err := yaml.Unmarshal(data, &ag); err != nil {
			continue
		}
		if ag.Publisher != genericGoPluginPublisher || ag.Name != "go" {
			continue
		}
		found = append(found, ag.Publisher+"/"+ag.Name+":"+ag.Version)
		if ag.Version != sourceworkspace.GenericGoPluginVersion {
			continue
		}
		cli.Info("Bootstrapping native source packager %s/go:%s", genericGoPluginPublisher, sourceworkspace.GenericGoPluginVersion)
		if err := bootstrapSourcePackager(ctx, dir, destination); err != nil {
			return fmt.Errorf("bootstrap %s/go:%s from %s: %w", genericGoPluginPublisher, sourceworkspace.GenericGoPluginVersion, dir, err)
		}
		return nil
	}

	detail := "no Go agent source was found"
	if len(found) > 0 {
		detail = "available Go agent source is " + strings.Join(found, ", ")
	}
	return fmt.Errorf(
		"source packager %s/go:%s is not installed at %s and cannot be bootstrapped: %s",
		genericGoPluginPublisher,
		sourceworkspace.GenericGoPluginVersion,
		destination,
		detail,
	)
}

func buildSourcePackager(ctx context.Context, sourceDir, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create agent directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codefly-go-bootstrap-*")
	if err != nil {
		return fmt.Errorf("create temporary executable: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close temporary executable: %w", err)
	}
	defer os.Remove(temporaryPath)

	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", temporaryPath, ".")
	command.Dir = sourceDir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("native Go bootstrap build: %w\n%s", err, boundedAgentCIOutput(output))
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		return fmt.Errorf("mark bootstrap executable: %w", err)
	}
	file, err := os.Open(temporaryPath)
	if err != nil {
		return fmt.Errorf("open bootstrap executable: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync bootstrap executable: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close bootstrap executable: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("install bootstrap executable: %w", err)
	}
	return nil
}

func resolveSourcePluginHome() string {
	current := resources.CodeflyHomeDir()
	plugin := sourcePackagerPath(current)
	if _, err := os.Stat(plugin); err == nil {
		return current
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return current
	}
	defaultHome := filepath.Join(home, ".codefly")
	if _, err := os.Stat(sourcePackagerPath(defaultHome)); err == nil {
		return defaultHome
	}
	return current
}

func installAgentPackageArtifacts(artifacts []*builderv0.PackageArtifact, result *agentBuildResult) error {
	installedNative := false
	installedLinux := result.containerPath == ""
	for _, artifact := range artifacts {
		target := artifact.GetTarget().GetOs() + "/" + artifact.GetTarget().GetArchitecture()
		destinations := []string{}
		if target == runtime.GOOS+"/"+runtime.GOARCH {
			if artifact.GetKind() == builderv0.PackageArtifact_EXECUTABLE {
				destinations = append(destinations, result.nativePath)
				installedNative = true
			} else if artifact.GetKind() == builderv0.PackageArtifact_SBOM {
				destinations = append(destinations, result.nativePath+".cdx.json")
			}
		}
		if result.containerPath != "" && target == "linux/amd64" {
			if artifact.GetKind() == builderv0.PackageArtifact_EXECUTABLE {
				destinations = append(destinations, result.containerPath)
				installedLinux = true
			} else if artifact.GetKind() == builderv0.PackageArtifact_SBOM {
				destinations = append(destinations, result.containerPath+".cdx.json")
			}
		}
		for _, destination := range destinations {
			if err := copyAgentCIFile(artifact.GetPath(), destination); err != nil {
				return fmt.Errorf("install package artifact %s: %w", destination, err)
			}
		}
	}
	if !installedNative {
		return fmt.Errorf("Builder.Package returned no executable for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if !installedLinux {
		return fmt.Errorf("Builder.Package returned no executable for linux/amd64")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codefly-sbom-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// runAudit invokes the source resource's ordinary Builder.Audit operation.
// The plugin owns scanner/toolchain execution; this caller only applies the
// agent-release suppression and gating policy to the typed response.
//
// Findings matching IDs in a workspace-root .govulncheck.yaml are filtered
// out and reported separately as "suppressed (reviewed)".
func runAudit(ctx context.Context, dir string, ag agentYAML, failOnVuln bool) error {
	cli.Header(1, "Auditing %s:%s for vulnerabilities", ag.Name, ag.Version)
	res, err := runAgentSourceAudit(ctx, dir)
	if err != nil {
		return fmt.Errorf("audit could not complete (use --skip-audit for an explicit waiver): %w", err)
	}
	return applyAgentAuditPolicy(dir, ag, res, failOnVuln)
}

func applyAgentAuditPolicy(dir string, ag agentYAML, res *builderv0.AuditResponse, failOnVuln bool) error {
	if res == nil {
		return fmt.Errorf("Builder.Audit returned no response")
	}
	cli.Info("Tool: %s", res.Tool)

	suppressions := loadSuppressions(dir)

	// Partition findings: suppressed (workspace policy), actionable
	// (upstream has a patch), unpatched (upstream has no fix yet).
	// --fail-on-vuln only blocks on actionable.
	var actionable, unpatched, suppressed []*builderv0.AuditFinding
	for _, f := range res.Findings {
		if _, ok := suppressions[f.Id]; ok {
			suppressed = append(suppressed, f)
			continue
		}
		if f.FixedVersion == "" {
			unpatched = append(unpatched, f)
		} else {
			actionable = append(actionable, f)
		}
	}

	if len(actionable)+len(unpatched) == 0 {
		if len(suppressed) > 0 {
			cli.Info("No actionable vulnerabilities. %d suppressed by .govulncheck.yaml (reviewed).", len(suppressed))
		} else {
			cli.Info("No vulnerabilities found.")
		}
	} else {
		if len(actionable) > 0 {
			cli.Info("Vulnerabilities with available fixes: %d", len(actionable))
			for _, f := range actionable {
				cli.Info("  [%s] %s %s@%s → fixed in %s (%s)",
					severityLabel(f.Severity), f.Id, f.Package, f.CurrentVersion,
					f.FixedVersion, truncate(f.Summary, 80))
			}
		}
		if len(unpatched) > 0 {
			cli.Info("Unpatched upstream (tracked, no action available): %d", len(unpatched))
			for _, f := range unpatched {
				cli.Info("  [%s] %s %s@%s — no upstream fix yet (%s)",
					severityLabel(f.Severity), f.Id, f.Package, f.CurrentVersion,
					truncate(f.Summary, 80))
			}
		}
		if len(suppressed) > 0 {
			cli.Info("Suppressed (reviewed via .govulncheck.yaml): %d", len(suppressed))
		}
	}
	if len(res.Outdated) > 0 {
		cli.Info("Outdated packages: %d (run `codefly upgrade agent --dry-run` to preview)",
			len(res.Outdated))
	}

	if failOnVuln {
		// Only block on actionable findings — unpatched upstream vulns can't
		// be resolved by a rebuild, so failing the build on them just blocks
		// development without improving security.
		var blockers int
		for _, f := range actionable {
			if f.Severity == builderv0.AuditFinding_HIGH ||
				f.Severity == builderv0.AuditFinding_CRITICAL {
				blockers++
			}
		}
		if blockers > 0 {
			return fmt.Errorf("audit failed: %d high/critical vulnerability(ies) with available upstream fixes in %s:%s (use --skip-audit to bypass)",
				blockers, ag.Name, ag.Version)
		}
	}
	return nil
}

// runAgentSourceAudit adapts an agent repository into a normal Codefly source
// resource and asks its selected plugin to perform Builder.Audit. No language
// command or scanner is selected by the CLI.
func runAgentSourceAudit(ctx context.Context, dir string) (*builderv0.AuditResponse, error) {
	prepared, err := sourceworkspace.Prepare(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer prepared.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Codefly executable: %w", err)
	}
	command := exec.CommandContext(ctx, executable,
		"--timestamps=false",
		"audit", "service", "source",
		"--json",
		"--outdated=true",
		"--fail-on-vuln=false",
	)
	command.Dir = prepared.Dir
	command.Env = agentCIChildEnvironment(resolveSourcePluginHome(), "CI=1", "CODEFLY_COLOR=never")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("plugin-owned Builder.Audit: %w\n%s", err, boundedAgentCIOutput(output))
	}
	response := &builderv0.AuditResponse{}
	if err := protojson.Unmarshal(bytes.TrimSpace(output), response); err != nil {
		return nil, fmt.Errorf("decode Builder.Audit response: %w\n%s", err, boundedAgentCIOutput(output))
	}
	if response.GetState() == nil {
		return nil, fmt.Errorf("Builder.Audit returned no status")
	}
	switch response.GetState().GetState() {
	case builderv0.AuditStatus_CLEAN, builderv0.AuditStatus_FINDINGS:
		return response, nil
	default:
		return nil, fmt.Errorf("Builder.Audit failed: %s", response.GetState().GetMessage())
	}
}

// loadSuppressions walks up from dir looking for a .govulncheck.yaml and
// returns the set of suppressed vuln IDs. Missing file or parse errors are
// silently ignored — suppressions are optional, not required.
func loadSuppressions(dir string) map[string]struct{} {
	cur := dir
	for {
		p := filepath.Join(cur, ".govulncheck.yaml")
		data, err := os.ReadFile(p)
		if err == nil {
			var doc struct {
				Suppressions []struct {
					ID string `yaml:"id"`
				} `yaml:"suppressions"`
			}
			if yaml.Unmarshal(data, &doc) == nil {
				m := make(map[string]struct{}, len(doc.Suppressions))
				for _, s := range doc.Suppressions {
					if s.ID != "" {
						m[s.ID] = struct{}{}
					}
				}
				return m
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

func severityLabel(s builderv0.AuditFinding_Severity) string {
	switch s {
	case builderv0.AuditFinding_CRITICAL:
		return "CRITICAL"
	case builderv0.AuditFinding_HIGH:
		return "HIGH"
	case builderv0.AuditFinding_MEDIUM:
		return "MEDIUM"
	case builderv0.AuditFinding_LOW:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
