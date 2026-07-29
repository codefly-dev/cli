package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	gitObjectPattern  = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)
	argoNamePattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	githubPullPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[1-9][0-9]*$`)
)

const (
	approvedReviewDecision = "APPROVED"
	healthyStatus          = "Healthy"
)

type argoApplication struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
		Source  struct {
			RepoURL        string `json:"repoURL"`
			Path           string `json:"path"`
			TargetRevision string `json:"targetRevision"`
		} `json:"source"`
		Sources []struct {
			RepoURL string `json:"repoURL"`
			Path    string `json:"path"`
		} `json:"sources"`
		Destination struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status    string   `json:"status"`
			Revision  string   `json:"revision"`
			Revisions []string `json:"revisions"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			Phase      string `json:"phase"`
			SyncResult struct {
				Revision  string   `json:"revision"`
				Revisions []string `json:"revisions"`
			} `json:"syncResult"`
		} `json:"operationState"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
		Resources []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"resources"`
	} `json:"status"`
}

type argoProject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		SourceRepos  []string `json:"sourceRepos"`
		Destinations []struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destinations"`
		ClusterResourceWhitelist []struct {
			Group string `json:"group"`
			Kind  string `json:"kind"`
		} `json:"clusterResourceWhitelist"`
		NamespaceResourceWhitelist []struct {
			Group string `json:"group"`
			Kind  string `json:"kind"`
		} `json:"namespaceResourceWhitelist"`
	} `json:"spec"`
}

func Observe(ctx context.Context, input *ObserveRequest) (ObserveResult, error) {
	request, err := validateObserveRequest(input)
	if err != nil {
		return ObserveResult{}, err
	}
	if err := verifyPublishedRevision(ctx, request); err != nil {
		return ObserveResult{}, err
	}
	review, err := observeReview(ctx, request.PullRequest, request.Revision, request.Commit, request.Repository, request.Local)
	if err != nil {
		return ObserveResult{}, err
	}
	project, err := loadArgoProject(ctx, request.AppProject)
	if err != nil {
		return ObserveResult{}, err
	}
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := request.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	observeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	names := append([]string(nil), request.Applications...)
	sort.Strings(names)
	last := map[string]ApplicationEvidence{}
	healthySweeps := 0
	for {
		current := map[string]ApplicationEvidence{}
		allHealthy := true
		for _, name := range names {
			_, evidence, done, err := observeApplication(observeCtx, &project, name, request)
			if err != nil {
				return ObserveResult{}, err
			}
			last[name] = evidence
			if done {
				current[name] = evidence
			} else {
				allHealthy = false
			}
		}
		if allHealthy {
			healthySweeps++
		} else {
			healthySweeps = 0
		}
		if healthySweeps == 2 {
			last = current
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-observeCtx.Done():
			timer.Stop()
			var states []string
			for _, name := range names {
				state := last[name]
				states = append(states, fmt.Sprintf("%s(sync=%s health=%s operation=%s revision=%s)", name, state.Sync, state.Health, state.Operation, state.Revision))
			}
			return ObserveResult{}, fmt.Errorf("Argo CD health observation timed out: %s", strings.Join(states, ", "))
		case <-timer.C:
		}
	}

	evidence := Evidence{
		SchemaVersion: SchemaVersion, Module: request.Module, Environment: request.Environment,
		RenderDigest: request.RenderDigest, SignedCommit: request.Commit, Tree: request.Tree,
		Review: review, Repository: request.Repository, Path: request.Path,
		ArgoRevision: request.Revision, Health: healthyStatus, ObservedAt: time.Now().UTC(),
	}
	for _, name := range names {
		item := last[name]
		if evidence.Cluster == "" {
			evidence.Cluster = item.Cluster
		} else if evidence.Cluster != item.Cluster {
			return ObserveResult{}, fmt.Errorf("applications reconcile to different clusters: %s and %s", evidence.Cluster, item.Cluster)
		}
		evidence.Applications = append(evidence.Applications, item)
	}
	clusterIdentity, err := loadClusterIdentity(ctx, evidence.Cluster)
	if err != nil {
		return ObserveResult{}, err
	}
	evidence.ClusterIdentity = clusterIdentity
	for index := range evidence.Applications {
		evidence.Applications[index].ClusterIdentity = clusterIdentity
	}
	filename := request.Module + "-" + request.Environment + "-" + request.Revision + ".json"
	if err := writeReceipt(request.WorkspaceRoot, "evidence", filename, evidence); err != nil {
		return ObserveResult{}, err
	}
	return ObserveResult{Path: filepath.Join(request.WorkspaceRoot, ".codefly", "gitops", "evidence", filename), Evidence: evidence}, nil
}

