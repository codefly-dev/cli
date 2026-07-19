// Package sourcefix implements atomic, plugin-owned source repair for CLI
// commands. Language behavior remains behind Code.Fix; this package stages,
// verifies, reports, commits, and rolls back multi-file plans.
package sourcefix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"google.golang.org/grpc"
)

type Executor interface {
	Execute(context.Context, *codev0.CodeRequest, ...grpc.CallOption) (*codev0.CodeResponse, error)
}

type Options struct {
	Files  []string
	Paths  []string
	Mode   basev0.FixMode
	DryRun bool
}

type Result struct {
	File         string   `json:"file"`
	Changed      bool     `json:"changed"`
	Wrote        bool     `json:"wrote"`
	BeforeSHA256 string   `json:"before_sha256"`
	AfterSHA256  string   `json:"after_sha256"`
	Actions      []string `json:"actions,omitempty"`
	Output       string   `json:"output,omitempty"`
}

type Report struct {
	Service string   `json:"service"`
	Mode    string   `json:"mode"`
	DryRun  bool     `json:"dry_run"`
	Changed int      `json:"changed"`
	Written int      `json:"written"`
	Results []Result `json:"results"`
}

type stagedFile struct {
	result   Result
	original string
	fixed    string
}

func Run(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, options Options) (*Report, error) {
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, fmt.Errorf("load service plugin: %w", err)
	}
	if err := instance.LoadRuntime(ctx, true); err != nil {
		return nil, fmt.Errorf("load runtime contract: %w", err)
	}
	environment, err := resources.LocalEnvironment().Proto()
	if err != nil {
		return nil, fmt.Errorf("encode local environment: %w", err)
	}
	instance.Runtime.Workspace = workspace
	if _, err := instance.Runtime.Load(ctx, environment); err != nil {
		return nil, fmt.Errorf("initialize source root: %w", err)
	}
	client, err := services.LoadCode(ctx, service)
	if err != nil {
		return nil, fmt.Errorf("load Code client: %w", err)
	}
	files, err := ResolveFiles(ctx, client, service, options.Files, options.Paths)
	if err != nil {
		return nil, err
	}
	return FixFiles(ctx, client, service.Name, files, options)
}

// ResolveFiles turns explicit files and directory scopes into a deterministic
// source-file list. With neither, the complete source tree is selected.
func ResolveFiles(ctx context.Context, client Executor, service *resources.Service, files, scopes []string) ([]string, error) {
	extensions, err := sourceExtensions(service)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, file := range files {
		clean, err := cleanSourcePath(file)
		if err != nil {
			return nil, err
		}
		if !hasExtension(clean, extensions) {
			return nil, fmt.Errorf("%s is not a supported %s source file", file, service.Agent.Name)
		}
		set[clean] = struct{}{}
	}
	if len(scopes) == 0 && len(files) == 0 {
		scopes = []string{""}
	}
	for _, scope := range scopes {
		clean, err := cleanSourcePath(scope)
		if err != nil {
			return nil, err
		}
		response, err := client.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ListFiles{ListFiles: &codev0.ListFilesRequest{
			Path: clean, Extensions: extensions, Recursive: true,
		}}})
		if err != nil {
			return nil, fmt.Errorf("list source files under %q: %w", scope, err)
		}
		listing := response.GetListFiles()
		if listing == nil {
			return nil, codeResponseError(response, "plugin returned no file listing")
		}
		for _, file := range listing.GetFiles() {
			if !file.GetIsDirectory() && hasExtension(file.GetPath(), extensions) {
				set[filepath.ToSlash(file.GetPath())] = struct{}{}
			}
		}
	}
	resolved := make([]string, 0, len(set))
	for file := range set {
		resolved = append(resolved, file)
	}
	sort.Strings(resolved)
	if len(resolved) == 0 {
		return nil, fmt.Errorf("no supported source files selected")
	}
	return resolved, nil
}

