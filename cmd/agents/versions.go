package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/blang/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v37/github"
	"github.com/spf13/cobra"
)

// ciPlatform is the os/arch a released agent must ship an asset for to be
// downloadable inside CI. A git tag can exist with no artifact for this
// platform — that gap (module-saas-starter#3) is exactly what these commands
// surface.
const ciPlatform = "linux_amd64"

// Seams so the resolvability logic can be tested without reaching GitHub, an
// OCI registry, or the local filesystem.
var (
	fetchReleases = fetchReleasesFromGitHub
	fetchTags     = fetchTagsFromGitHub
	fetchOCITags  = fetchOCITagsFromRegistry
)

// releaseInfo is one published GitHub release: the version it tags and the
// os_arch suffixes it ships a downloadable asset for.
type releaseInfo struct {
	version   string
	platforms []string
}

// sourceFlags records, per version, which sources can supply it. GithubRelease
// specifically means the CI-platform asset is present — the resolvability
// signal — regardless of which other platforms the release also ships.
type sourceFlags struct {
	Tag           bool `json:"tag"`
	GithubRelease bool `json:"github_release"`
	OCI           bool `json:"oci"`
	PinnedHere    bool `json:"pinned_here"`
	LocalCache    bool `json:"local_cache"`
}

func (f sourceFlags) resolvable() bool {
	return f.GithubRelease || f.OCI
}

type versionEntry struct {
	Version string      `json:"version"`
	Sources sourceFlags `json:"sources"`
	// ReleasePlatforms lists every os_arch the GitHub release ships an asset
	// for, so a version downloadable only for the host (not the CI platform)
	// isn't misread as having no artifact at all.
	ReleasePlatforms []string `json:"release_platforms,omitempty"`
	sem              semver.Version
}

type inventory struct {
	Agent            string         `json:"agent"`
	CIPlatform       string         `json:"ci_platform"`
	OCIConfigured    bool           `json:"oci_configured"`
	Versions         []versionEntry `json:"versions"`
	Pinned           []string       `json:"pinned,omitempty"`
	LatestTag        string         `json:"latest_tag,omitempty"`
	LatestResolvable string         `json:"latest_resolvable,omitempty"`
}

func (inv inventory) versionResolvable(version string) bool {
	if version == "latest" {
		return inv.LatestResolvable != ""
	}
	for _, entry := range inv.Versions {
		if entry.Version == version {
			return entry.Sources.resolvable()
		}
	}
	return false
}

var versionsJSON bool

// VersionsCmd reports every known version of a single agent and whether each is
// resolvable per source (git tag, GitHub release asset, OCI manifest), plus the
// version pinned in the current workspace and what sits in the local cache.
var VersionsCmd = &cobra.Command{
	Use:   "versions <publisher/name>",
	Short: "List an agent's versions and whether each is resolvable",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		// This command lists every version, so the specific version in the
		// argument is irrelevant. Pin it to a placeholder so ParseAgent doesn't
		// warn "no version specified, using latest" on the common bare form.
		spec := args[0]
		if !strings.Contains(spec, ":") {
			spec += ":latest"
		}
		agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, spec)
		if err != nil {
			return fmt.Errorf("invalid agent: %w", err)
		}

		inv := collectInventory(ctx, agent, pinnedVersions(ctx, agent))

		if versionsJSON {
			return writeJSON(inv)
		}
		renderInventory(inv)
		return nil
	},
}

var listJSON bool

// ListCmd enumerates every agent pinned across the current workspace and, for
// each, whether the pinned version is resolvable, the latest resolvable
// version, and the latest tag — a one-shot "are all my pins publishable?" view.
var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every agent pinned in the workspace and its resolvability",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		pins, err := workspacePins(ctx, workspace)
		if err != nil {
			return err
		}

		summaries := summarizeWorkspaceAgents(ctx, pins)
		if listJSON {
			return writeJSON(summaries)
		}
		renderSummaries(summaries)
		return nil
	},
}

func init() {
	VersionsCmd.Flags().BoolVar(&versionsJSON, "json", false, "Emit the version inventory as JSON")
	ListCmd.Flags().BoolVar(&listJSON, "json", false, "Emit the workspace agent inventory as JSON")
}

// collectInventory gathers versions from every source and assembles the
// resolvability inventory. GitHub lookups that fail (missing repo, rate limit)
// degrade to a warning so the local-cache and pinned columns still render.
func collectInventory(ctx context.Context, agent *resources.Agent, pinned []string) inventory {
	releases, err := fetchReleases(ctx, agent)
	if err != nil {
		cli.Warning("cannot list GitHub releases for %s/%s: %v", agent.Publisher, agent.Name, err)
	}
	tags, err := fetchTags(ctx, agent)
	if err != nil {
		cli.Warning("cannot list GitHub tags for %s/%s: %v", agent.Publisher, agent.Name, err)
	}
	local := localCacheVersions(ctx, agent)
	ociConfigured, ociVersions, err := fetchOCITags(ctx, agent)
	if err != nil {
		cli.Warning("cannot list OCI tags for %s/%s: %v", agent.Publisher, agent.Name, err)
	}
	return buildInventory(agent, releases, tags, local, pinned, ociVersions, ociConfigured)
}

