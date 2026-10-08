package deploysecrets

import (
	"slices"
	"sort"
)

// propertyLocation identifies one property of one remote key in one backend. The
// backend is part of it because the alias index spans every backend a run read
// and two backends may hold a remote key of the same NAME: without it, two
// unrelated properties would share an index entry and one of them would be
// resolved against the other's alias group.
type propertyLocation struct{ backend, remote, property string }

// configurationAliases finds connected properties, not just equal key spellings
// on one pair. A property reading A+B makes all other properties reading either
// key carry one value, so every property is mapped to the sorted keys of the
// alias group it belongs to.
func configurationAliases(states []*remoteState) map[propertyLocation][]string {
	type node struct {
		location propertyLocation
		keys     []string
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
			i := len(nodes)
			nodes = append(nodes, node{propertyLocation{state.backend, state.secret.RemoteKey, property.Property}, property.Keys})
			parent = append(parent, i)
			for _, key := range property.Keys {
				if !configurationKey(key) {
					continue
				}
				if previous, ok := byKey[key]; ok {
					parent[find(i)] = find(previous)
				} else {
					byKey[key] = i
				}
			}
		}
	}
	groups := map[int][]string{}
	for i, n := range nodes {
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
		aliases[n.location] = groups[find(i)]
	}
	return aliases
}
