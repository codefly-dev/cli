package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/executionplan"
	"github.com/codefly-dev/core/resources"
)

type ReplayPlan struct {
	Schema      string           `json:"schema"`
	Candidate   string           `json:"candidate"`
	Content     string           `json:"content"`
	Selection   *Plan            `json:"selection"`
	Fingerprint string           `json:"fingerprint"`
	Invocation  ReplayInvocation `json:"invocation"`
	Tasks       []ReplayTask     `json:"tasks"`
}

type ReplayInvocation struct {
	Phases         []string `json:"phases"`
	Suites         []string `json:"suites"`
	RuntimeContext string   `json:"runtime_context"`
}

type ReplayTask struct {
	Stage         resources.Stage `json:"stage"`
	Phase         string          `json:"phase"`
	Suite         string          `json:"suite,omitempty"`
	Service       PlannedService  `json:"service"`
	Prerequisites []string        `json:"prerequisites"`
	Resources     []string        `json:"resources"`
}

func buildReplayPlan(ctx context.Context, workspace *resources.Workspace, plan *Plan, invocation ReplayInvocation) (*ReplayPlan, error) {
	phases, err := normalizeRunPhases(invocation.Phases)
	if err != nil {
		return nil, err
	}
	invocation.Phases = phases
	invocation.Suites = normalizeTestSuites(invocation.Suites)
	invocation.RuntimeContext = normalizedCacheRuntimeContext(invocation.RuntimeContext)
	root, err := gitRoot(ctx, workspace.Dir())
	if err != nil {
		return nil, err
	}
	revision, err := gitOutput(ctx, root, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	candidate := strings.TrimSpace(string(revision))
	if plan.Head != "" {
		head, headErr := gitOutput(ctx, root, "rev-parse", "--verify", plan.Head+"^{commit}")
		if headErr != nil {
			return nil, headErr
		}
		if strings.TrimSpace(string(head)) != candidate {
			return nil, fmt.Errorf("plan head is not the checked-out candidate")
		}
	}
	content, err := replayInputs(ctx, workspace, root)
	if err != nil {
		return nil, err
	}
	result := &ReplayPlan{Schema: "codefly.ci-replay/v3", Invocation: invocation, Tasks: []ReplayTask{}, Candidate: candidate, Content: content, Selection: plan}
	result.Tasks, err = resolveReplayTasks(ctx, workspace, plan, invocation)
	if err != nil {
		return nil, err
	}
	if err = verifyReplayVisibility(ctx, workspace, plan, invocation.Phases); err != nil {
		return nil, err
	}
	for _, selected := range plan.Services {
		closure, err := architecture.SelectClosure(ctx, workspace, selected.Service)
		if err != nil {
			return nil, err
		}
		draft, err := closure.Draft(ctx, architecture.PlanOptions{Phase: executionplan.PhaseBuild, StatePolicy: executionplan.StatePolicy{Lifecycle: executionplan.LifecycleStop}})
		if err != nil {
			return nil, err
		}
		if len(draft.SchemaSteps) > 0 {
			return nil, fmt.Errorf("CI replay has no executor for schema job %s", draft.SchemaSteps[0].ID)
		}
		// Core's draft describes the union, including valid mixed-stage cycles.
		// Validate its resource metadata here. Ordering is owned exclusively by
		// resolveScheduledTasks, which validates Core's stage graphs and supplies
		// the exact prerequisite edges executed and fingerprinted below.
		draft.Edges = nil
		if err := draft.Validate(); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(result.Tasks)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	result.Fingerprint = "sha256:" + hex.EncodeToString(digest[:])
	return result, nil
}

// verifyReplayVisibility judges the replay's declared dependencies with the
// stage-scoped pass run, deploy and validate already share, so replaying a
// recorded selection cannot refuse a composition every other command accepts.
// The selection seeds it, not the resolved tasks: a phase that schedules no
// service task still replays a composition, and one that judges nothing is a
// gate that passes everything.
func verifyReplayVisibility(ctx context.Context, workspace *resources.Workspace, plan *Plan, phases []string) error {
	var seeds []string
	for _, selected := range plan.Services {
		ref, err := resources.ParseServiceWithOptionalModule(selected.Service)
		if err != nil {
			return err
		}
		if !slices.Contains(seeds, ref.Module) {
			seeds = append(seeds, ref.Module)
		}
	}
	if len(seeds) == 0 {
		return nil
	}
	for _, stage := range replayStages(phases) {
		closure, err := workspace.ResolveModuleClosure(ctx, stage, seeds)
		if err != nil {
			return err
		}
		if err := closure.ValidateServiceDependencies(ctx); err != nil {
			return err
		}
	}
	return nil
}

// replayStages returns the stages the requested phases exercise. Every CI phase
// builds what it touches, so the build stage is always judged; a phase that
// locks the dependency closure is one that also starts the service, which is
// what brings the run stage in. Verify schedules no service task at all, so it
// has no stage to scope to and judges every edge, exactly as Core's
// workspace-wide pass does for the same reason.
//
// scheduleStage is deliberately not consulted: it names the single graph a
// phase's tasks are ordered in, and Core sorts test's build and run graphs
// separately, so reading it as the only stage a phase touches would leave a
// test replay never judging the build edges it builds through.
func replayStages(phases []string) []resources.Stage {
	for _, phase := range phases {
		if phaseLocksDependencyClosure(phase) {
			return resources.Stages()
		}
	}
	return []resources.Stage{resources.StageBuild}
}

func resolveReplayTasks(ctx context.Context, workspace *resources.Workspace, plan *Plan, invocation ReplayInvocation) ([]ReplayTask, error) {
	tasks := []ReplayTask{}
	for _, phase := range invocation.Phases {
		suites := []string{""}
		if phase == string(resources.PhaseTest) {
			suites = invocation.Suites
		}
		for _, suite := range suites {
			options := ScheduleOptions{Phase: phase, Suite: suite, LockDependencyClosure: phaseLocksDependencyClosure(phase)}
			scheduled, taskErr := resolveScheduledTasks(ctx, workspace, plan, options)
			if taskErr != nil {
				return nil, taskErr
			}
			for index := range scheduled {
				task := &scheduled[index]
				tasks = append(tasks, ReplayTask{Stage: scheduleStage(options), Phase: phase, Suite: suite, Service: task.planned, Prerequisites: task.prerequisites, Resources: task.resources})
			}
		}
	}
	return tasks, nil
}

func replayInputs(ctx context.Context, workspace *resources.Workspace, root string) (string, error) {
	inventory, modules, err := loadPlanInventory(ctx, workspace)
	if err != nil {
		return "", err
	}
	inputs := []cacheDigestPath{{Label: "repository", Path: root}}
	for _, module := range modules {
		inputs = append(inputs, cacheDigestPath{Label: "module/" + module.name, Path: module.dir})
	}
	for _, service := range inventory {
		inputs = append(inputs, cacheDigestPath{Label: "service/" + service.unique, Path: service.dir})
	}
	libraries, err := workspace.LoadLibraries(ctx)
	if err != nil {
		return "", err
	}
	for _, library := range libraries {
		inputs = append(inputs, cacheDigestPath{Label: "library/" + library.Name, Path: library.Dir()})
	}
	snapshot := &replaySnapshot{digests: map[string]string{}}
	output := filepath.Join(workspace.Dir(), ".codefly", "ci")
	tracked, err := gitOutput(ctx, root, "ls-files", "--", output)
	if err != nil {
		return "", err
	}
	info, statErr := os.Lstat(output)
	if len(tracked) == 0 && (os.IsNotExist(statErr) || (statErr == nil && info.IsDir())) {
		snapshot.output = cleanAbs(output)
	}
	digests := make([]CICacheResourceDigest, 0, len(inputs))
	for _, input := range inputs {
		digest, hashErr := snapshot.digest(ctx, input.Path)
		if hashErr != nil {
			return "", fmt.Errorf("hash %s: %w", input.Label, hashErr)
		}
		digests = append(digests, CICacheResourceDigest{Resource: input.Label, Digest: digest})
	}
	return aggregateCacheDigests(digests), nil
}

// Directory digests form a graph: legitimate package-manager back-links must
// not cause infinite traversal or make a reproducible source tree unhashable.
type replaySnapshot struct {
	digests map[string]string
	output  string
}

func (snapshot *replaySnapshot) skipOutput(path string) (bool, error) {
	if snapshot.output == "" {
		return false, nil
	}
	if filepath.Clean(path) == snapshot.output {
		return true, nil
	}
	if filepath.Clean(path) != filepath.Dir(snapshot.output) {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(snapshot.output) {
			return false, nil
		}
	}
	return true, nil
}

func (snapshot *replaySnapshot) digest(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if digest, visited := snapshot.digests[resolved]; visited {
		return digest, nil
	}
	snapshot.digests[resolved] = "back-reference"
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("unsupported source file type: %s", path)
		}
		if err := hashCacheEntry(hasher, resolved, resolved); err != nil {
			return "", err
		}
	} else {
		entries, readErr := os.ReadDir(resolved)
		if readErr != nil {
			return "", readErr
		}
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			child := filepath.Join(resolved, entry.Name())
			skip, skipErr := snapshot.skipOutput(child)
			if skipErr != nil {
				return "", skipErr
			}
			if skip {
				continue
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if err := hashCacheEntry(hasher, resolved, child); err != nil {
					return "", err
				}
			}
			digest, digestErr := snapshot.digest(ctx, child)
			if digestErr != nil {
				return "", digestErr
			}
			writeCacheRecord(hasher, entry.Name(), digest)
		}
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	snapshot.digests[resolved] = digest
	return digest, nil
}

