package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

type baseManifest struct {
	Files map[string]string `json:"files"`
}

type BaseReport struct {
	Modules []BaseModuleReport
}

type BaseModuleReport struct {
	Module                   string
	Files                    int
	Omitted                  map[string]int
	Allowed                  []AllowedDivergence
	Missing                  []string
	Modified                 []string
	MissingRequiredAdditions []string
	InvalidRequiredAdditions []string
	Error                    string
}

type AllowedDivergence struct {
	Path   string
	Reason string
}

func (report BaseReport) Failed() int {
	failed := 0
	for _, module := range report.Modules {
		if module.Error != "" || len(module.Missing) > 0 || len(module.Modified) > 0 ||
			len(module.MissingRequiredAdditions) > 0 || len(module.InvalidRequiredAdditions) > 0 {
			failed++
		}
	}
	return failed
}

// VerifyBase checks every guarded module without printing or mutating. A
// module is guarded when it carries tools/base-manifest.json.
func VerifyBase(ctx context.Context, workspace *resources.Workspace) (BaseReport, error) {
	if workspace == nil {
		return BaseReport{}, fmt.Errorf("workspace is nil")
	}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return BaseReport{}, fmt.Errorf("load workspace modules: %w", err)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })
	report := BaseReport{Modules: []BaseModuleReport{}}
	for _, module := range modules {
		dir := module.Dir()
		manifestPath := filepath.Join(dir, "tools", "base-manifest.json")
		data, err := os.ReadFile(manifestPath)
		if errorsIsNotExist(err) {
			continue
		}
		moduleReport := BaseModuleReport{Module: module.Name, Omitted: map[string]int{}}
		if err != nil {
			moduleReport.Error = fmt.Sprintf("read base-manifest.json: %v", err)
			report.Modules = append(report.Modules, moduleReport)
			continue
		}
		var manifest baseManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			moduleReport.Error = fmt.Sprintf("invalid base-manifest.json: %v", err)
			report.Modules = append(report.Modules, moduleReport)
			continue
		}
		moduleReport.Files = len(manifest.Files)
		allow, err := loadBaseIntegrityAllow(filepath.Join(dir, "tools", "base-integrity-allow.json"))
		if err != nil {
			moduleReport.Error = fmt.Sprintf("invalid base-integrity-allow.json: %v", err)
			report.Modules = append(report.Modules, moduleReport)
			continue
		}
		composed := map[string]bool{}
		for _, reference := range module.ServiceReferences {
			composed[reference.Name] = true
		}
		paths := make([]string, 0, len(manifest.Files))
		for relative := range manifest.Files {
			paths = append(paths, relative)
		}
		sort.Strings(paths)
		for _, relative := range paths {
			if !safeModulePath(dir, relative, false) {
				moduleReport.Error = fmt.Sprintf("base manifest contains unsafe path %q", relative)
				continue
			}
			if service := serviceOf(relative); service != "" && len(composed) > 0 && !composed[service] {
				moduleReport.Omitted[service]++
				continue
			}
			digest, err := sha256File(filepath.Join(dir, relative))
			if err != nil {
				if _, allowed := allow.Divergences[relative]; !allowed {
					moduleReport.Missing = append(moduleReport.Missing, relative)
				}
				continue
			}
			if digest == manifest.Files[relative] {
				continue
			}
			if reason, allowed := allow.Divergences[relative]; allowed {
				moduleReport.Allowed = append(moduleReport.Allowed, AllowedDivergence{Path: relative, Reason: reason})
			} else {
				moduleReport.Modified = append(moduleReport.Modified, relative)
			}
		}
		moduleReport.MissingRequiredAdditions, moduleReport.InvalidRequiredAdditions = validateRequiredAdditions(dir, allow.RequiredAdditions)
		report.Modules = append(report.Modules, moduleReport)
	}
	if failed := report.Failed(); failed > 0 {
		return report, fmt.Errorf("base-integrity verification failed: %d module(s) failed; promote the change upstream into canonical, or express it as a side-addition", failed)
	}
	return report, nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

func serviceOf(relative string) string {
	const prefix = "services/"
	if !strings.HasPrefix(relative, prefix) {
		return ""
	}
	rest := relative[len(prefix):]
	if index := strings.IndexByte(rest, '/'); index >= 0 {
		return rest[:index]
	}
	return ""
}

func sha256File(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

type baseIntegrityAllow struct {
	Divergences       map[string]string
	RequiredAdditions map[string]string
}

func loadBaseIntegrityAllow(path string) (baseIntegrityAllow, error) {
	result := baseIntegrityAllow{
		Divergences:       map[string]string{},
		RequiredAdditions: map[string]string{},
	}
	payload, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(payload, &entries); err != nil {
		return result, err
	}
	if entries == nil {
		return result, fmt.Errorf("policy must be a JSON object")
	}
	for key, raw := range entries {
		if key == "requiredAdditions" {
			if err := json.Unmarshal(raw, &result.RequiredAdditions); err != nil {
				return result, fmt.Errorf("requiredAdditions must be a path-to-reason object: %w", err)
			}
			continue
		}
		if _, ok := canonicalModulePath(key); !ok {
			return result, fmt.Errorf("divergence path %q is not canonical", key)
		}
		var reason string
		if err := json.Unmarshal(raw, &reason); err != nil {
			return result, fmt.Errorf("divergence %q reason must be a string: %w", key, err)
		}
		if strings.TrimSpace(reason) == "" {
			return result, fmt.Errorf("divergence %q reason must not be empty", key)
		}
		result.Divergences[key] = reason
	}
	return result, nil
}
