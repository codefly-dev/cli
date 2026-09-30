package deploysecrets

import (
	"fmt"
	"slices"
	"sort"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/solutionrun"
)

type propertyLocation struct{ remote, property string }

// propertyReaders uses the bindings read from the render. Older in-memory
// callers can omit them only when the association is unambiguous; on-disk
// renders always provide them, including renders made before this field existed.
func propertyReaders(secret *gitops.RenderedServiceSecret, property gitops.RenderedSecretProperty) ([]gitops.RenderedSecretReader, error) {
	if len(property.Readers) > 0 {
		return property.Readers, nil
	}
	if len(secret.Services) > 1 && (len(secret.Properties) > 1 || len(property.Keys) > 1) {
		return nil, fmt.Errorf("remote key %s property %s records no service/key bindings: re-read the rendered environment", secret.RemoteKey, property.Property)
	}
	var readers []gitops.RenderedSecretReader
	for _, service := range secret.Services {
		for _, key := range property.Keys {
			readers = append(readers, gitops.RenderedSecretReader{Service: service, Key: key})
		}
	}
	return readers, nil
}

// configurationAliases finds connected properties, not just equal key spellings
// on one pair. A property reading A+B makes all other properties reading either
// key carry one value. Federation identities remain service-specific.
func configurationAliases(states []*remoteState, derivations map[string]map[string]solutionrun.SecretDerivation) (map[propertyLocation][]string, error) {
	type node struct {
		location propertyLocation
		keys     []string
		derived  bool
	}
	var nodes []node
	var parent []int
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	byKey := map[string]int{}
	for _, state := range states {
		for _, property := range state.secret.Properties {
			_, derived := derivations[state.secret.RemoteKey][property.Property]
			i := len(nodes)
			nodes = append(nodes, node{propertyLocation{state.secret.RemoteKey, property.Property}, property.Keys, derived})
			parent = append(parent, i)
			for _, key := range property.Keys {
				if !configurationKey(key) {
					continue
				}
				if previous, ok := byKey[key]; ok {
					if nodes[previous].derived != derived {
						return nil, fmt.Errorf("configuration key %s mixes federation and configured sources; correct the rendered mappings", key)
					}
					if !derived {
						parent[find(i)] = find(previous)
					}
				} else {
					byKey[key] = i
				}
			}
		}
	}
	groups := map[int][]string{}
	for i, n := range nodes {
		if n.derived {
			continue
		}
		root := find(i)
		for _, key := range n.keys {
			if !slices.Contains(groups[root], key) {
				groups[root] = append(groups[root], key)
			}
		}
	}
	for _, keys := range groups {
		sort.Strings(keys)
	}
	aliases := map[propertyLocation][]string{}
	for i, n := range nodes {
		if !n.derived {
			aliases[n.location] = groups[find(i)]
		}
	}
	return aliases, nil
}
