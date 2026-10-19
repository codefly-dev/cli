package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// What a local cluster run (cli#918) reads back from the local delivery
// repository after it promoted: the revision to serve, the promoted tree whose
// bootstrap it applies, and the delivery Jobs whose receipts it reports.

// LatestLocalPromotion is the environment's latest local promotion: the tip
// of the base branch of the local delivery repository, which a developer's
// signed publish fast-forwards (PublishPlan.SelfPromotes). It is what the
// fetch remote serves to the cluster's Argo CD.
func LatestLocalPromotion(ctx context.Context, workspace *resources.Workspace, environment string) (string, error) {
	config, _, baseBranch, _, err := resolveGitops(workspace, environment, true)
	if err != nil {
		return "", err
	}
	out, err := gitCommand(ctx, workspace.Dir(), "ls-remote", "--exit-code", "--refs", "--", config.RepoURL, "refs/heads/"+baseBranch)
	if err != nil {
		return "", fmt.Errorf("environment %s has no local promotion on %s yet: %w", environment, baseBranch, err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || !gitObjectPattern.MatchString(fields[0]) {
		return "", fmt.Errorf("the local base branch %s resolved to %q", baseBranch, out)
	}
	return fields[0], nil
}

// ExportPromotion checks the promoted revision of the environment's local
// delivery repository out into dest (which must not exist yet).
func ExportPromotion(ctx context.Context, workspace *resources.Workspace, environment, revision, dest string) error {
	if !gitObjectPattern.MatchString(revision) {
		return fmt.Errorf("revision must be an exact Git object ID, got %q", revision)
	}
	config, _, _, _, err := resolveGitops(workspace, environment, true)
	if err != nil {
		return err
	}
	if _, err = gitCommand(ctx, filepath.Dir(dest), "clone", "--quiet", "--no-checkout", "--", config.RepoURL, dest); err != nil {
		return fmt.Errorf("clone the local delivery repository: %w", err)
	}
	if _, err = gitCommand(ctx, dest, "checkout", "--quiet", "--detach", revision); err != nil {
		return fmt.Errorf("check out the promoted revision %s: %w", revision, err)
	}
	return nil
}

// DeliveryJob is one delivery Job a promoted tree renders: its kind
// (manifest), its name and namespace, and the file it is in.
type DeliveryJob struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
	// Application is the Argo CD Application the module's ApplicationSet
	// stamps for the Job's overlay on the in-cluster tenant, as the CLI
	// rendered it: the one Application a local run may ask for a delivery.
	Application DeliveryApplication `json:"application"`
}

// DeliveryApplication is an Argo CD Application's full identity: its name and
// what it syncs, from where, to where. A local run asks for a sync only from
// the Application matching ALL of it — never one found by its path alone,
// which every tenant's Application of the overlay shares.
type DeliveryApplication struct {
	Name           string                 `json:"name"`
	RepoURL        string                 `json:"repoURL"`
	TargetRevision string                 `json:"targetRevision"`
	Path           string                 `json:"path"`
	Server         string                 `json:"server"`
	Namespace      string                 `json:"namespace"`
	Owner          ApplicationSetIdentity `json:"owner"`
}

// ApplicationSetIdentity names the rendered owner; UID is bound from the apply response.
type ApplicationSetIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	UID        string `json:"uid"`
}