func validateObserveRequest(input *ObserveRequest) (*ObserveRequest, error) {
	if input == nil {
		return nil, fmt.Errorf("observation request is required")
	}
	request := *input
	if request.WorkspaceRoot == "" || request.Module == "" || request.Environment == "" {
		return nil, fmt.Errorf("workspace root, module, and environment are required")
	}
	if err := validatePathComponent("module", request.Module); err != nil {
		return nil, err
	}
	if err := validatePathComponent("environment", request.Environment); err != nil {
		return nil, err
	}
	if request.AppProject == "" {
		return nil, fmt.Errorf("selected AppProject is required")
	}
	if len(request.Applications) == 0 {
		return nil, fmt.Errorf("at least one Argo CD application is required")
	}
	if request.Repository == "" || request.Path == "" {
		return nil, fmt.Errorf("published repository and path are required")
	}
	if request.Local && request.Environment != "local" {
		return nil, fmt.Errorf("local review qualification is limited to the local environment")
	}
	if request.Revision == "" || request.Commit == "" || request.Tree == "" || request.RenderDigest == "" {
		return nil, fmt.Errorf("revision, signed commit, tree, and render digest are required")
	}
	for label, value := range map[string]string{
		"revision": request.Revision, "signed commit": request.Commit, "tree": request.Tree,
	} {
		if !gitObjectPattern.MatchString(value) {
			return nil, fmt.Errorf("%s must be an exact Git object ID", label)
		}
	}
	request.Revision = strings.ToLower(request.Revision)
	request.Commit = strings.ToLower(request.Commit)
	request.Tree = strings.ToLower(request.Tree)
	if !digestPattern.MatchString(request.RenderDigest) {
		return nil, fmt.Errorf("render digest must be an exact SHA-256 digest")
	}
	request.RenderDigest = strings.ToLower(request.RenderDigest)
	seenApplications := map[string]struct{}{}
	for _, application := range request.Applications {
		if err := validateArgoName("application", application); err != nil {
			return nil, err
		}
		if _, exists := seenApplications[application]; exists {
			return nil, fmt.Errorf("Argo CD application %s is selected more than once", application)
		}
		seenApplications[application] = struct{}{}
	}
	if err := validateArgoName("AppProject", request.AppProject); err != nil {
		return nil, err
	}
	return &request, nil
}

func validateArgoName(label, value string) error {
	if len(value) > 253 || !argoNamePattern.MatchString(value) {
		return fmt.Errorf("%s %q is invalid", label, value)
	}
	return nil
}

func loadArgoProject(ctx context.Context, name string) (argoProject, error) {
	output, err := command(ctx, "", "argocd", "proj", "get", name, "-o", "json")
	if err != nil {
		return argoProject{}, fmt.Errorf("observe Argo CD AppProject %s: %w", name, err)
	}
	var project argoProject
	if err := json.Unmarshal([]byte(output), &project); err != nil {
		return argoProject{}, fmt.Errorf("decode Argo CD AppProject %s: %w", name, err)
	}
	if project.Metadata.Name != name {
		return argoProject{}, fmt.Errorf("Argo CD returned AppProject %q, expected %q", project.Metadata.Name, name)
	}
	for _, destination := range project.Spec.Destinations {
		if strings.Contains(destination.Server, "*") || strings.Contains(destination.Name, "*") || strings.Contains(destination.Namespace, "*") {
			return argoProject{}, fmt.Errorf("AppProject %s contains wildcard destination authority", name)
		}
	}
	for _, repository := range project.Spec.SourceRepos {
		if strings.Contains(repository, "*") {
			return argoProject{}, fmt.Errorf("AppProject %s contains wildcard source repository authority", name)
		}
	}
	for _, whitelist := range []struct {
		label     string
		resources []struct {
			Group string `json:"group"`
			Kind  string `json:"kind"`
		}
	}{
		{label: "cluster", resources: project.Spec.ClusterResourceWhitelist},
		{label: "namespace", resources: project.Spec.NamespaceResourceWhitelist},
	} {
		for _, resource := range whitelist.resources {
			if resource.Kind == "" || strings.Contains(resource.Group, "*") || strings.Contains(resource.Kind, "*") {
				return argoProject{}, fmt.Errorf("AppProject %s contains wildcard or incomplete %s resource authority", name, whitelist.label)
			}
		}
	}
	return project, nil
}