// buildInventory is the pure assembly step: given the versions each source
// knows about, it unions them, flags every source per version, and derives the
// latest tag and latest resolvable version. ociVersions are the tags an OCI
// registry lists as available; a version present only there still surfaces.
func buildInventory(agent *resources.Agent, releases []releaseInfo, tags, local, pinned, ociVersions []string, ociConfigured bool) inventory {
	entries := map[string]*versionEntry{}
	ensure := func(version string) (*versionEntry, bool) {
		parsed, err := semver.Parse(strings.TrimPrefix(version, "v"))
		if err != nil {
			return nil, false
		}
		key := parsed.String()
		entry, ok := entries[key]
		if !ok {
			entry = &versionEntry{Version: key, sem: parsed}
			entries[key] = entry
		}
		return entry, true
	}

	for _, release := range releases {
		if entry, ok := ensure(release.version); ok {
			entry.Sources.Tag = true
			for _, platform := range release.platforms {
				entry.ReleasePlatforms = appendUnique(entry.ReleasePlatforms, platform)
				if platform == ciPlatform {
					entry.Sources.GithubRelease = true
				}
			}
		}
	}
	for _, tag := range tags {
		if entry, ok := ensure(tag); ok {
			entry.Sources.Tag = true
		}
	}
	for _, version := range local {
		if entry, ok := ensure(version); ok {
			entry.Sources.LocalCache = true
		}
	}
	for _, version := range pinned {
		if entry, ok := ensure(version); ok {
			entry.Sources.PinnedHere = true
		}
	}
	for _, version := range ociVersions {
		if entry, ok := ensure(version); ok {
			entry.Sources.OCI = true
		}
	}

	inv := inventory{
		Agent:         fmt.Sprintf("%s/%s", agent.Publisher, agent.Name),
		CIPlatform:    ciPlatform,
		OCIConfigured: ociConfigured,
		Pinned:        pinned,
		Versions:      make([]versionEntry, 0, len(entries)),
	}
	var latestTag, latestResolvable *semver.Version
	for _, entry := range entries {
		slices.Sort(entry.ReleasePlatforms)
		inv.Versions = append(inv.Versions, *entry)
		if entry.Sources.Tag && (latestTag == nil || entry.sem.GT(*latestTag)) {
			v := entry.sem
			latestTag = &v
		}
		if entry.Sources.resolvable() && (latestResolvable == nil || entry.sem.GT(*latestResolvable)) {
			v := entry.sem
			latestResolvable = &v
		}
	}
	sort.Slice(inv.Versions, func(i, j int) bool {
		return inv.Versions[i].sem.GT(inv.Versions[j].sem)
	})
	if latestTag != nil {
		inv.LatestTag = latestTag.String()
	}
	if latestResolvable != nil {
		inv.LatestResolvable = latestResolvable.String()
	}
	return inv
}

// agentPin is a single service's pinned agent, tagged with the module the
// service lives in.
type agentPin struct {
	module string
	agent  *resources.Agent
}

type agentSummary struct {
	Agent            string   `json:"agent"`
	Pinned           string   `json:"pinned"`
	PinnedResolvable bool     `json:"pinned_resolvable"`
	LatestResolvable string   `json:"latest_resolvable"`
	LatestTag        string   `json:"latest_tag"`
	Modules          []string `json:"modules,omitempty"`
}

// workspacePins enumerates every service in the workspace and returns those
// that pin an agent, tagged with their module.
func workspacePins(ctx context.Context, workspace *resources.Workspace) ([]agentPin, error) {
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace services: %w", err)
	}
	var pins []agentPin
	for _, ref := range refs {
		service, err := workspace.LoadService(ctx, ref)
		if err != nil {
			// A single broken service shouldn't blind the whole "are all my
			// pins publishable?" overview — skip it with a warning.
			cli.Warning("cannot load service %q: %v", ref.Name, err)
			continue
		}
		if service.Agent == nil {
			continue
		}
		pins = append(pins, agentPin{module: ref.Module, agent: service.Agent})
	}
	return pins, nil
}