// FixFiles stages every language fix before writing anything, verifies the
// input and output hashes, rechecks all inputs for concurrent edits, then
// commits with best-effort rollback on failure.
func FixFiles(ctx context.Context, client Executor, service string, files []string, options Options) (*Report, error) {
	staged := make([]stagedFile, 0, len(files))
	for _, file := range files {
		original, err := readFile(ctx, client, file)
		if err != nil {
			return nil, err
		}
		response, err := client.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{
			File: file, Mode: options.Mode, DryRun: true,
		}}})
		if err != nil {
			return nil, fmt.Errorf("preview fix for %s: %w", file, err)
		}
		fix := response.GetFix()
		if fix == nil || !fix.GetSuccess() {
			return nil, codeResponseError(response, fmt.Sprintf("fix failed for %s", file))
		}
		if digest(original) != fix.GetBeforeSha256() || digest(fix.GetContent()) != fix.GetAfterSha256() {
			return nil, fmt.Errorf("plugin returned inconsistent fix evidence for %s", file)
		}
		staged = append(staged, stagedFile{
			original: original, fixed: fix.GetContent(),
			result: Result{File: file, Changed: fix.GetChanged(), BeforeSHA256: fix.GetBeforeSha256(), AfterSHA256: fix.GetAfterSha256(), Actions: fix.GetActions(), Output: fix.GetOutput()},
		})
	}

	report := &Report{Service: service, Mode: options.Mode.String(), DryRun: options.DryRun, Results: make([]Result, len(staged))}
	for i := range staged {
		report.Results[i] = staged[i].result
		if staged[i].result.Changed {
			report.Changed++
		}
	}
	if options.DryRun || report.Changed == 0 {
		return report, nil
	}

	// Optimistic concurrency gate: no file may change between preview and the
	// start of the commit phase.
	for _, file := range staged {
		current, err := readFile(ctx, client, file.result.File)
		if err != nil {
			return nil, err
		}
		if digest(current) != file.result.BeforeSHA256 {
			return nil, fmt.Errorf("%s changed after fix preview; refusing to overwrite it", file.result.File)
		}
	}

	written := make([]int, 0, report.Changed)
	for index := range staged {
		if !staged[index].result.Changed {
			continue
		}
		// Recheck immediately before each write as well as at the commit gate.
		// This catches edits made while an earlier staged file was being written.
		current, err := readFile(ctx, client, staged[index].result.File)
		if err != nil {
			return nil, rollbackError(ctx, client, staged, written, fmt.Errorf("recheck %s: %w", staged[index].result.File, err))
		}
		if digest(current) != staged[index].result.BeforeSHA256 {
			return nil, rollbackError(ctx, client, staged, written, fmt.Errorf("%s changed during fix commit; refusing to overwrite it", staged[index].result.File))
		}
		if err := writeFile(ctx, client, staged[index].result.File, staged[index].fixed); err != nil {
			rollbackErrs := make([]string, 0, len(written)+1)
			// A transport failure can be reported after the remote write was
			// applied. Restore only known staged states; never overwrite an
			// intervening user edit with the preview's original content.
			if restoreErr := restoreStagedFile(ctx, client, staged[index]); restoreErr != nil {
				rollbackErrs = append(rollbackErrs, restoreErr.Error())
			}
			for i := len(written) - 1; i >= 0; i-- {
				prior := written[i]
				if restoreErr := restoreStagedFile(ctx, client, staged[prior]); restoreErr != nil {
					rollbackErrs = append(rollbackErrs, restoreErr.Error())
				}
			}
			if len(rollbackErrs) > 0 {
				return nil, fmt.Errorf("commit %s: %w; rollback errors: %s", staged[index].result.File, err, strings.Join(rollbackErrs, "; "))
			}
			return nil, fmt.Errorf("commit %s: %w; all writes rolled back", staged[index].result.File, err)
		}
		written = append(written, index)
		staged[index].result.Wrote = true
		report.Results[index].Wrote = true
		report.Written++
	}
	return report, nil
}

func rollbackError(ctx context.Context, client Executor, staged []stagedFile, written []int, cause error) error {
	rollbackErrs := make([]string, 0, len(written))
	for i := len(written) - 1; i >= 0; i-- {
		prior := written[i]
		if err := restoreStagedFile(ctx, client, staged[prior]); err != nil {
			rollbackErrs = append(rollbackErrs, err.Error())
		}
	}
	if len(rollbackErrs) > 0 {
		return fmt.Errorf("%w; rollback errors: %s", cause, strings.Join(rollbackErrs, "; "))
	}
	if len(written) > 0 {
		return fmt.Errorf("%w; all prior writes rolled back", cause)
	}
	return cause
}

func restoreStagedFile(ctx context.Context, client Executor, staged stagedFile) error {
	current, err := readFile(ctx, client, staged.result.File)
	if err != nil {
		return fmt.Errorf("inspect %s for rollback: %w", staged.result.File, err)
	}
	switch digest(current) {
	case staged.result.BeforeSHA256:
		return nil
	case staged.result.AfterSHA256:
		if err := writeFile(ctx, client, staged.result.File, staged.original); err != nil {
			return fmt.Errorf("restore %s: %w", staged.result.File, err)
		}
		return nil
	default:
		return fmt.Errorf("refusing to roll back %s: live content no longer matches the staged before/after state", staged.result.File)
	}
}

func readFile(ctx context.Context, client Executor, path string) (string, error) {
	response, err := client.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ReadFile{ReadFile: &codev0.ReadFileRequest{Path: path}}})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	read := response.GetReadFile()
	if read == nil || !read.GetExists() {
		return "", codeResponseError(response, fmt.Sprintf("source file %s does not exist", path))
	}
	return read.GetContent(), nil
}

func writeFile(ctx context.Context, client Executor, path, content string) error {
	response, err := client.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_WriteFile{WriteFile: &codev0.WriteFileRequest{Path: path, Content: content}}})
	if err != nil {
		return err
	}
	if response.GetWriteFile() == nil || !response.GetWriteFile().GetSuccess() {
		return codeResponseError(response, fmt.Sprintf("write failed for %s", path))
	}
	return nil
}

func sourceExtensions(service *resources.Service) ([]string, error) {
	if service == nil || service.Agent == nil {
		return nil, fmt.Errorf("service has no source agent")
	}
	switch service.Agent.Name {
	case "go", "go-grpc":
		return []string{".go"}, nil
	case "python", "python-fastapi":
		return []string{".py"}, nil
	case "nextjs":
		return []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"}, nil
	case "rust":
		return []string{".rs"}, nil
	case "swift":
		return []string{".swift"}, nil
	default:
		return nil, fmt.Errorf("agent %q does not advertise a source fixer", service.Agent.Name)
	}
}

func cleanSourcePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return "", nil
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("source path must be relative: %s", path)
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source path escapes source root: %s", path)
	}
	return filepath.ToSlash(clean), nil
}

func hasExtension(path string, extensions []string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, candidate := range extensions {
		if ext == candidate {
			return true
		}
	}
	return false
}

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func codeResponseError(response *codev0.CodeResponse, fallback string) error {
	if response != nil && response.GetFailure() != nil && response.GetFailure().GetMessage() != "" {
		return fmt.Errorf("%s", response.GetFailure().GetMessage())
	}
	return fmt.Errorf("%s", fallback)
}