func observeApplication(ctx context.Context, project *argoProject, name string, request *ObserveRequest) (argoApplication, ApplicationEvidence, bool, error) {
	output, err := command(ctx, "", "argocd", "app", "get", name, "--refresh", "-o", "json")
	if err != nil {
		return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("observe Argo CD application %s: %w", name, err)
	}
	var app argoApplication
	if err := json.Unmarshal([]byte(output), &app); err != nil {
		return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("decode Argo CD application %s: %w", name, err)
	}
	if app.Metadata.Name != name {
		return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("Argo CD returned application %q, expected %q", app.Metadata.Name, name)
	}
	sourcePath, err := validateApplicationSource(project, name, &app, request)
	if err != nil {
		return argoApplication{}, ApplicationEvidence{}, false, err
	}
	cluster, err := validateApplicationAuthority(project, name, &app, request.AppProject)
	if err != nil {
		return argoApplication{}, ApplicationEvidence{}, false, err
	}
	revision, err := applicationRevision(name, &app)
	if err != nil {
		return argoApplication{}, ApplicationEvidence{}, false, err
	}
	evidence := ApplicationEvidence{
		Name: name, Project: app.Spec.Project, Repository: app.Spec.Source.RepoURL, Path: sourcePath,
		Sync:   app.Status.Sync.Status,
		Health: app.Status.Health.Status, Operation: app.Status.OperationState.Phase,
		Revision: revision, Cluster: cluster, DestinationNamespace: app.Spec.Destination.Namespace,
	}
	switch app.Status.OperationState.Phase {
	case "Error", "Failed":
		return app, evidence, false, fmt.Errorf("Argo CD application %s operation %s", name, app.Status.OperationState.Phase)
	}
	done := app.Status.Sync.Status == "Synced" &&
		app.Status.Health.Status == healthyStatus &&
		app.Status.OperationState.Phase == "Succeeded"
	if !done {
		return app, evidence, false, nil
	}
	for _, observed := range []string{revision, app.Status.OperationState.SyncResult.Revision} {
		if observed != "" && observed != request.Revision {
			return app, evidence, false, fmt.Errorf("Argo CD application %s reconciled revision %s, expected %s", name, observed, request.Revision)
		}
	}
	if revision == "" {
		return app, evidence, false, fmt.Errorf("Argo CD application %s did not report a reconciled revision", name)
	}
	return app, evidence, true, nil
}

func validateApplicationSource(project *argoProject, name string, app *argoApplication, request *ObserveRequest) (string, error) {
	if len(app.Spec.Sources) > 0 {
		return "", fmt.Errorf("Argo CD application %s uses multiple sources; exact publication identity is ambiguous", name)
	}
	if app.Spec.Source.RepoURL == "" || app.Spec.Source.Path == "" {
		return "", fmt.Errorf("Argo CD application %s source repository and path are required", name)
	}
	if !projectAllowsSource(project, app.Spec.Source.RepoURL) {
		return "", fmt.Errorf("Argo CD application %s source repository is outside AppProject %s", name, project.Metadata.Name)
	}
	if !request.Local {
		matches, err := repositoriesMatch(request.Repository, app.Spec.Source.RepoURL)
		if err != nil {
			return "", err
		}
		if !matches {
			return "", fmt.Errorf("Argo CD application %s observes repository %s, expected %s", name, app.Spec.Source.RepoURL, request.Repository)
		}
	}
	sourcePath, err := validateRelativePath(app.Spec.Source.Path)
	if err != nil {
		return "", fmt.Errorf("Argo CD application %s source path: %w", name, err)
	}
	expectedPath, err := validateRelativePath(request.Path)
	if err != nil {
		return "", fmt.Errorf("published path: %w", err)
	}
	if sourcePath != expectedPath {
		return "", fmt.Errorf("Argo CD application %s observes path %s, expected %s", name, sourcePath, expectedPath)
	}
	return sourcePath, nil
}