// summarizeWorkspaceAgents builds one summary row per distinct pinned agent.
// Inventories are cached per publisher/name so agents pinned by several
// services only hit GitHub once.
func summarizeWorkspaceAgents(ctx context.Context, pins []agentPin) []agentSummary {
	inventories := map[string]inventory{}
	rows := map[string]*agentSummary{}
	var order []string

	for _, pin := range pins {
		agent := pin.agent
		repoKey := fmt.Sprintf("%s/%s", agent.Publisher, agent.Name)
		inv, ok := inventories[repoKey]
		if !ok {
			inv = collectInventory(ctx, agent, nil)
			inventories[repoKey] = inv
		}
		pinKey := agent.Identifier()
		row, ok := rows[pinKey]
		if !ok {
			row = &agentSummary{
				Agent:            repoKey,
				Pinned:           agent.Version,
				PinnedResolvable: inv.versionResolvable(agent.Version),
				LatestResolvable: inv.LatestResolvable,
				LatestTag:        inv.LatestTag,
			}
			rows[pinKey] = row
			order = append(order, pinKey)
		}
		if pin.module != "" {
			row.Modules = appendUnique(row.Modules, pin.module)
		}
	}

	summaries := make([]agentSummary, 0, len(order))
	for _, key := range order {
		summaries = append(summaries, *rows[key])
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Agent != summaries[j].Agent {
			return summaries[i].Agent < summaries[j].Agent
		}
		return summaries[i].Pinned < summaries[j].Pinned
	})
	return summaries
}

// pinnedVersions returns the versions of the given agent pinned by services in
// the current workspace. Best-effort: outside a workspace it returns nothing so
// `agent versions` still works from anywhere.
func pinnedVersions(ctx context.Context, agent *resources.Agent) []string {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil
	}
	var versions []string
	for _, service := range services {
		if service.Agent == nil {
			continue
		}
		if service.Agent.Publisher == agent.Publisher && service.Agent.Name == agent.Name {
			versions = appendUnique(versions, service.Agent.Version)
		}
	}
	return versions
}

