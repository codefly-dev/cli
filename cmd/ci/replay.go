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
	"strings"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/executionplan"
	"github.com/codefly-dev/core/resources"
)

type ReplayPlan struct {
	Schema     string           `json:"schema"`
	Candidate  string           `json:"candidate"`
	Content    string           `json:"content"`
	Selection  *Plan            `json:"selection"`
	Execution  []StagePlan      `json:"execution"`
	Invocation ReplayInvocation `json:"invocation"`
	Tasks      []ReplayTask     `json:"tasks"`
}

type StagePlan struct {
	Stage       resources.Stage     `json:"stage"`
	Plan        *executionplan.Plan `json:"plan"`
	Fingerprint string              `json:"fingerprint"`
}

type ReplayInvocation struct {
	Phases         []string `json:"phases"`
	Suites         []string `json:"suites"`
	RuntimeContext string   `json:"runtime_context"`
}

type ReplayTask struct {
	Phase         string         `json:"phase"`
	Suite         string         `json:"suite,omitempty"`
	Service       PlannedService `json:"service"`
	Prerequisites []string       `json:"prerequisites"`
	Resources     []string       `json:"resources"`
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
	inventory, _, err := loadPlanInventory(ctx, workspace)
	if err != nil {
		return nil, err
	}
	kinds := map[[2]string]resources.DependencyKind{}
	for _, record := range inventory {
		for _, dependency := range record.service.ServiceDependencies {
			kinds[[2]string{dependency.Unique(), record.unique}] = dependency.Kind
		}
	}
	result := &ReplayPlan{Schema: "codefly.ci-replay/v2", Invocation: invocation, Tasks: []ReplayTask{}, Candidate: candidate, Content: content, Selection: plan, Execution: []StagePlan{}}
	result.Tasks, err = resolveReplayTasks(ctx, workspace, plan, invocation)
	if err != nil {
		return nil, err
	}
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return nil, err
	}
	for _, selected := range plan.Services {
		selectedDependencies, restrictionErr := dependencies.Restrict(ctx, selected.Service)
		if restrictionErr != nil {
			return nil, restrictionErr
		}
		if visibilityErr := selectedDependencies.VerifyVisibility(ctx); visibilityErr != nil {
			return nil, visibilityErr
		}

		closure, err := architecture.SelectClosure(ctx, workspace, selected.Service)
		if err != nil {
			return nil, err
		}
		for _, stage := range resources.Stages() {
			draft, err := closure.Draft(ctx, architecture.PlanOptions{Phase: executionplan.Phase(stage), StatePolicy: executionplan.StatePolicy{Lifecycle: executionplan.LifecycleStop}})
			if err != nil {
				return nil, err
			}
			edges := draft.Edges[:0]
			for index := range draft.Edges {
				edge := draft.Edges[index]
				kind := kinds[[2]string{edge.From, edge.To}]
				if kind != resources.DependencyKindLegacy {
					edge.Kind = executionplan.EdgeKind(kind)
				}
				if kindErr := kind.Validate(); kindErr != nil {
					return nil, kindErr
				}
				for _, participating := range kind.Stages() {
					if participating == stage {
						edges = append(edges, edge)
					}
				}
			}
			draft.Edges = edges
			if len(draft.SchemaSteps) > 0 {
				return nil, fmt.Errorf("CI replay has no executor for schema job %s", draft.SchemaSteps[0].ID)
			}
			draft = draft.Canonical()
			if validationErr := draft.Validate(); validationErr != nil {
				return nil, fmt.Errorf("validate %s plan for %s: %w", stage, selected.Service, validationErr)
			}
			fingerprint, err := draft.SemanticFingerprint()
			if err != nil {
				return nil, fmt.Errorf("validate %s plan for %s: %w", stage, selected.Service, err)
			}
			result.Execution = append(result.Execution, StagePlan{Stage: stage, Plan: draft, Fingerprint: fingerprint})
		}
	}
	return result, nil
}

func resolveReplayTasks(ctx context.Context, workspace *resources.Workspace, plan *Plan, invocation ReplayInvocation) ([]ReplayTask, error) {
	tasks := []ReplayTask{}
	for _, phase := range invocation.Phases {
		if phase == ciPhaseVerify {
			continue
		}
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
				tasks = append(tasks, ReplayTask{Phase: phase, Suite: suite, Service: task.planned, Prerequisites: task.prerequisites, Resources: task.resources})
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
	if submitted.Schema != "codefly.ci-replay/v2" || submitted.Selection == nil {
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
