package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/solutionhost"
)

// --- What a presence document says the host runs ---
//
// A presence document names, per workload, the one container that
// authenticates, the exact build it must be running and the identity it must
// present. All three are read off the rendered unit rather than declared: the
// render pins every image by digest already, the pod template names the account
// the workload runs as, and the environment names the trust domain the host's
// issuer puts in that account's SVID.

// presenceWorkloads derives the workloads one rendered unit declares: every
// Deployment, StatefulSet or DaemonSet in its overlay, with the container named
// after the service as the authenticating one.
//
// The authenticating container is the one named like the service, because that
// is the container the render projects the service's configuration into and
// the only one whose image the service's build produced. A unit whose single
// container has another name is accepted; a unit with several containers and
// none named like the service is refused rather than guessed: "whichever one
// presented the token" is how a sidecar ends up holding a workload's authority.
func presenceWorkloads(owned string, opts *RenderOptions, unit SolutionArtifactUnit) ([]solutionhost.Workload, error) {
	rendered, err := renderedWorkloads(filepath.Join(owned, filepath.FromSlash(unit.Path)), opts.Environment)
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", unit.Name, err)
	}
	if len(rendered) == 0 {
		return nil, fmt.Errorf("artifact %s renders no workload, yet it is a backend artifact; a generation that renders something to run names what runs it", unit.Name)
	}
	if unit.Subject == "" {
		return nil, fmt.Errorf("artifact %s declares no workload identity in environment %s; a presence document names the principal the host must expect, and this workload authenticates as nothing (declare service-identity for %s)", unit.Name, opts.Environment, unit.Name)
	}
	workloads := make([]solutionhost.Workload, 0, len(rendered))
	for index := range rendered {
		workload := &rendered[index]
		authenticating, others, err := authenticatingContainer(unit.Name, workload)
		if err != nil {
			return nil, fmt.Errorf("artifact %s workload %s: %w", unit.Name, workload.Name, err)
		}
		workloads = append(workloads, solutionhost.Workload{
			Name:      workload.Name,
			Artifact:  unit.Name,
			Container: authenticating.Name,
			Image:     solutionhost.Image{Repository: authenticating.Image.Repository, Digest: solutionhost.ImageDigest(authenticating.Image.Digest)},
			Identity: solutionhost.WorkloadIdentity{
				Audience: opts.Host.Audience,
				Subject:  unit.Subject,
				SPIFFEID: opts.Host.SPIFFEID(opts.Namespace, workload.ServiceAccount),
			},
			// A pointer to a non-nil list: core refuses an absent (nil) list
			// because it cannot be told from "there are none", and
			// authenticatingContainer always returns a declaration.
			NonAuthenticating: &others,
		})
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Name < workloads[j].Name })
	return workloads, nil
}

// authenticatingContainer picks the container that authenticates for a
// service, and lists every other container — init containers included — as one
// that must never be accepted as it. An empty list is a declaration, so it is
// always returned non-nil.
func authenticatingContainer(service string, workload *CellWorkload) (CellContainer, []string, error) {
	var chosen *CellContainer
	for index := range workload.Containers {
		if workload.Containers[index].Name == service {
			chosen = &workload.Containers[index]
			break
		}
	}
	if chosen == nil && len(workload.Containers) == 1 {
		chosen = &workload.Containers[0]
	}
	if chosen == nil {
		names := make([]string, 0, len(workload.Containers))
		for _, container := range workload.Containers {
			names = append(names, container.Name)
		}
		return CellContainer{}, nil, fmt.Errorf("none of its containers (%s) is named %q, so the one that authenticates cannot be told from a sidecar", strings.Join(names, ", "), service)
	}
	others := []string{}
	for _, container := range workload.Containers {
		if container.Name != chosen.Name {
			others = append(others, container.Name)
		}
	}
	for _, container := range workload.InitContainers {
		others = append(others, container.Name)
	}
	sort.Strings(others)
	return *chosen, others, nil
}

// releaseDigest pins the release a module instance deploys: the content digest
// of the module package as the composition materialized it, every regular file
// in path order. Two renders of one version agree on it, and a changed source
// is a changed release.
//
// Version control metadata and render output are left out: neither is part of
// the package, and both change without the release changing. It is a release
// digest and never an image or a rendered-bytes digest, which core keeps as
// distinct types so the three cannot be confused.
func releaseDigest(moduleDir string) (solutionhost.ReleaseDigest, error) {
	if moduleDir == "" {
		return "", fmt.Errorf("the module has no directory, so its release cannot be digested")
	}
	type entry struct {
		path   string
		digest string
		size   int64
	}
	var entries []entry
	err := filepath.WalkDir(moduleDir, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(moduleDir, path)
		if relErr != nil {
			return relErr
		}
		if item.IsDir() {
			switch {
			case relative == ".":
				return nil
			case item.Name() == ".git", relative == "deployments", strings.HasPrefix(item.Name(), ".codefly-render-"):
				return filepath.SkipDir
			}
			return nil
		}
		if !item.Type().IsRegular() {
			return nil
		}
		file, openErr := os.Open(path) //nolint:gosec // a file of the module directory being digested
		if openErr != nil {
			return openErr
		}
		defer file.Close()
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		if copyErr != nil {
			return copyErr
		}
		entries = append(entries, entry{path: filepath.ToSlash(relative), digest: hex.EncodeToString(hash.Sum(nil)), size: size})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest the release at %s: %w", moduleDir, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("the module at %s holds no files, so its release cannot be digested", moduleDir)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hash := sha256.New()
	for _, item := range entries {
		fmt.Fprintf(hash, "%s\x00%s\x00%d\n", item.path, item.digest, item.size)
	}
	return solutionhost.ReleaseDigest("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
}
