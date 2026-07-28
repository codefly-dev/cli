package gitops

import (
	"context"
	"encoding/json"
	"fmt"
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

type argoApplication struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Project     string `json:"project"`
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
		Destinations []struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destinations"`
	} `json:"spec"`
}

func Observe(ctx context.Context, request ObserveRequest) (ObserveResult, error) {
	if request.WorkspaceRoot == "" || request.Module == "" || request.Environment == "" {
		return ObserveResult{}, fmt.Errorf("workspace root, module, and environment are required")
	}
	if err := validatePathComponent("module", request.Module); err != nil {
		return ObserveResult{}, err
	}
	if err := validatePathComponent("environment", request.Environment); err != nil {
		return ObserveResult{}, err
	}
	if request.AppProject == "" {
		return ObserveResult{}, fmt.Errorf("selected AppProject is required")
	}
	if len(request.Applications) == 0 {
		return ObserveResult{}, fmt.Errorf("at least one Argo CD application is required")
	}
	if request.Revision == "" || request.Commit == "" || request.Tree == "" || request.RenderDigest == "" {
		return ObserveResult{}, fmt.Errorf("revision, signed commit, tree, and render digest are required")
	}
	for label, value := range map[string]string{
		"revision": request.Revision, "signed commit": request.Commit, "tree": request.Tree,
	} {
		if !gitObjectPattern.MatchString(value) {
			return ObserveResult{}, fmt.Errorf("%s must be an exact Git object ID", label)
		}
	}
	if !digestPattern.MatchString(request.RenderDigest) {
		return ObserveResult{}, fmt.Errorf("render digest must be an exact SHA-256 digest")
	}
	for _, application := range request.Applications {
		if err := validateArgoName("application", application); err != nil {
			return ObserveResult{}, err
		}
	}
	if err := validateArgoName("AppProject", request.AppProject); err != nil {
		return ObserveResult{}, err
	}
	review, err := observeReview(ctx, request.PullRequest, request.Revision, request.Commit)
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
	completed := map[string]ApplicationEvidence{}
	last := map[string]ApplicationEvidence{}
	for len(completed) != len(names) {
		for _, name := range names {
			if _, ok := completed[name]; ok {
				continue
			}
			app, evidence, done, err := observeApplication(observeCtx, project, name, request.Revision)
			if err != nil {
				return ObserveResult{}, err
			}
			last[name] = evidence
			if app.Spec.Project != request.AppProject {
				return ObserveResult{}, fmt.Errorf("Argo CD application %s belongs to AppProject %s, expected %s", name, app.Spec.Project, request.AppProject)
			}
			if done {
				completed[name] = evidence
			}
		}
		if len(completed) == len(names) {
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
		Review: review, ArgoRevision: request.Revision, Health: "Healthy", ObservedAt: time.Now().UTC(),
	}
	for _, name := range names {
		item := completed[name]
		if evidence.Cluster == "" {
			evidence.Cluster = item.Cluster
		} else if evidence.Cluster != item.Cluster {
			return ObserveResult{}, fmt.Errorf("applications reconcile to different clusters: %s and %s", evidence.Cluster, item.Cluster)
		}
		evidence.Applications = append(evidence.Applications, item)
	}
	filename := request.Module + "-" + request.Environment + "-" + request.Revision + ".json"
	if err := writeReceipt(request.WorkspaceRoot, "evidence", filename, evidence); err != nil {
		return ObserveResult{}, err
	}
	return ObserveResult{Path: filepath.Join(request.WorkspaceRoot, ".codefly", "gitops", "evidence", filename), Evidence: evidence}, nil
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
	return project, nil
}

func observeApplication(ctx context.Context, project argoProject, name, expectedRevision string) (argoApplication, ApplicationEvidence, bool, error) {
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
	for _, condition := range app.Status.Conditions {
		kind := strings.ToLower(condition.Type + " " + condition.Message)
		if strings.Contains(kind, "sharedresource") || strings.Contains(kind, "shared resource") || strings.Contains(kind, "repeatedresource") {
			return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("Argo CD application %s reports shared resources: %s", name, condition.Message)
		}
	}
	cluster := app.Spec.Destination.Server
	if cluster == "" {
		cluster = app.Spec.Destination.Name
	}
	if !projectAllows(project, app.Spec.Destination.Server, app.Spec.Destination.Name, app.Spec.Destination.Namespace) {
		return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("Argo CD application %s destination is outside AppProject %s", name, project.Metadata.Name)
	}
	for _, resource := range app.Status.Resources {
		if resource.Namespace != "" && resource.Namespace != app.Spec.Destination.Namespace {
			return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf(
				"Argo CD application %s resource %s/%s is outside destination namespace %s",
				name, resource.Kind, resource.Name, app.Spec.Destination.Namespace,
			)
		}
	}
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
		return argoApplication{}, ApplicationEvidence{}, false, fmt.Errorf("Argo CD application %s uses multiple source revisions; exact publication identity is ambiguous", name)
	}
	evidence := ApplicationEvidence{
		Name: name, Project: app.Spec.Project, Sync: app.Status.Sync.Status,
		Health: app.Status.Health.Status, Operation: app.Status.OperationState.Phase,
		Revision: revision, Cluster: cluster, DestinationNamespace: app.Spec.Destination.Namespace,
	}
	switch app.Status.OperationState.Phase {
	case "Error", "Failed":
		return app, evidence, false, fmt.Errorf("Argo CD application %s operation %s", name, app.Status.OperationState.Phase)
	}
	done := app.Status.Sync.Status == "Synced" && app.Status.Health.Status == "Healthy" && app.Status.OperationState.Phase == "Succeeded"
	if done {
		for _, observed := range []string{revision, app.Status.OperationState.SyncResult.Revision} {
			if observed != "" && observed != expectedRevision {
				return app, evidence, false, fmt.Errorf("Argo CD application %s reconciled revision %s, expected %s", name, observed, expectedRevision)
			}
		}
		if revision == "" {
			return app, evidence, false, fmt.Errorf("Argo CD application %s did not report a reconciled revision", name)
		}
	}
	return app, evidence, done, nil
}

func projectAllows(project argoProject, server, name, namespace string) bool {
	for _, destination := range project.Spec.Destinations {
		clusterMatches := destination.Server != "" && destination.Server == server ||
			destination.Name != "" && destination.Name == name
		if clusterMatches && destination.Namespace == namespace {
			return true
		}
	}
	return false
}

func observeReview(ctx context.Context, pullRequest, expectedRevision, publishedCommit string) (ReviewEvidence, error) {
	if pullRequest == "" {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request is required")
	}
	if strings.HasPrefix(pullRequest, "file://") {
		return ReviewEvidence{
			URL: pullRequest, State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED",
			MergeCommit: expectedRevision,
		}, nil
	}
	if !githubPullPattern.MatchString(pullRequest) {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request must be a canonical GitHub URL")
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
	if response.State != "MERGED" {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request is %s, expected MERGED", response.State)
	}
	if response.ReviewDecision != "APPROVED" {
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
		if review.State == "APPROVED" && review.Author.Login != "" {
			evidence.Reviewers = append(evidence.Reviewers, review.Author.Login)
		}
	}
	sort.Strings(evidence.Reviewers)
	if len(evidence.Reviewers) == 0 {
		return ReviewEvidence{}, fmt.Errorf("promotion pull request has no approving review")
	}
	return evidence, nil
}