func fetchReleasesFromGitHub(ctx context.Context, agent *resources.Agent) ([]releaseInfo, error) {
	client := newGitHubClient()
	owner, repo := githubSource(agent)
	var out []releaseInfo
	opt := &github.ListOptions{PerPage: 100}
	for {
		releases, resp, err := client.Repositories.ListReleases(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			version := strings.TrimPrefix(release.GetTagName(), "v")
			assetPrefix := fmt.Sprintf("service-%s_%s_", agent.Name, version)
			var platforms []string
			for _, asset := range release.Assets {
				platform, ok := strings.CutPrefix(asset.GetName(), assetPrefix)
				if !ok {
					continue
				}
				if platform, ok := strings.CutSuffix(platform, ".tar.gz"); ok {
					platforms = append(platforms, platform)
				}
			}
			out = append(out, releaseInfo{version: version, platforms: platforms})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func fetchTagsFromGitHub(ctx context.Context, agent *resources.Agent) ([]string, error) {
	client := newGitHubClient()
	owner, repo := githubSource(agent)
	var out []string
	opt := &github.ListOptions{PerPage: 100}
	for {
		tags, resp, err := client.Repositories.ListTags(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			out = append(out, strings.TrimPrefix(tag.GetName(), "v"))
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// githubSource mirrors manager.toGithubSource (unexported): the publisher's
// dots become dashes and the repo is service-<name>.
func githubSource(agent *resources.Agent) (owner, repo string) {
	return strings.ReplaceAll(agent.Publisher, ".", "-"), "service-" + agent.Name
}

// newGitHubClient returns a client authenticated with GITHUB_TOKEN/GH_TOKEN
// when either is set. Listing every version of every pinned agent multiplies
// requests fast, and the unauthenticated 60/hour limit turns this diagnostic
// flaky exactly when a workspace has many pins to check.
func newGitHubClient() *github.Client {
	token := githubToken()
	if token == "" {
		return github.NewClient(nil)
	}
	return github.NewClient(&http.Client{Transport: &tokenTransport{token: token}})
}

// githubToken resolves a GitHub token from GITHUB_TOKEN/GH_TOKEN, falling back
// to the `gh` CLI's stored credential. Without the `gh` fallback, `agent list`/
// `versions` runs unauthenticated (60 req/hour) and reports resolvable versions
// as "-" the moment a workspace has several pins to check — a confusing false
// negative on a machine that is in fact fully authenticated via `gh`.
func githubToken() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type tokenTransport struct {
	token string
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func localCacheVersions(ctx context.Context, agent *resources.Agent) []string {
	dir := filepath.Join(resources.AgentBase(ctx), "agents", agentSubdir(agent), agent.Publisher)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix := agent.Name + "__"
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		version := strings.TrimPrefix(name, prefix)
		if _, err := semver.Parse(version); err != nil {
			continue
		}
		out = append(out, version)
	}
	return out
}

func agentSubdir(agent *resources.Agent) string {
	switch {
	case agent.IsApplication():
		return "applications"
	case agent.IsToolbox():
		return "toolboxes"
	case agent.IsService():
		return "services"
	default:
		return "modules"
	}
}

// fetchOCITagsFromRegistry lists the versions an OCI registry publishes for the
// agent via the distribution-spec tags/list endpoint. Listing (rather than
// probing known versions one at a time) is what lets a version published only
// to OCI still appear in the inventory. The first return reports whether a
// registry is configured at all, so the OCI column can distinguish "absent"
// from "not checked".
func fetchOCITagsFromRegistry(ctx context.Context, agent *resources.Agent) (bool, []string, error) {
	registry := strings.TrimSpace(os.Getenv("AGENT_REGISTRY"))
	if registry == "" {
		return false, nil, nil
	}
	scheme := strings.TrimSpace(os.Getenv("AGENT_REGISTRY_SCHEME"))
	if scheme == "" {
		if strings.HasPrefix(registry, "localhost") || strings.HasPrefix(registry, "127.0.0.1") {
			scheme = "http"
		} else {
			scheme = "https"
		}
	}
	url := fmt.Sprintf("%s://%s/v2/agents/%s/%s/tags/list", scheme, registry, agent.Publisher, agent.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return true, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true, nil, fmt.Errorf("registry returned %d", resp.StatusCode)
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return true, nil, err
	}
	for i := range payload.Tags {
		payload.Tags[i] = strings.TrimPrefix(payload.Tags[i], "v")
	}
	return true, payload.Tags, nil
}

func renderInventory(inv inventory) {
	cli.Header(1, "%s", inv.Agent)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tTAG\tGITHUB-RELEASE\tOCI\tPINNED-HERE\tLOCAL-CACHE")
	for _, entry := range inv.Versions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.Version,
			presence(entry.Sources.Tag),
			releaseCell(entry),
			ociMark(entry.Sources.OCI, inv.OCIConfigured),
			presence(entry.Sources.PinnedHere),
			presence(entry.Sources.LocalCache),
		)
	}
	_ = tw.Flush()

	fmt.Printf("\nGitHub-release: ✓/✗ = %s asset present (CI-downloadable); any other platforms shipped are listed in parentheses\n", inv.CIPlatform)
	if !inv.OCIConfigured {
		fmt.Println("OCI column: not checked (set AGENT_REGISTRY)")
	}
	fmt.Printf("latest tag        -> %s\n", dashIfEmpty(inv.LatestTag))
	fmt.Printf("latest resolvable -> %s\n", dashIfEmpty(inv.LatestResolvable))
	if inv.LatestTag != "" && !inv.versionResolvable(inv.LatestTag) {
		fmt.Printf("  warning: latest tag %s has no downloadable artifact\n", inv.LatestTag)
	}
	for _, pin := range inv.Pinned {
		fmt.Printf("pinned            -> %s (resolvable: %s)\n", pin, yesNo(inv.versionResolvable(pin)))
	}
}

func renderSummaries(summaries []agentSummary) {
	if len(summaries) == 0 {
		cli.Info("No agents pinned in the workspace")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tPINNED\tRESOLVABLE\tLATEST-RESOLVABLE\tLATEST-TAG\tMODULES")
	for _, summary := range summaries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			summary.Agent,
			summary.Pinned,
			yesNo(summary.PinnedResolvable),
			dashIfEmpty(summary.LatestResolvable),
			dashIfEmpty(summary.LatestTag),
			dashIfEmpty(strings.Join(summary.Modules, ", ")),
		)
	}
	_ = tw.Flush()
}

func writeJSON(payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

// releaseCell renders the GitHub-release column. ✓/✗ reflects whether the
// CI-platform asset (the resolvability signal) is present; any platforms the
// release actually ships are listed so a host-only asset isn't misread as
// "no artifact at all".
func releaseCell(entry versionEntry) string {
	if entry.Sources.GithubRelease {
		return "✓ " + strings.Join(entry.ReleasePlatforms, ",")
	}
	if len(entry.ReleasePlatforms) > 0 {
		return "✗ (" + strings.Join(entry.ReleasePlatforms, ",") + ")"
	}
	return "✗"
}

// mark is for columns where absence is a meaningful "no" (a release asset that
// isn't there): present ✓, absent ✗.
func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// presence is for columns where absence is just "not applicable" (this version
// isn't tagged / pinned / cached) rather than a failure: present ✓, absent "-".
func presence(ok bool) string {
	if ok {
		return "✓"
	}
	return "-"
}

func ociMark(ok, configured bool) string {
	if !configured {
		return "-"
	}
	return mark(ok)
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func appendUnique(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}
