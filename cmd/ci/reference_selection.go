package ci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func preserveReferenceDependents(ctx context.Context, workspace *resources.Workspace, root string, plan *Plan, candidate []serviceRecord, selected map[string]*mutablePlanService) error {
	var declarations []string
	for _, path := range plan.ChangedFiles {
		if filepath.Base(path) == resources.ServiceConfigurationName {
			declarations = append(declarations, path)
		}
	}
	if len(declarations) == 0 {
		return nil
	}
	for _, path := range declarations {
		if !pathWithin(referenceChangedPath(root, path), cleanAbs(root)) {
			return fmt.Errorf("cannot reconcile changed external service declaration %s using this repository's --base; run CI from the declaration's owning repository", path)
		}
	}
	reference := firstNonEmpty(plan.Base, gitHeadRevision)
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", reference+"^{commit}"); err != nil {
		if plan.Base != "" {
			return err
		}
		for _, path := range declarations {
			known := false
			for _, service := range candidate {
				if referenceChangedPath(root, path) == filepath.Join(service.dir, resources.ServiceConfigurationName) {
					known = true
				}
			}
			if !known {
				return fmt.Errorf("cannot reconcile removed service declaration %s without a reference revision", path)
			}
		}
		return nil
	}
	referenceServices, err := referenceServiceInventory(ctx, workspace, root, reference)
	if err != nil {
		return err
	}
	dependents := map[string][]string{}
	for _, records := range [][]serviceRecord{referenceServices, candidate} {
		for _, record := range records {
			for _, dependency := range record.service.ServiceDependencies {
				dependents[dependency.Unique()] = append(dependents[dependency.Unique()], record.unique)
			}
		}
	}
	live := map[string]bool{}
	for _, service := range candidate {
		live[service.unique] = true
	}
	for _, path := range declarations {
		absolute := referenceChangedPath(root, path)
		resolved := false
		for _, service := range candidate {
			if absolute == filepath.Join(service.dir, resources.ServiceConfigurationName) {
				resolved = true
				break
			}
		}
		for _, previous := range referenceServices {
			if absolute != filepath.Join(previous.dir, resources.ServiceConfigurationName) {
				continue
			}
			resolved = true
			queue := []string{previous.unique}
			visited := map[string]bool{}
			for len(queue) > 0 {
				current := queue[0]
				queue = queue[1:]
				if visited[current] {
					continue
				}
				visited[current] = true
				queue = append(queue, dependents[current]...)
				if live[current] {
					addPlanSelection(selected, current, "dependent", "affected by reference declaration "+previous.unique, path)
				}
			}
		}
		if !resolved {
			return fmt.Errorf("cannot reconcile removed service declaration %s at reference %s; supply --base with a revision containing the declaration", path, reference)
		}
	}
	return nil
}

func referenceServiceInventory(ctx context.Context, workspace *resources.Workspace, root, revision string) ([]serviceRecord, error) {
	tree, err := gitOutput(ctx, root, "ls-tree", "-r", "--name-only", "-z", revision)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, path := range strings.Split(string(tree), "\x00") {
		files[path] = true
	}
	read := func(path string, destination any) error {
		// A main-repository revision does not version external checkouts. They
		// are fixed resolved inputs to this comparison, not historical files in
		// its Git tree. External declaration changes require their own CI bounds.
		if !pathWithin(cleanAbs(path), cleanAbs(root)) {
			payload, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return yaml.Unmarshal(payload, destination)
		}
		relative, relErr := filepath.Rel(root, cleanAbs(path))
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if !files[relative] {
			return fmt.Errorf("cannot resolve reference manifest %s at %s", relative, revision)
		}
		payload, readErr := gitOutput(ctx, root, "show", revision+":"+relative)
		if readErr != nil {
			return readErr
		}
		return yaml.Unmarshal(payload, destination)
	}
	workspacePath := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	relative, err := filepath.Rel(root, cleanAbs(workspacePath))
	if err != nil {
		return nil, err
	}
	if !files[filepath.ToSlash(relative)] {
		return nil, nil
	}
	var previous resources.Workspace
	if err := read(workspacePath, &previous); err != nil {
		return nil, err
	}
	var result []serviceRecord
	references := previous.Modules
	if previous.Layout == resources.LayoutKindFlat && len(references) == 0 {
		references = []*resources.ModuleReference{{Name: previous.Name}}
	}
	for _, reference := range references {
		dir := filepath.Join(workspace.Dir(), "modules", reference.Name)
		if previous.Layout == resources.LayoutKindFlat {
			dir = workspace.Dir()
		}
		if reference.PathOverride != nil {
			dir = *reference.PathOverride
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(workspace.Dir(), dir)
			}
		}
		var module resources.Module
		if err := read(filepath.Join(dir, resources.ModuleConfigurationName), &module); err != nil {
			return nil, err
		}
		module.WithDir(dir)
		for _, serviceReference := range module.ServiceReferences {
			serviceDir := module.ServicePath(ctx, serviceReference)
			var service resources.Service
			if err := read(filepath.Join(serviceDir, resources.ServiceConfigurationName), &service); err != nil {
				return nil, err
			}
			service.WithModule(reference.Name)
			for _, dependency := range service.ServiceDependencies {
				if dependency.Module == "" {
					dependency.Module = reference.Name
				}
			}
			result = append(result, serviceRecord{unique: resources.ServiceUnique(reference.Name, service.Name), module: reference.Name, dir: cleanAbs(serviceDir), service: &service})
		}
	}
	return result, nil
}

func referenceChangedPath(root, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	return cleanAbs(path)
}