func validateApplicationAuthority(project *argoProject, name string, app *argoApplication, appProject string) (string, error) {
	if app.Spec.Project != appProject {
		return "", fmt.Errorf("Argo CD application %s belongs to AppProject %s, expected %s", name, app.Spec.Project, appProject)
	}
	for _, condition := range app.Status.Conditions {
		kind := strings.ToLower(condition.Type + " " + condition.Message)
		if strings.Contains(kind, "sharedresource") || strings.Contains(kind, "shared resource") || strings.Contains(kind, "repeatedresource") {
			return "", fmt.Errorf("Argo CD application %s reports shared resources: %s", name, condition.Message)
		}
	}
	cluster := app.Spec.Destination.Server
	if cluster == "" {
		cluster = app.Spec.Destination.Name
	}
	if !projectAllows(project, app.Spec.Destination.Server, app.Spec.Destination.Name, app.Spec.Destination.Namespace) {
		return "", fmt.Errorf("Argo CD application %s destination is outside AppProject %s", name, project.Metadata.Name)
	}
	for _, resource := range app.Status.Resources {
		if !projectAllowsResource(project, app.Spec.Destination.Server, app.Spec.Destination.Name, resource.Group, resource.Kind, resource.Namespace) {
			return "", fmt.Errorf(
				"Argo CD application %s resource %s/%s is outside AppProject %s authority",
				name, resource.Kind, resource.Name, project.Metadata.Name,
			)
		}
	}
	return cluster, nil
}

func applicationRevision(name string, app *argoApplication) (string, error) {
	revision := app.Status.Sync.Revision
	if revision == "" && len(app.Status.Sync.Revisions) == 1 {
		revision = app.Status.Sync.Revisions[0]
	}
	if revision == "" {
		revision = app.Status.OperationState.SyncResult.Revision
	}
	if revision == "" && len(app.Status.OperationState.SyncResult.Revisions) == 1 {
		revision = app.Status.OperationState.SyncResult.Revisions[0]
	}
	if len(app.Status.Sync.Revisions) > 1 || len(app.Status.OperationState.SyncResult.Revisions) > 1 {
		return "", fmt.Errorf("Argo CD application %s uses multiple source revisions; exact publication identity is ambiguous", name)
	}
	return revision, nil
}

func projectAllows(project *argoProject, server, name, namespace string) bool {
	for _, destination := range project.Spec.Destinations {
		clusterMatches := destination.Server != "" && destination.Server == server ||
			destination.Name != "" && destination.Name == name
		if clusterMatches && destination.Namespace == namespace {
			return true
		}
	}
	return false
}

func projectAllowsSource(project *argoProject, repository string) bool {
	for _, allowed := range project.Spec.SourceRepos {
		if strings.TrimSuffix(allowed, ".git") == strings.TrimSuffix(repository, ".git") {
			return true
		}
		matches, err := repositoriesMatch(allowed, repository)
		if err == nil && matches {
			return true
		}
	}
	return false
}

func projectAllowsResource(project *argoProject, server, name, group, kind, namespace string) bool {
	if namespace == "" {
		for _, allowed := range project.Spec.ClusterResourceWhitelist {
			if allowed.Group == group && allowed.Kind == kind {
				return true
			}
		}
		return false
	}
	if !projectAllows(project, server, name, namespace) {
		return false
	}
	if len(project.Spec.NamespaceResourceWhitelist) == 0 {
		return true
	}
	for _, allowed := range project.Spec.NamespaceResourceWhitelist {
		if allowed.Group == group && allowed.Kind == kind {
			return true
		}
	}
	return false
}