// DeliveryJobsOf lists the delivery Jobs a module's promoted tree renders for
// the environment: the Job manifests under <published path>/…/overlays/<env>/.
func DeliveryJobsOf(tree, publishedPath, environment string) ([]DeliveryJob, error) {
	// The tree is read through a root confined to it, so a symlink in a
	// promoted tree can never lead the walk outside it.
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files := root.FS()
	start := path.Clean(filepath.ToSlash(publishedPath))
	if _, statErr := fs.Stat(files, start); errors.Is(statErr, fs.ErrNotExist) {
		return nil, nil
	}
	var jobs []DeliveryJob
	err = fs.WalkDir(files, start, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "deliver-"+deliveryManifest+".yaml" {
			return nil
		}
		if path.Base(path.Dir(file)) != environment || path.Base(path.Dir(path.Dir(file))) != overlaysDirectory {
			return nil
		}
		data, readErr := fs.ReadFile(files, file)
		if readErr != nil {
			return readErr
		}
		var job struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string            `yaml:"name"`
				Namespace string            `yaml:"namespace"`
				Labels    map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
		}
		if readErr = yaml.Unmarshal(data, &job); readErr != nil {
			return fmt.Errorf("read delivery Job %s: %w", file, readErr)
		}
		if job.Kind != "Job" || job.Metadata.Name == "" || job.Metadata.Namespace == "" {
			return fmt.Errorf("delivery Job %s names no Job, name or namespace", file)
		}
		jobs = append(jobs, DeliveryJob{
			Kind: job.Metadata.Labels[deliveredDocumentLabel], Name: job.Metadata.Name, Namespace: job.Metadata.Namespace, Path: file,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Path < jobs[j].Path })
	if len(jobs) == 0 {
		return jobs, nil
	}
	// A tree with no Argo bootstrap (a flat workspace) identifies no
	// Application; the jobs carry none, and a local run refuses to ask for a
	// delivery it cannot address.
	set := path.Join(start, "bootstrap", "applicationset.yaml")
	if _, statErr := fs.Stat(files, set); errors.Is(statErr, fs.ErrNotExist) {
		return jobs, nil
	}
	applications, err := inClusterApplications(files, set)
	if err != nil {
		return nil, err
	}
	for index := range jobs {
		overlay := path.Dir(jobs[index].Path)
		application, found := applications[overlay]
		if !found {
			return nil, fmt.Errorf("the module's ApplicationSet stamps no in-cluster Application for the delivery overlay %s", overlay)
		}
		jobs[index].Application = application
	}
	return jobs, nil
}

// inClusterApplications reads the module's rendered ApplicationSet and returns
// the Application it stamps on the in-cluster tenant for each overlay, by
// overlay path: the name its template gives, the repository and revision it
// syncs, and the destination.
func inClusterApplications(files fs.FS, file string) (map[string]DeliveryApplication, error) {
	data, err := fs.ReadFile(files, file)
	if err != nil {
		return nil, fmt.Errorf("read the module's ApplicationSet %s: %w", file, err)
	}
	var set argoApplicationSetManifest
	if err = yaml.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("read the module's ApplicationSet %s: %w", file, err)
	}
	if set.Kind != kindApplicationSet || len(set.Spec.Generators) == 0 || len(set.Spec.Generators[0].Matrix.Generators) != 2 {
		return nil, fmt.Errorf("%s is not the ApplicationSet the CLI renders (an in-cluster fan-out first)", file)
	}
	tenants, components := set.Spec.Generators[0].Matrix.Generators[0].List, set.Spec.Generators[0].Matrix.Generators[1].List
	if tenants == nil || components == nil || len(tenants.Elements) != 1 {
		return nil, fmt.Errorf("%s is not the ApplicationSet the CLI renders (one in-cluster tenant, then its components)", file)
	}
	var tenant argoTenantElement
	if err = remarshal(tenants.Elements[0], &tenant); err != nil || tenant.Tenant != defaultTenant || tenant.Server != inClusterServer {
		return nil, fmt.Errorf("%s does not fan out to the in-cluster tenant first (%v)", file, err)
	}
	prefix := tenant.Tenant
	if len(prefix) > tenantNameBudget {
		prefix = prefix[:tenantNameBudget]
	}
	applications := map[string]DeliveryApplication{}
	for _, raw := range components.Elements {
		var component argoComponentElement
		if err = remarshal(raw, &component); err != nil {
			return nil, err
		}
		applications[component.Overlay] = DeliveryApplication{
			Name:    prefix + "-" + component.Component,
			RepoURL: set.Spec.Template.Spec.Source.RepoURL, TargetRevision: set.Spec.Template.Spec.Source.TargetRevision,
			Path: component.Overlay, Server: tenant.Server, Namespace: component.Namespace,
			Owner: ApplicationSetIdentity{APIVersion: set.APIVersion, Kind: set.Kind, Name: set.Metadata.Name, Namespace: set.Metadata.Namespace},
		}
	}
	return applications, nil
}

// remarshal decodes a generic YAML value into a typed one.
func remarshal(value, into any) error {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(encoded, into)
}

// DeliveryReceipts are the lines of a delivery Job's log that answer for a
// document — delivered with the host's receipt, refused with its verdict, or
// given up on — and its closing summary; the retry chatter is left out.
func DeliveryReceipts(log string) []string {
	var receipts []string
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "delivered "), strings.HasPrefix(line, "refused "), strings.HasPrefix(line, "giving up on "):
			receipts = append(receipts, line)
		}
	}
	return receipts
}
