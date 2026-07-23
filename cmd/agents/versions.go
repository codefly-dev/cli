package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/blang/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/manager"
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
)

// releaseInfo is one published GitHub release, reduced to what resolvability
// needs: the version it tags and whether it carries the CI-platform asset.
type releaseInfo struct {
	version    string
	hasCIAsset bool
}

// sourceFlags records, per version, which sources can supply it.
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
	sem     semver.Version
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

		agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, args[0])
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
	ociConfigured, ociAvailable := ociVersionChecker(ctx, agent)
	return buildInventory(agent, releases, tags, local, pinned, ociConfigured, ociAvailable)
}

// buildInventory is the pure assembly step: given the versions each source
// knows about, it unions them, flags every source per version, and derives the
// latest tag and latest resolvable version.
func buildInventory(agent *resources.Agent, releases []releaseInfo, tags, local, pinned []string, ociConfigured bool, ociAvailable func(version string) bool) inventory {
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
			if release.hasCIAsset {
				entry.Sources.GithubRelease = true
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
	if ociConfigured {
		for _, entry := range entries {
			if ociAvailable(entry.Version) {
				entry.Sources.OCI = true
			}
		}
	}

	inv := inventory{
		Agent:         fmt.Sprintf("%s/%s", agent.Publisher, agent.Name),
		CIPlatform:    ciPlatform,
		OCIConfigured: ociConfigured,
		Pinned:        pinned,
	}
	var latestTag, latestResolvable *semver.Version
	for _, entry := range entries {
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
			return nil, fmt.Errorf("load service %q: %w", ref.Name, err)
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
			wantAsset := fmt.Sprintf("service-%s_%s_%s.tar.gz", agent.Name, version, ciPlatform)
			hasAsset := false
			for _, asset := range release.Assets {
				if asset.GetName() == wantAsset {
					hasAsset = true
					break
				}
			}
			out = append(out, releaseInfo{version: version, hasCIAsset: hasAsset})
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
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if token == "" {
		return github.NewClient(nil)
	}
	return github.NewClient(&http.Client{Transport: &tokenTransport{token: token}})
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

// ociVersionChecker reports whether an OCI registry is configured and, if so,
// returns a probe for whether a given version's manifest exists there.
func ociVersionChecker(ctx context.Context, agent *resources.Agent) (bool, func(version string) bool) {
	store := manager.NewOCIStoreFromEnv(slog.Default())
	if store == nil {
		return false, func(string) bool { return false }
	}
	return true, func(version string) bool {
		probe := *agent
		probe.Version = version
		ok, err := store.Available(ctx, &probe)
		return err == nil && ok
	}
}

func renderInventory(inv inventory) {
	cli.Header(1, "%s", inv.Agent)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tTAG\tGITHUB-RELEASE\tOCI\tPINNED-HERE\tLOCAL-CACHE")
	for _, entry := range inv.Versions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.Version,
			mark(entry.Sources.Tag),
			mark(entry.Sources.GithubRelease),
			ociMark(entry.Sources.OCI, inv.OCIConfigured),
			mark(entry.Sources.PinnedHere),
			mark(entry.Sources.LocalCache),
		)
	}
	_ = tw.Flush()

	fmt.Printf("\nCI platform: %s | GitHub-release column = downloadable %s asset\n", inv.CIPlatform, inv.CIPlatform)
	if !inv.OCIConfigured {
		fmt.Println("OCI column: not checked (set AGENT_REGISTRY)")
	}
	fmt.Printf("latest tag        -> %s\n", dashIfEmpty(inv.LatestTag))
	fmt.Printf("latest resolvable -> %s\n", dashIfEmpty(inv.LatestResolvable))
	if inv.LatestTag != "" && inv.LatestTag != inv.LatestResolvable {
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

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
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
