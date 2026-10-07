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
	all, err := renderedWorkloads(filepath.Join(owned, filepath.FromSlash(unit.Path)), opts.Environment)
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", unit.Name, err)
	}
	// Presence names what MINTS under the service's principal: its serving
	// workloads. A bootstrap Job or CronJob of the unit runs its own image and
	// never authenticates as the service, so it is not an approved build —
	// listed here, its image would be one the host accepts a token from.
	var rendered []renderedWorkload
	for index := range all {
		switch all[index].Kind {
		case kindDeployment, kindStatefulSet, kindDaemonSet:
			rendered = append(rendered, all[index])
		}
	}
	if len(rendered) == 0 {
		return nil, fmt.Errorf("artifact %s renders no serving workload (a Deployment, StatefulSet or DaemonSet), yet it is a backend artifact; a generation that renders something to run names what runs it", unit.Name)
	}
	if unit.Subject == "" {
		return nil, fmt.Errorf("artifact %s declares no workload identity in environment %s; a presence document names the principal the host must expect, and this workload authenticates as nothing (declare service-identity for %s)", unit.Name, opts.Environment, unit.Name)
	}
	workloads := make([]solutionhost.Workload, 0, len(rendered))
	for index := range rendered {
		workload := &rendered[index]
		// The identity a document names is the account's, and the namespace's
		// default account is every pod's that names none: a workload running as
		// it would be declared under an identity any pod in the namespace can
		// present. A serving workload names its own account.
		if workload.ServiceAccount == deliveryServiceAccount {
			// The delivery account is the delivery Job's: a serving workload
			// presenting its identity would be a deployment caller and a
			// runtime caller under one account, the collision the host's
			// distinction between the two assumes absent.
			return nil, fmt.Errorf("artifact %s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job; a serving workload names an account of its own", unit.Name, workload.Name, deliveryServiceAccount)
		}
		if workload.ServiceAccount == defaultServiceAccount {
			return nil, fmt.Errorf("artifact %s workload %s runs as the namespace's default ServiceAccount, whose identity every pod of the namespace that names no account shares; a workload a host admits names its own account (spec.template.spec.serviceAccountName)", unit.Name, workload.Name)
		}
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
//
// It also refuses a pod template in which a token minted for an audience is
// mounted by any container but the authenticating one. The host cannot close
// that gap from the document: a projected token is the pod's, so a sidecar
// mounting it presents it as the workload and the host cannot tell which
// container asked. Refusing it here, keyed on the token's explicit audience
// (the token the ServiceAccount plugin injects is projected too and mounted
// everywhere), puts the failure in front of whoever wrote the pod template,
// at publish, rather than in front of an operator reading a denial at rollout.
func authenticatingContainer(service string, workload *renderedWorkload) (renderedContainer, []string, error) {
	var chosen *renderedContainer
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
		return renderedContainer{}, nil, fmt.Errorf("none of its containers (%s) is named %q, so the one that authenticates cannot be told from a sidecar", strings.Join(names, ", "), service)
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
	if err := refuseSharedTokens(chosen.Name, workload); err != nil {
		return renderedContainer{}, nil, err
	}
	return *chosen, others, nil
}

// refuseSharedTokens refuses a pod template in which a projected token minted
// for an explicit audience is mounted by a container other than the one that
// authenticates — a sidecar or an init container, which would present it as
// the workload.
func refuseSharedTokens(authenticating string, workload *renderedWorkload) error {
	var shared []string
	for _, container := range append(append([]renderedContainer(nil), workload.Containers...), workload.InitContainers...) {
		if container.Name == authenticating {
			continue
		}
		for _, mount := range workload.tokenMounts[container.Name] {
			shared = append(shared, fmt.Sprintf("%s mounts %s (audience %q)", container.Name, mount.volume, mount.audience))
		}
	}
	if len(shared) == 0 {
		return nil
	}
	sort.Strings(shared)
	return fmt.Errorf("a projected token minted for an audience is mounted by a container other than the authenticating one, %q: %s; a token the pod shares is a token a sidecar can present as the workload, and the cell's admission refuses the pod for it, so mount it into %q alone",
		authenticating, strings.Join(shared, ", "), authenticating)
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
		file, openErr := openWithin(moduleDir, relative)
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
