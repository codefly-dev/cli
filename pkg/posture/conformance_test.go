package posture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// workload wraps a pod specification body in a StatefulSet, so a case states only
// the thing under test.
func workload(body string) string {
	return "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: store\nspec:\n  template:\n    spec:\n" + body
}

// custody is the credential a store holds as its own state: what tells a store of
// credentials apart from a cache, which loses nothing it cannot recompute.
const custody = `          env:
            - name: STORE_ROOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_ROOT_TOKEN
`

// documents decodes a multi-document manifest body into an effective set.
func documents(t *testing.T, body string) []Document {
	t.Helper()
	var decoded []Document
	for index, value := range decodeAll(t, body) {
		decoded = append(decoded, Document{Location: "services/store/base/workload.yaml", Value: value})
		_ = index
	}
	return decoded
}

func decodeAll(t *testing.T, body string) []map[string]any {
	t.Helper()
	values, err := decodeDocuments([]byte(body))
	require.NoError(t, err)
	return values
}

// Each case is one spelling of a thing a rule is about. A rule that recognised
// only the spelling its author thought of would be satisfied by any of the others,
// so every equivalent form is pinned here — and so is every correct configuration
// that must not need an allowance.
func TestRuleConformanceMatrix(t *testing.T) {
	subject := Subject{Module: "shop", Service: "store"}
	cases := []struct {
		name     string
		manifest string
		mesh     bool
		rule     string // "" means the render must accept it
		// detail, when set, is what the refusal must say — so that a rule reaching
		// the right verdict for the wrong reason is still a failure.
		detail string
	}{
		{
			name: "an https listener with its own certificate and key",
			manifest: workload(`      containers:
        - name: store
          args:
            - --listen=https://0.0.0.0:8443
            - --cert=/keys/listener.crt
            - --key=/keys/listener.key
`),
			mesh: true, rule: RulePeerTransportMaterial,
		},
		{
			name: "an https listener with no certificate option of its own",
			manifest: workload(`      containers:
        - name: store
          args: ["--listen=https://0.0.0.0:8443"]
`),
			mesh: true, rule: RulePeerTransportMaterial,
		},
		{
			name: "a plain http listener is not transport the workload terminates",
			manifest: workload(`      containers:
        - name: store
          args: ["--listen=http://0.0.0.0:8080"]
`),
			mesh: true,
		},
		{
			name: "a certificate path under a name that says only path",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: CERTIFICATE_PATH
              value: /keys/listener.crt
`),
			mesh: true, rule: RulePeerTransportMaterial,
		},
		{
			name: "a transport switch turned on",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: MUTUAL_TLS
              value: "true"
`),
			mesh: true, rule: RulePeerTransportMaterial,
		},
		{
			name: "the same transport switch turned off is not a violation",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: MUTUAL_TLS
              value: "false"
`),
			mesh: true,
		},
		{
			name: "an application credential delivered as a secret reference is not transport material",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: APP_CLIENT_KEY
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: APP_CLIENT_KEY
            - name: API_KEY_FILE
              value: /run/api/key
            - name: SESSION_MAX_ACTIVE_DEVICES
              value: "4"
`),
			mesh: true,
		},
		{
			name: "a comment naming a retired option is not configuration",
			manifest: workload(`      containers:
        - name: store
          command: ["/bin/sh", "-c"]
          args:
            - |
              # TLS_CERT_FILE was retired when the mesh took over transport.
              exec store server --listen=http://0.0.0.0:8080
`),
			mesh: true,
		},
		{
			name:     "an in-memory storage mode chosen by option value",
			manifest: workload("      containers:\n        - name: store\n          args: [\"--storage=memory\"]\n" + custody),
			rule:     RuleInMemoryStateStore,
		},
		{
			name:     "an in-memory storage mode chosen by a separate option token",
			manifest: workload("      containers:\n        - name: store\n          args: [\"--storage\", \"inmem\"]\n" + custody),
			rule:     RuleInMemoryStateStore,
		},
		{
			name:     "an in-memory switch under another spelling",
			manifest: workload("      containers:\n        - name: store\n          env:\n            - name: STORE_IN_MEMORY\n              value: \"true\"\n            - name: STORE_ROOT_TOKEN\n              value: seeded\n"),
			rule:     RuleInMemoryStateStore,
		},
		{
			name:     "the same in-memory switch turned off is not a violation",
			manifest: workload("      containers:\n        - name: store\n          env:\n            - name: STORE_INMEM\n              value: \"false\"\n            - name: STORE_ROOT_TOKEN\n              value: seeded\n"),
		},
		{
			name:     "a development switch turned off is not a violation",
			manifest: workload("      containers:\n        - name: store\n          args: [\"--dev=false\"]\n" + custody),
		},
		{
			name: "an ordinary cache keeping its data in memory is not a credential store",
			manifest: workload(`      containers:
        - name: cache
          env:
            - name: CACHE_BACKEND
              value: memory
            - name: CACHE_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: secret-cache
                  key: CACHE_PASSWORD
`),
		},
		{
			name: "a development server started inside a shell program",
			manifest: workload(`      containers:
        - name: store
          command: ["/bin/sh", "-c"]
          args: ["exec store server -dev"]
` + custody),
			rule: RuleInMemoryStateStore,
			// The option inside the program is what is named: the render read the
			// program rather than merely refusing to read it.
			detail: "-dev starts its development server",
		},
		{
			// No readable option says which mode this program selects, and the
			// workload holds credential material: the render refuses rather than
			// assume the program conforms.
			name: "a credential store started by a program the render cannot read",
			manifest: workload(`      containers:
        - name: store
          command: ["/bin/sh", "-c"]
          args: ["set -eu\nexec store server --config=/tmp/store.conf"]
` + custody),
			rule:   RuleInMemoryStateStore,
			detail: "cannot read as configuration",
		},
		{
			// A switch whose value lives in a Secret cannot be read, and absence
			// of a readable value is not evidence that it is off.
			name: "an in-memory switch whose value the render cannot read",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: STORE_INMEM
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_INMEM
            - name: STORE_ROOT_TOKEN
              value: seeded
`),
			rule:   RuleInMemoryStateStore,
			detail: "a value this render cannot read",
		},
		{
			name: "a transport switch whose value the render cannot read",
			manifest: workload(`      containers:
        - name: store
          env:
            - name: MUTUAL_TLS
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: MUTUAL_TLS
`),
			mesh: true, rule: RulePeerTransportMaterial,
			detail: "a value this render cannot read",
		},
		{
			name: "a configuration claim mounted where configuration is read",
			manifest: workload(`      containers:
        - name: store
          volumeMounts:
            - name: config
              mountPath: /etc/service
              readOnly: true
      volumes:
        - name: config
          persistentVolumeClaim:
            claimName: store-config
`),
			rule: RuleNonScratchMount,
		},
		{
			name: "a scratch volume mounted where credentials are read",
			manifest: workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: extra
              mountPath: /secrets
      volumes:
        - name: tmp
          emptyDir: {}
        - name: extra
          emptyDir:
            medium: Memory
`),
			rule: RuleNonScratchMount,
		},
		{
			name: "scratch and a durable data claim at a data path are what a stateful workload needs",
			manifest: workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: data
              mountPath: /var/lib/store
      volumes:
        - name: tmp
          emptyDir: {}
        - name: data
          persistentVolumeClaim:
            claimName: store-data
`),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			declaration := &Declaration{}
			if test.mesh {
				declaration.Asserts = map[string]bool{AssertMeshProtectedTransport: true}
			}
			err := ValidateDocuments(documents(t, test.manifest), subject, declaration)
			if test.rule == "" {
				require.NoError(t, err, "a conforming render must not need an allowance")
				return
			}
			require.Error(t, err)
			var violation *Violation
			require.ErrorAs(t, err, &violation)
			require.Equal(t, test.rule, violation.Rule)
			require.NotEmpty(t, violation.Field)
			require.Contains(t, err.Error(), "deployed render refuses service shop/store")
			if test.detail != "" {
				require.Contains(t, violation.Detail, test.detail)
			}
		})
	}
}

// A value a workload reads from elsewhere in the same manifest set is part of its
// configuration. A rule that read only literals would be satisfied by moving the
// value one indirection away.
func TestReferencedValuesAreConfiguration(t *testing.T) {
	subject := Subject{Module: "shop", Service: "store"}
	configMap := `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-settings
data:
  STORAGE_BACKEND: memory
  TLS_CERT_FILE: /keys/listener.crt
---
`
	t.Run("through envFrom", func(t *testing.T) {
		manifest := configMap + workload(`      containers:
        - name: store
          envFrom:
            - configMapRef:
                name: store-settings
`+custody)
		err := ValidateDocuments(documents(t, manifest), subject, &Declaration{})
		require.ErrorContains(t, err, RuleInMemoryStateStore)
		require.ErrorContains(t, err, "STORAGE_BACKEND=memory")
	})
	t.Run("through a single key reference", func(t *testing.T) {
		manifest := configMap + workload(`      containers:
        - name: store
          env:
            - name: STORAGE_BACKEND
              valueFrom:
                configMapKeyRef:
                  name: store-settings
                  key: STORAGE_BACKEND
            - name: STORE_ROOT_TOKEN
              value: seeded
`)
		err := ValidateDocuments(documents(t, manifest), subject, &Declaration{})
		require.ErrorContains(t, err, RuleInMemoryStateStore)
	})
	t.Run("a mesh-protected environment sees a referenced certificate", func(t *testing.T) {
		manifest := configMap + workload(`      containers:
        - name: store
          envFrom:
            - configMapRef:
                name: store-settings
`)
		err := ValidateDocuments(documents(t, manifest), subject,
			&Declaration{Asserts: map[string]bool{AssertMeshProtectedTransport: true}})
		require.ErrorContains(t, err, RulePeerTransportMaterial)
		require.ErrorContains(t, err, "TLS_CERT_FILE=/keys/listener.crt")
	})
	// A value the render cannot read is not evidence of conformance: a storage
	// mode delivered through a Secret fails closed, naming the reference.
	t.Run("an unreadable storage mode fails closed", func(t *testing.T) {
		manifest := workload(`      containers:
        - name: store
          env:
            - name: STORAGE_BACKEND
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORAGE_BACKEND
` + `            - name: STORE_ROOT_TOKEN
              value: seeded
`)
		err := ValidateDocuments(documents(t, manifest), subject, &Declaration{})
		require.ErrorContains(t, err, RuleInMemoryStateStore)
		require.ErrorContains(t, err, `secret "secret-store" key "STORAGE_BACKEND"`)
	})
}

// A workload shipped inside a wrapper list is applied exactly as a top-level one,
// so it is held to the same rules.
func TestWorkloadsInsideAListAreInspected(t *testing.T) {
	body := `apiVersion: v1
kind: List
items:
  - ` + indent(workload("      containers:\n        - name: store\n          args: [\"server\", \"-dev\"]\n"+custody), 4)
	err := ValidateDocuments(documents(t, body), Subject{Module: "shop", Service: "store"}, &Declaration{})
	require.ErrorContains(t, err, RuleInMemoryStateStore)
	require.ErrorContains(t, err, "items[0]")
}

func indent(body string, by int) string {
	prefix := ""
	for i := 0; i < by; i++ {
		prefix += " "
	}
	lines := []byte{}
	for index, line := range splitLines(body) {
		if index > 0 {
			lines = append(lines, []byte(prefix)...)
		}
		lines = append(lines, []byte(line+"\n")...)
	}
	return string(lines)
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

// The posture reads the manifests a cell would apply, so a patch in any form —
// including one that REMOVES what a base declared — decides the outcome.
func TestEffectiveTreeIsWhatTheCellWouldApply(t *testing.T) {
	subject := Subject{Module: "shop", Service: "store"}
	build := func(t *testing.T, files map[string]string) []Document {
		t.Helper()
		root := t.TempDir()
		for path, content := range files {
			full := filepath.Join(root, filepath.FromSlash(path))
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		}
		documents, err := EffectiveTree(root, "staging")
		require.NoError(t, err)
		return documents
	}
	conforming := workload(`      containers:
        - name: store
          image: registry.example.com/acme/store@sha256:aaaa
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`)

	t.Run("an inline JSON patch that adds a mount is refused", func(t *testing.T) {
		documents := build(t, map[string]string{
			"services/store/base/workload.yaml":      conforming,
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
            name: store-settings
`,
		})
		require.ErrorContains(t,
			ValidateDocuments(documents, subject, &Declaration{}), RuleNonScratchMount)
	})

	t.Run("an overlay that removes a base mount is accepted", func(t *testing.T) {
		documents := build(t, map[string]string{
			"services/store/base/workload.yaml": workload(`      containers:
        - name: store
          image: registry.example.com/acme/store@sha256:aaaa
      volumes:
        - name: config
          configMap:
            name: store-settings
`),
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
		require.NoError(t, ValidateDocuments(documents, subject, &Declaration{}),
			"the cell applies no mount, so there is nothing to refuse")
	})

	t.Run("another environment's overlay is not this environment's manifests", func(t *testing.T) {
		documents := build(t, map[string]string{
			"services/store/base/workload.yaml":                  conforming,
			"services/store/base/kustomization.yaml":             "resources:\n  - workload.yaml\n",
			"services/store/overlays/staging/kustomization.yaml": "resources:\n  - ../../base\n",
			"services/store/overlays/local/kustomization.yaml":   "resources:\n  - ../../base\n  - development.yaml\n",
			"services/store/overlays/local/development.yaml": workload(`      containers:
        - name: store
          args: ["server", "-dev"]
` + custody),
		})
		require.NoError(t, ValidateDocuments(documents, subject, &Declaration{}),
			"a development overlay this environment never applies is not part of its render")
	})
}

func TestDecodeDocumentsRejectsBrokenYAML(t *testing.T) {
	_, err := decodeDocuments([]byte("kind: Deployment\n\tbad: indent\n"))
	require.Error(t, err)
	var value map[string]any
	require.Error(t, yaml.Unmarshal([]byte("\tbad"), &value))
}