func repositoriesMatch(left, right string) (bool, error) {
	leftSlug, leftErr := validateRepositoryURL(left, strings.HasPrefix(left, "file://"))
	rightSlug, rightErr := validateRepositoryURL(right, strings.HasPrefix(right, "file://"))
	if leftErr != nil {
		return false, fmt.Errorf("validate repository %s: %w", left, leftErr)
	}
	if rightErr != nil {
		return false, fmt.Errorf("validate repository %s: %w", right, rightErr)
	}
	if leftSlug != "" || rightSlug != "" {
		return leftSlug != "" && leftSlug == rightSlug, nil
	}
	leftURL, _ := filepath.Abs(strings.TrimPrefix(left, "file://"))
	rightURL, _ := filepath.Abs(strings.TrimPrefix(right, "file://"))
	return leftURL == rightURL, nil
}

func verifyPublishedRevision(ctx context.Context, request *ObserveRequest) error {
	if _, err := validateRepositoryURL(request.Repository, request.Local); err != nil {
		return fmt.Errorf("published repository: %w", err)
	}
	targetPath, err := validateRelativePath(request.Path)
	if err != nil {
		return fmt.Errorf("published path: %w", err)
	}
	temp, err := os.MkdirTemp("", "codefly-gitops-observe-")
	if err != nil {
		return fmt.Errorf("create observation checkout: %w", err)
	}
	defer os.RemoveAll(temp)
	repo := filepath.Join(temp, "repo")
	if _, err := gitCommand(ctx, temp, "clone", "--quiet", "--no-checkout", "--", request.Repository, repo); err != nil {
		return fmt.Errorf("clone published repository: %w", err)
	}
	revision, err := gitCommand(ctx, repo, "rev-parse", request.Revision+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve published revision %s: %w", request.Revision, err)
	}
	if revision != request.Revision {
		return fmt.Errorf("published revision resolved to %s, expected %s", revision, request.Revision)
	}
	commit, err := gitCommand(ctx, repo, "rev-parse", request.Commit+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve signed publication commit %s: %w", request.Commit, err)
	}
	if commit != request.Commit {
		return fmt.Errorf("signed publication commit resolved to %s, expected %s", commit, request.Commit)
	}
	if _, err := gitCommand(ctx, repo, "merge-base", "--is-ancestor", request.Commit, request.Revision); err != nil {
		return fmt.Errorf("signed publication commit %s is not contained in reviewed revision %s", request.Commit, request.Revision)
	}
	tree, err := gitCommand(ctx, repo, "rev-parse", request.Commit+"^{tree}")
	if err != nil {
		return err
	}
	if tree != request.Tree {
		return fmt.Errorf("signed publication commit tree is %s, expected %s", tree, request.Tree)
	}
	rawCommit, err := gitCommand(ctx, repo, "cat-file", "-p", request.Commit)
	if err != nil {
		return err
	}
	if !strings.Contains(rawCommit, "\ngpgsig ") {
		return fmt.Errorf("publication commit %s is not signed", request.Commit)
	}
	if _, err := gitCommand(ctx, repo, "checkout", "--quiet", request.Revision, "--", targetPath); err != nil {
		return fmt.Errorf("checkout published path %s at %s: %w", targetPath, request.Revision, err)
	}
	target := filepath.Join(repo, filepath.FromSlash(targetPath))
	if err := ValidateRenderedTree(target, request.AppProject, true); err != nil {
		return fmt.Errorf("validate reconciled Git tree: %w", err)
	}
	inventory, err := LoadInventory(target)
	if err != nil {
		return err
	}
	if inventory.Digest != request.RenderDigest {
		return fmt.Errorf("reconciled Git tree digest is %s, expected %s", inventory.Digest, request.RenderDigest)
	}
	return nil
}

