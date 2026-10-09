package show

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// snapshotSelection freezes only workspace declarations. It does not reconstruct
// installed modules, configuration, local overrides, build inputs or receipts.
// Core remains the owner of composition and duplicate-selection validation.
func snapshotSelection(ctx context.Context, root, revision string, imports []string) (selectionReport, error) {
	empty := selectionReport{}
	root, err := filepath.Abs(root)
	if err != nil {
		return empty, err
	}
	pins := map[string]string{root: revision}
	for _, entry := range imports {
		directory, commit, ok := strings.Cut(entry, "=")
		if !ok || !filepath.IsAbs(directory) {
			return empty, fmt.Errorf("import revision must be absolute-workspace-directory=commit")
		}
		directory = filepath.Clean(directory)
		if _, exists := pins[directory]; exists {
			return empty, fmt.Errorf("duplicate snapshot workspace pin")
		}
		pins[directory] = commit
	}
	for _, commit := range pins {
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
			return empty, fmt.Errorf("snapshot revisions must be immutable 40-character commits")
		}
	}
	stage, err := os.MkdirTemp("", "codefly-selection-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(stage)
	// Stop Core's ancestor overlay search inside the isolated reconstruction.
	if err = os.WriteFile(filepath.Join(stage, resources.LocalOverlayConfigurationName), []byte("{}\n"), 0600); err != nil {
		return empty, err
	}
	stagedPath := func(directory string) string {
		return filepath.Join(stage, strings.TrimPrefix(directory, string(filepath.Separator)))
	}
	snapshot := selectionSnapshot{ctx: ctx, stage: stage, pins: pins, visited: map[string]bool{}}
	if err = snapshot.materialize(root, 0); err != nil {
		return empty, err
	}

	if len(snapshot.visited) != len(pins) {
		return empty, fmt.Errorf("snapshot contains unused import pins")
	}
	ws, err := resources.LoadWorkspaceFromDir(ctx, stagedPath(root))
	if err != nil {
		return empty, err
	}
	report := selectionProjection(ws)
	original := func(directory string) string {
		relative, _ := filepath.Rel(stage, directory)
		return filepath.Join(string(filepath.Separator), relative)
	}
	report.Directory = root
	for i := range report.Workspaces {
		item := &report.Workspaces[i]
		item.Directory = original(item.Directory)
		if item.ParentDirectory != "" {
			item.ParentDirectory = original(item.ParentDirectory)
		}
		item.ManifestPath = filepath.Join(item.Directory, "workspace.codefly.yaml")
		item.Revision = pins[item.Directory]
		item.SourceMode = "snapshot"
	}
	for i := range report.Modules {
		report.Modules[i].DeclarationDirectory = original(report.Modules[i].DeclarationDirectory)
		report.Modules[i].Resolution = &selectionResolution{State: "unavailable", Record: "not collected for committed declarations"}
	}
	return report, nil
}

type selectionSnapshot struct {
	ctx     context.Context
	stage   string
	pins    map[string]string
	visited map[string]bool
}

func (s *selectionSnapshot) stagedPath(directory string) string {
	return filepath.Join(s.stage, strings.TrimPrefix(directory, string(filepath.Separator)))
}
func (s *selectionSnapshot) materialize(directory string, depth int) error {
	if depth > 32 || len(s.visited) > 128 {
		return fmt.Errorf("snapshot workspace closure exceeds limits")
	}
	commit, pinned := s.pins[directory]
	if !pinned {
		return fmt.Errorf("imported workspace requires an explicit snapshot commit: %s", directory)
	}
	if s.visited[directory] {
		return nil
	}
	s.visited[directory] = true
	run := func(args ...string) ([]byte, error) {
		// #nosec G204 -- fixed git executable; only object-reading verbs, immutable commit IDs and bounded paths are supplied below.
		out, err := exec.CommandContext(s.ctx, "git", append([]string{"-C", directory}, args...)...).Output()
		if err != nil {
			return nil, fmt.Errorf("snapshot Git object unavailable")
		}
		return out, nil
	}
	// The configured workspace must be the repository root; nested workspaces
	// need an explicit subpath contract before this export can support them.
	top, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(directory)
	if err != nil || actual != expected {
		return fmt.Errorf("snapshot workspace must be a repository root")
	}
	if _, err = run("cat-file", "-e", commit+"^{commit}"); err != nil {
		return err
	}
	object := commit + ":workspace.codefly.yaml"
	sizeBytes, err := run("cat-file", "-s", object)
	if err != nil {
		return err
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(sizeBytes)))
	if err != nil || size > 1_000_000 {
		return fmt.Errorf("workspace declaration exceeds size limit")
	}
	content, err := run("show", object)
	if err != nil {
		return err
	}
	var declaration resources.Workspace
	if err := yaml.Unmarshal(content, &declaration); err != nil {
		return fmt.Errorf("invalid snapshot workspace declaration")
	}
	if declaration.Layout != resources.LayoutKindModules {
		return fmt.Errorf("snapshot selection requires modules layout")
	}
	destination := s.stagedPath(directory)
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(destination, "workspace.codefly.yaml"), content, 0600); err != nil {
		return err
	}
	for _, ref := range declaration.Workspaces {
		if ref == nil || ref.Path == "" || filepath.IsAbs(ref.Path) {
			return fmt.Errorf("snapshot import requires an explicit relative workspace path")
		}
		child := filepath.Clean(filepath.Join(directory, ref.Path))
		if filepath.Join(destination, ref.Path) != s.stagedPath(child) {
			return fmt.Errorf("snapshot import escapes the reconstruction boundary")
		}
		if err := s.materialize(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}
