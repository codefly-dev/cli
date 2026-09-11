package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/executionplan"
	"github.com/codefly-dev/core/resources"
)

type ReplayPlan struct {
	Schema    string      `json:"schema"`
	Candidate string      `json:"candidate"`
	Content   string      `json:"content"`
	Selection *Plan       `json:"selection"`
	Execution []StagePlan `json:"execution"`
}

type StagePlan struct {
	Stage       resources.Stage     `json:"stage"`
	Plan        *executionplan.Plan `json:"plan"`
	Fingerprint string              `json:"fingerprint"`
}

func buildReplayPlan(ctx context.Context, workspace *resources.Workspace, plan *Plan) (*ReplayPlan, error) {
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
	content, err := replayContent(ctx, root, map[string]bool{})
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
	result := &ReplayPlan{Schema: "codefly.ci-replay/v1", Candidate: candidate, Content: content, Selection: plan, Execution: []StagePlan{}}
	for _, selected := range plan.Services {
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
			if stage == resources.StageRun {
				draft.SchemaSteps = nil
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

// Replay hashes the complete local tree, including ignored files and symlink
// targets. Cache pruning is unsuitable here: an omitted input could change the
// code executed without changing the submitted plan's identity.
func replayContent(ctx context.Context, root string, visiting map[string]bool) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if visiting[resolved] {
		return "", fmt.Errorf("cyclic source symlink at %s", root)
	}
	visiting[resolved] = true
	defer delete(visiting, resolved)
	hasher := sha256.New()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if !entry.Type().IsRegular() && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported source file type: %s", path)
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if hashErr := hashCacheEntry(hasher, root, path); hashErr != nil {
			return hashErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, targetErr := filepath.EvalSymlinks(path)
			if targetErr != nil {
				return targetErr
			}
			digest, digestErr := replayContent(ctx, target, visiting)
			if digestErr != nil {
				return digestErr
			}
			writeCacheRecord(hasher, "symlink-content", digest)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash candidate content: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func readReplayPlan(ctx context.Context, workspace *resources.Workspace, path string, options *PlanOptions) (*Plan, error) {
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
	if submitted.Schema != "codefly.ci-replay/v1" || submitted.Selection == nil {
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
	current, err := buildReplayPlan(ctx, workspace, expected)
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
	return expected, nil
}
