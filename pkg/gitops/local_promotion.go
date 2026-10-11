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
// (presence or authority), its name and namespace, and the file it is in.
type DeliveryJob struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
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
		if entry.IsDir() || (entry.Name() != "deliver-"+deliveryPresence+".yaml" && entry.Name() != "deliver-"+deliveryAuthority+".yaml") {
			return nil
		}
		if path.Base(path.Dir(file)) != environment || path.Base(path.Dir(path.Dir(file))) != "overlays" {
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
			Kind: job.Metadata.Labels[deliveryLabel], Name: job.Metadata.Name, Namespace: job.Metadata.Namespace, Path: file,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Path < jobs[j].Path })
	return jobs, nil
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