func loadClusterIdentity(ctx context.Context, cluster string) (string, error) {
	output, err := command(ctx, "", "argocd", "cluster", "get", cluster, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("observe Argo CD cluster %s: %w", cluster, err)
	}
	var value struct {
		Server string         `json:"server"`
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		return "", fmt.Errorf("decode Argo CD cluster %s: %w", cluster, err)
	}
	if value.Server == "" && value.Name == "" {
		return "", fmt.Errorf("Argo CD cluster %s has no registered identity", cluster)
	}
	if value.Server != cluster && value.Name != cluster {
		return "", fmt.Errorf("Argo CD returned cluster %s/%s, expected %s", value.Name, value.Server, cluster)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func observeReview(ctx context.Context, pullRequest, expectedRevision, publishedCommit, repository string, local bool) (ReviewEvidence, error) {
	if pullRequest == "" {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request is required")
	}
	if strings.HasPrefix(pullRequest, "file://") {
		if !local {
			return ReviewEvidence{}, fmt.Errorf("local review references require explicit local qualification")
		}
		parts := strings.Split(pullRequest, "#")
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "refs/codefly/reviews/") {
			return ReviewEvidence{}, fmt.Errorf("local promotion review must name an exact codefly review ref")
		}
		matches, err := repositoriesMatch(parts[0], repository)
		if err != nil || !matches {
			return ReviewEvidence{}, fmt.Errorf("local promotion review repository differs from published repository")
		}
		output, err := gitCommand(ctx, "", "ls-remote", "--exit-code", "--refs", parts[0], parts[1])
		if err != nil {
			return ReviewEvidence{}, fmt.Errorf("verify local promotion review ref: %w", err)
		}
		fields := strings.Fields(output)
		if len(fields) != 2 || fields[0] != publishedCommit || publishedCommit != expectedRevision {
			return ReviewEvidence{}, fmt.Errorf("local promotion review ref resolves to %q, expected %s", output, expectedRevision)
		}
		return ReviewEvidence{
			URL: pullRequest, State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED",
			MergeCommit: expectedRevision,
		}, nil
	}
	if !githubPullPattern.MatchString(pullRequest) {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request must be a canonical GitHub URL")
	}
	repositorySlug, err := validateRepositoryURL(repository, false)
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("validate published repository: %w", err)
	}
	parsedPullRequest, err := url.Parse(pullRequest)
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("parse promotion pull request: %w", err)
	}
	segments := strings.Split(strings.Trim(parsedPullRequest.Path, "/"), "/")
	if len(segments) != 4 || segments[0]+"/"+strings.TrimSuffix(segments[1], ".git") != repositorySlug {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request repository differs from published repository")
	}
	output, err := command(ctx, "", "gh", "pr", "view", pullRequest,
		"--json", "url,state,reviewDecision,reviews,mergeCommit,commits")
	if err != nil {
		return ReviewEvidence{}, fmt.Errorf("observe promotion review: %w", err)
	}
	var response struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		ReviewDecision string `json:"reviewDecision"`
		Reviews        []struct {
			State  string `json:"state"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"reviews"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		Commits []struct {
			OID string `json:"oid"`
		} `json:"commits"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return ReviewEvidence{}, fmt.Errorf("decode promotion review: %w", err)
	}
	if response.URL != pullRequest {
		return ReviewEvidence{}, fmt.Errorf("GitHub returned promotion pull request %s, expected %s", response.URL, pullRequest)
	}
	if response.State != "MERGED" {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request is %s, expected MERGED", response.State)
	}
	if response.ReviewDecision != approvedReviewDecision {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request review decision is %s, expected APPROVED", response.ReviewDecision)
	}
	if response.MergeCommit.OID != expectedRevision {
		return ReviewEvidence{}, fmt.Errorf("promotion merge revision is %s, expected %s", response.MergeCommit.OID, expectedRevision)
	}
	published := false
	for _, commit := range response.Commits {
		if commit.OID == publishedCommit {
			published = true
			break
		}
	}
	if !published {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request does not contain signed commit %s", publishedCommit)
	}
	evidence := ReviewEvidence{
		URL: response.URL, State: response.State, ReviewDecision: response.ReviewDecision,
		MergeCommit: response.MergeCommit.OID,
	}
	for _, review := range response.Reviews {
		if review.State == approvedReviewDecision && review.Author.Login != "" {
			evidence.Reviewers = append(evidence.Reviewers, review.Author.Login)
		}
	}
	sort.Strings(evidence.Reviewers)
	if len(evidence.Reviewers) == 0 {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request has no approving review")
	}
	return evidence, nil
}
