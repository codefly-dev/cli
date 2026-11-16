package main

import (
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// consumerNames are the names of one platform that consumes Codefly: its
// organisation, its tooling, its products and modules, and a retired project
// of its. The CLI is the tool every consumer runs, so none of them belongs in
// it — in code, tests, fixtures or docs. A test that needs a realistic
// composition uses neutral names (notes, quill, sidekick, northwind). Codefly's
// own modules (saas-starter among them) are not a consumer's and stay named.
const consumerNames = `(?i)(?<![A-Za-z0-9])(?<![Rr]ound-)(obin|obin-ai|obinh|obinctl|robin|wiki|lastlogin|workos|authkit|platform-obin|platform-core|core-solutions|infra-base|lodestar)(?![A-Za-z0-9])`

// TestTheCLINamesNoConsumer holds every file git tracks, by path and by
// content, to consumerNames. A failure is a consumer's name, path or incident
// written into the tool: state the rule generically, and keep the incident in
// the consumer's own records.
func TestTheCLINamesNoConsumer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	files, err := exec.Command("git", "ls-files").Output()
	require.NoError(t, err, "git ls-files")
	require.Greater(t, len(strings.Fields(string(files))), 100, "the listing does not read this repository")
	// Go's regexp has no look-behind, so a path is held to the same names
	// between separators.
	paths := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(obin|obin-ai|obinh|obinctl|robin|wiki|lastlogin|workos|authkit|platform-obin|platform-core|core-solutions|infra-base|lodestar)($|[^A-Za-z0-9])`)
	var hits []string
	for _, file := range strings.Fields(string(files)) {
		if paths.MatchString(file) {
			hits = append(hits, file+" (its path)")
		}
	}
	// git grep exits 1 when nothing matches, which is the passing case.
	matched, err := exec.Command("git", "grep", "-n", "-I", "-P", consumerNames, "--", ".", ":!boundary_test.go").Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		t.Fatalf("git grep: %v", err)
	}
	if text := strings.TrimSpace(string(matched)); text != "" {
		hits = append(hits, strings.Split(text, "\n")...)
	}
	require.Empty(t, hits, "the CLI names a consumer")
}

// hostLibraries are the authority repository's libraries that implement one
// host's delivery protocol: its manifest and envelope, its hop, its delivery
// document and its client.
var hostLibraries = []string{
	"github.com/codefly-dev/authority/libraries/solutionhost/go",
	"github.com/codefly-dev/authority/libraries/delivery/go",
	"github.com/codefly-dev/authority/libraries/hop/go",
	"github.com/codefly-dev/authority/libraries/authority-client/go",
	"github.com/codefly-dev/authority/libraries/fixture-composition/go",
}

// hostLinked is every CLI package that still links a host library: the
// host-delivery render (pkg/gitops and what it calls), the local host a native
// run stands up, and the solution scaffold. The list ONLY SHRINKS. Each entry
// leaves when its code moves behind the host's delivery target, and a package
// added here instead of moving is the CLI growing host knowledge again — which
// binds that host's protocol changes to a CLI release.
var hostLinked = []string{
	"github.com/codefly-dev/cli/cmd/add",
	"github.com/codefly-dev/cli/cmd/delivery",
	"github.com/codefly-dev/cli/cmd/generate",
	"github.com/codefly-dev/cli/cmd/run",
	"github.com/codefly-dev/cli/pkg/delivery/signing",
	"github.com/codefly-dev/cli/pkg/environments",
	"github.com/codefly-dev/cli/pkg/fixturefragments",
	"github.com/codefly-dev/cli/pkg/generators",
	"github.com/codefly-dev/cli/pkg/gitops",
	"github.com/codefly-dev/cli/pkg/localcluster",
	"github.com/codefly-dev/cli/pkg/methodpolicy",
	"github.com/codefly-dev/cli/pkg/orchestration",
	"github.com/codefly-dev/cli/pkg/run/identity",
	"github.com/codefly-dev/cli/pkg/solutionrun",
	"github.com/codefly-dev/cli/pkg/solutionshape",
}

// TestNoNewPackageLinksAHostLibrary is the ratchet on hostLinked: no CLI
// package outside it imports a host library directly, and core — the
// structure every tool builds on — imports none at all.
func TestNoNewPackageLinksAHostLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, "github.com/codefly-dev/cli/...").Output()
	require.NoError(t, err, "go list the CLI's packages")
	var linking []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		for _, imported := range fields[1:] {
			if slices.ContainsFunc(hostLibraries, func(library string) bool { return strings.HasPrefix(imported, library) }) {
				linking = append(linking, fields[0])
				break
			}
		}
	}
	if !slices.Contains(linking, "github.com/codefly-dev/cli/pkg/gitops") {
		t.Fatal("pkg/gitops links no host library: the listing reads something else, or the render moved — shrink hostLinked")
	}
	for _, pkg := range linking {
		if !slices.Contains(hostLinked, pkg) {
			t.Errorf("%s links a host library; host delivery belongs behind the host's delivery target, not in a new CLI package", pkg)
		}
	}
	for _, pkg := range hostLinked {
		if !slices.Contains(linking, pkg) {
			t.Errorf("%s no longer links a host library: remove it from hostLinked so the list keeps shrinking", pkg)
		}
	}
	// Core, as far as the CLI links it: no core package imports a host
	// library either.
	graph, err := exec.Command("go", "list", "-deps", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, "github.com/codefly-dev/cli/...").Output()
	require.NoError(t, err, "go list the CLI's dependency graph")
	for _, line := range strings.Split(strings.TrimSpace(string(graph)), "\n") {
		fields := strings.Fields(line)
		if !strings.HasPrefix(fields[0], "github.com/codefly-dev/core/") {
			continue
		}
		for _, imported := range fields[1:] {
			if slices.ContainsFunc(hostLibraries, func(library string) bool { return strings.HasPrefix(imported, library) }) {
				t.Errorf("core's %s links the host library %s", fields[0], imported)
			}
		}
	}
}
