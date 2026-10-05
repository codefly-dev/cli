package environments

import (
	"fmt"
	"slices"
	"strings"
)

// localClusterKinds run their nodes on the machine that builds the images: a
// k3d, kind or minikube cluster is containers in the local container engine.
var localClusterKinds = map[string]bool{ClusterKindK3d: true, "kind": true, "minikube": true}

// DeploysToCell reports whether this environment deploys to a cell — somewhere
// other than the machine running the command — which is what makes a restricted
// render subject to the deployed security posture (see pkg/posture).
//
// It is decided by what the environment DECLARES, never by its name: a k3d, kind
// or minikube cluster runs its nodes in the local container engine, and anything
// else is a cell. An environment that declares no cluster kind counts as a cell:
// the posture is the last door before one, and a classification that defaulted
// the other way would make an undeclared environment the way around it.
func (env *Environment) DeploysToCell() bool {
	if env == nil {
		return false
	}
	if env.Cluster == nil || env.Cluster.Kind == "" {
		return true
	}
	return !localClusterKinds[env.Cluster.Kind]
}

// imageArchitectures are the node architectures an environment may declare,
// named as Go and OCI name them.
var imageArchitectures = map[string]bool{
	"amd64": true, "arm64": true, "arm": true, "386": true,
	"ppc64le": true, "s390x": true, "riscv64": true,
}

// ImagePlatforms is the set of image platforms ("linux/<arch>") a build for
// this environment must produce: the architectures its cluster's nodes run, so
// every image runs natively on every node and nothing else is built.
//
// A cluster declares them (cluster.architectures). A local cluster (k3d, kind,
// minikube) runs on the machine building the images, so when it declares none
// its architecture is that engine's, hostArchitecture. A remote cluster that
// declares none, or an environment with no cluster, is refused: the platforms
// an image needs are a property of the nodes, and guessing them is how a render
// ended up emulating an architecture no node runs.
func (env *Environment) ImagePlatforms(hostArchitecture func() (string, error)) ([]string, error) {
	if env.Cluster == nil || env.Cluster.Kind == "" {
		return nil, fmt.Errorf("environment %q declares no cluster, so the architectures its images must run on are unknown: declare cluster.kind and cluster.architectures (for example [amd64])", env.Name)
	}
	architectures := env.Cluster.Architectures
	if len(architectures) == 0 {
		if !localClusterKinds[env.Cluster.Kind] {
			return nil, fmt.Errorf("environment %q deploys to a %s cluster whose node architectures are not declared: set cluster.architectures (for example [amd64]) to the architectures its nodes run", env.Name, env.Cluster.Kind)
		}
		host, err := hostArchitecture()
		if err != nil {
			return nil, fmt.Errorf("environment %q runs a local %s cluster, whose nodes have the container engine's architecture, which cannot be read: %w", env.Name, env.Cluster.Kind, err)
		}
		architectures = []string{host}
	}
	platforms := make([]string, 0, len(architectures))
	for _, architecture := range architectures {
		architecture = strings.TrimSpace(architecture)
		if !imageArchitectures[architecture] {
			return nil, fmt.Errorf("environment %q cluster.architectures names %q, which is not an architecture (use Go/OCI names such as amd64, arm64)", env.Name, architecture)
		}
		platform := "linux/" + architecture
		if slices.Contains(platforms, platform) {
			return nil, fmt.Errorf("environment %q cluster.architectures lists %q twice", env.Name, architecture)
		}
		platforms = append(platforms, platform)
	}
	return platforms, nil
}
