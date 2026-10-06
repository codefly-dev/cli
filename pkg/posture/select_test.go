package posture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// forbidden is a workload carrying a volume that is not the standard scratch one.
const forbidden = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: store
      volumes:
        - name: config
          configMap:
            name: store-config
`

// permitted is the conforming workload, in a file of its own.
const permitted = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`

func selected(t *testing.T, root string) []Document {
	t.Helper()
	documents, err := SelectManifests(root, "staging")
	require.NoError(t, err)
	return documents
}

// declaredUnits is the contract of every unit these trees carry: each declares its
// storage, its transport and the scratch volume the platform renders for it, so a
// case is about selection rather than about a missing declaration.
func declaredUnits(services ...string) Contracts {
	contracts := Contracts{}
	for _, service := range services {
		contracts.Add(&ServiceContract{
			Module: "shop", Service: service,
			StorageMode: StorageModeDurable, Transport: TransportMesh,
			ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
		})
	}
	return contracts
}

func refuses(t *testing.T, root string) error {
	t.Helper()
	return ValidateDocuments(selected(t, root),
		Subject{Module: "shop", Service: "store"}, declaredUnits("store", "api"), meshed())
}

// The selected set is what the cell would apply: patches decide, in whatever form
// they take, and a patch that removes something is honoured.
func TestSelectionIsWhatTheCellWouldApply(t *testing.T) {
	t.Run("a JSON patch that adds a volume is seen", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/store/base/workload.yaml":      permitted,
			"services/store/base/kustomization.yaml": "resources:\n  - workload.yaml\n",
			"services/store/overlays/staging/kustomization.yaml": `resources:
  - ../../base
patches:
  - target:
      kind: StatefulSet
      name: store
    patch: |-
      - op: add
        path: /spec/template/spec/volumes/-
        value:
          name: config
          configMap:
            name: store-config
`,
		})
		require.ErrorContains(t, refuses(t, root), RuleNonScratchMount)
	})

	t.Run("a patch that removes a forbidden volume is honoured", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/store/base/workload.yaml":      forbidden,
			"services/store/base/kustomization.yaml": "resources:\n  - workload.yaml\n",
			"services/store/overlays/staging/kustomization.yaml": `resources:
  - ../../base
patches:
  - target:
      kind: StatefulSet
      name: store
    patch: |-
      - op: remove
        path: /spec/template/spec/volumes
`,
		})
		require.NoError(t, refuses(t, root), "the cell applies no volume, so there is nothing to refuse")
	})

	// A root kustomization and the base it includes are one build, not two: building
	// the base as well would judge the tree against manifests the patch replaced.
	t.Run("a base is not built a second time", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"base/workload.yaml":      forbidden,
			"base/kustomization.yaml": "resources:\n  - workload.yaml\n",
			"kustomization.yaml": `resources:
  - base
patches:
  - target:
      kind: StatefulSet
      name: store
    patch: |-
      - op: remove
        path: /spec/template/spec/volumes
`,
		})
		require.NoError(t, refuses(t, root))
	})

	t.Run("another environment's overlay is not read", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/store/base/workload.yaml":                  permitted,
			"services/store/base/kustomization.yaml":             "resources:\n  - workload.yaml\n",
			"services/store/overlays/staging/kustomization.yaml": "resources:\n  - ../../base\n",
			"services/store/overlays/local/kustomization.yaml":   "resources:\n  - ../../base\n  - development.yaml\n",
			"services/store/overlays/local/development.yaml":     forbidden,
		})
		require.NoError(t, refuses(t, root))
	})

	// Selecting one unit's overlay must not blind the selector to the rest of the
	// tree: another unit's own kustomization, and manifests no kustomization
	// covers, are part of what the cell applies.
	t.Run("a sibling unit with no overlay for this environment is still read", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/api/base/workload.yaml":                  permitted,
			"services/api/base/kustomization.yaml":             "resources:\n  - workload.yaml\n",
			"services/api/overlays/staging/kustomization.yaml": "resources:\n  - ../../base\n",
			"services/store/kustomization.yaml":                "resources:\n  - workload.yaml\n",
			"services/store/workload.yaml":                     forbidden,
		})
		require.ErrorContains(t, refuses(t, root), RuleNonScratchMount)
	})

	t.Run("a manifest no kustomization covers is still read", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/api/base/workload.yaml":                  permitted,
			"services/api/base/kustomization.yaml":             "resources:\n  - workload.yaml\n",
			"services/api/overlays/staging/kustomization.yaml": "resources:\n  - ../../base\n",
			"services/store/plain.yaml":                        forbidden,
		})
		require.ErrorContains(t, refuses(t, root), RuleNonScratchMount)
	})
}

// Every list representation hides a workload the same way, so every one is
// unwrapped — typed lists, and lists of lists.
func TestListsAreUnwrappedRecursively(t *testing.T) {
	cases := map[string]string{
		"a plain List": `apiVersion: v1
kind: List
items:
  - ` + indented(forbidden),
		"a typed DeploymentList": `apiVersion: apps/v1
kind: DeploymentList
items:
  - ` + indented(forbidden),
		"a List inside a List": `apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: List
    items:
      - ` + indentedBy(forbidden, 8),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"services/store/wrapped.yaml": body})
			require.ErrorContains(t, refuses(t, root), RuleNonScratchMount)
		})
	}
}

func indented(body string) string { return indentedBy(body, 4) }

// indentedBy re-indents a manifest so it can be nested as a list item; the first
// line keeps the "- " the caller wrote.
func indentedBy(body string, by int) string {
	prefix := ""
	for index := 0; index < by; index++ {
		prefix += " "
	}
	out := ""
	for index, line := range splitLines(body) {
		if index == 0 {
			out += line + "\n"
			continue
		}
		if line == "" {
			out += "\n"
			continue
		}
		out += prefix + line + "\n"
	}
	return out
}

func splitLines(body string) []string {
	var lines []string
	current := ""
	for _, symbol := range body {
		if symbol == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(symbol)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