func replayContent(ctx context.Context, root string) (string, error) {
	snapshot := &replaySnapshot{digests: map[string]string{}}
	return snapshot.digest(ctx, root)
}

func readReplayPlan(ctx context.Context, workspace *resources.Workspace, path string, options *PlanOptions, invocation ReplayInvocation) (*Plan, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var submitted ReplayPlan
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&submitted); decodeErr != nil {
		return nil, fmt.Errorf("decode CI replay plan: %w", decodeErr)
	}
	if trailingErr := decoder.Decode(new(any)); trailingErr != io.EOF {
		return nil, fmt.Errorf("CI replay plan must contain one JSON document")
	}
	if submitted.Schema != "codefly.ci-replay/v3" || submitted.Selection == nil {
		return nil, fmt.Errorf("incompatible CI replay plan")
	}
	// Selection is recomputed from independently supplied bounds, never from the
	// submitted services, reasons, or changed paths.
	if options.Base == "" && len(options.ChangedFiles) == 0 && !options.All {
		return nil, fmt.Errorf("--plan requires independent --base, --changed-file, or --all selection bounds")
	}
	expected, err := BuildPlan(ctx, workspace, *options)
	if err != nil {
		return nil, err
	}
	current, err := buildReplayPlan(ctx, workspace, expected, invocation)
	if err != nil {
		return nil, err
	}
	if submitted.Candidate != current.Candidate {
		return nil, fmt.Errorf("stale CI plan: candidate revision changed")
	}
	if submitted.Content != current.Content {
		return nil, fmt.Errorf("stale CI plan: local source contents changed")
	}
	expectedJSON, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	submittedJSON, err := json.Marshal(&submitted)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(submittedJSON, expectedJSON) {
		return nil, fmt.Errorf("CI plan was altered or differs from required selection and resolved topology")
	}
	expected.replay = current
	return expected, nil
}
