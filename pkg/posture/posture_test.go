package posture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func document(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(text), &value))
	return value
}

// meshed is an environment that asserts the platform protects transport between
// its workloads.
func meshed(allowances ...Allowance) *Declaration {
	return &Declaration{
		Asserts:    map[string]bool{AssertMeshProtectedTransport: true},
		Allowances: allowances,
	}
}

const scratchWorkload = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          image: registry.example.com/acme/api@sha256:aaaa
          env:
            - name: ACME__ENDPOINT__SHOP__STORE__TCP
              value: store.acme.svc.cluster.local:5432
            - name: SESSION_MAX_ACTIVE_DEVICES
              value: "4"
            - name: API_KEY_FILE
              value: /run/api/key
      volumes:
        - name: tmp
          emptyDir: {}
`

const durableWorkload = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          image: registry.example.com/acme/store@sha256:bbbb
      volumes:
        - name: tmp
          emptyDir: {}
        - name: data
          persistentVolumeClaim:
            claimName: store-data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
`

func TestConformingWorkloadsPassEveryRule(t *testing.T) {
	subject := Subject{Module: "shop", Service: "api"}
	require.NoError(t, Validate(document(t, scratchWorkload), subject, "services/api/base/deployment.yaml", meshed()))
	require.NoError(t, Validate(document(t, durableWorkload), subject, "services/store/base/stateful-set.yaml", meshed()))
}

func TestRuleRefusals(t *testing.T) {
	subject := Subject{Module: "shop", Service: "store"}
	cases := []struct {
		name        string
		manifest    string
		declaration *Declaration
		rule        string
		field       string
		detail      string
	}{
		{
			name: "a listener configured with its own certificate on a mesh-protected environment",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          env:
            - name: STORE_TLS_CERT_FILE
              value: /etc/store/server.crt
`,
			declaration: meshed(),
			rule:        RulePeerTransportMaterial,
			field:       "spec.template.spec.containers[0].env[0]",
			detail:      "STORE_TLS_CERT_FILE=/etc/store/server.crt configures TLS material",
		},
		{
			name: "a client handed CA material on the command line",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          args: ["serve", "--ca-cert=/etc/peers/ca.crt"]
`,
			declaration: meshed(),
			rule:        RulePeerTransportMaterial,
			field:       "spec.template.spec.containers[0].args[1]",
			detail:      "--ca-cert=/etc/peers/ca.crt configures TLS material",
		},
		{
			name: "a volume delivering TLS material as files",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
      volumes:
        - name: peer-tls
          secret:
            secretName: store-peer-tls
`,
			declaration: meshed(),
			rule:        RulePeerTransportMaterial,
			field:       "spec.template.spec.volumes[0].secret",
			detail:      `volume "peer-tls" delivers tls as files`,
		},
		{
			name: "a ConfigMap volume carrying configuration",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
      volumes:
        - name: tmp
          emptyDir: {}
        - name: config
          configMap:
            name: store-config
`,
			rule:   RuleNonScratchMount,
			field:  "spec.template.spec.volumes[1].configMap",
			detail: `volume "config" takes its contents from a configMap source`,
		},
		{
			name: "a projected volume",
			manifest: `apiVersion: batch/v1
kind: Job
metadata:
  name: store-bootstrap
spec:
  template:
    spec:
      containers:
        - name: bootstrap
      volumes:
        - name: identity
          projected:
            sources: []
`,
			rule:   RuleNonScratchMount,
			field:  "spec.template.spec.volumes[0].projected",
			detail: `volume "identity" takes its contents from a projected source`,
		},
		{
			name: "a host path",
			manifest: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
      volumes:
        - name: host
          hostPath:
            path: /var/lib/store
`,
			rule:   RuleNonScratchMount,
			field:  "spec.template.spec.volumes[0].hostPath",
			detail: `volume "host" takes its contents from a hostPath source`,
		},
		{
			name: "a development-mode switch on the command line",
			manifest: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          command: ["store", "server", "-dev"]
          env:
            - name: STORE_ROOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_ROOT_TOKEN
`,
			rule:   RuleInMemoryStateStore,
			field:  "spec.template.spec.containers[0].command[2]",
			detail: "-dev starts its development server",
		},
		{
			name: "a development-mode root credential",
			manifest: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          env:
            - name: STORE_DEV_ROOT_TOKEN_ID
              value: "seeded"
`,
			rule:   RuleInMemoryStateStore,
			field:  "spec.template.spec.containers[0].env[0]",
			detail: "STORE_DEV_ROOT_TOKEN_ID=seeded starts its development server",
		},
		{
			name: "in-memory storage selected by value",
			manifest: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          env:
            - name: STORE_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_MASTER_KEY
            - name: STORE_STORAGE_BACKEND
              value: inmem
`,
			rule:   RuleInMemoryStateStore,
			field:  "spec.template.spec.containers[0].env[1]",
			detail: "STORE_STORAGE_BACKEND=inmem keeps its state in memory",
		},
		{
			name: "a CronJob's pod template is a workload too",
			manifest: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: store-compact
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: compact
          volumes:
            - name: config
              configMap:
                name: store-config
`,
			rule:   RuleNonScratchMount,
			field:  "spec.jobTemplate.spec.template.spec.volumes[0].configMap",
			detail: `volume "config" takes its contents from a configMap source`,
		},
		{
			name: "an init container is held to the same rules",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      initContainers:
        - name: migrate
          args: ["--dev-mode=true"]
          env:
            - name: STORE_ROOT_TOKEN
              value: seeded
      containers:
        - name: store
`,
			rule:   RuleInMemoryStateStore,
			field:  "spec.template.spec.initContainers[0].args[0]",
			detail: "--dev-mode=true starts its development server",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(document(t, test.manifest), subject, "services/store/base/workload.yaml", test.declaration)
			require.Error(t, err)
			var violation *Violation
			require.ErrorAs(t, err, &violation)
			require.Equal(t, test.rule, violation.Rule)
			require.Equal(t, test.field, violation.Field)
			message := err.Error()
			require.Contains(t, message, "deployed render refuses service shop/store")
			require.Contains(t, message, test.rule)
			require.Contains(t, message, test.field)
			require.Contains(t, message, test.detail)
			require.Contains(t, message, "services/store/base/workload.yaml")
			require.Contains(t, message, "posture.allowances")
			require.Contains(t, message, `docs/commands.md ("The deployed security posture")`)
		})
	}
}

// Transport material is only refused where the environment states that the
// platform already protects transport between its workloads. Without that
// assertion the service's own TLS may be the only protection there is.
func TestTransportMaterialIsOnlyRefusedOnAMeshProtectedEnvironment(t *testing.T) {
	manifest := document(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          env:
            - name: STORE_TLS_CERT_FILE
              value: /etc/store/server.crt
`)
	subject := Subject{Module: "shop", Service: "store"}
	require.NoError(t, Validate(manifest, subject, "workload.yaml", nil))
	require.NoError(t, Validate(manifest, subject, "workload.yaml", &Declaration{}))
	require.Error(t, Validate(manifest, subject, "workload.yaml", meshed()))
}

// A credential a service authenticates with is not its transport. Each of these
// is a real name a render carries today, and none of them is TLS material.
func TestCredentialNamesAreNotTransportMaterial(t *testing.T) {
	for _, name := range []string{
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__APP_INTEGRATION__APP_PRIVATE_KEY",
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__SHOP__STORE__POSTGRES__PASSWORD",
		"API_KEY_FILE",
		"SIGNING_KEY",
		"SESSION_MAX_ACTIVE_DEVICES",
	} {
		manifest := document(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          env:
            - name: `+name+`
              valueFrom:
                secretKeyRef:
                  name: secret-api
                  key: `+name+`
`)
		require.NoError(t,
			Validate(manifest, Subject{Module: "shop", Service: "api"}, "workload.yaml", meshed()),
			name)
	}
}

func TestAnAllowanceExcusesExactlyItsRuleAndService(t *testing.T) {
	manifest := document(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: web
      volumes:
        - name: config
          configMap:
            name: web-config
`)
	subject := Subject{Module: "shop", Service: "web"}
	bare := &Declaration{Allowances: []Allowance{{
		Rule: RuleNonScratchMount, Service: "web", Reason: "the asset bundle is built into the image",
	}}}
	qualified := &Declaration{Allowances: []Allowance{{
		Rule: RuleNonScratchMount, Service: "shop/web", Reason: "the asset bundle is built into the image",
	}}}
	require.NoError(t, Validate(manifest, subject, "workload.yaml", bare))
	require.NoError(t, Validate(manifest, subject, "workload.yaml", qualified))

	otherService := &Declaration{Allowances: []Allowance{{
		Rule: RuleNonScratchMount, Service: "shop/api", Reason: "unrelated",
	}}}
	otherRule := &Declaration{Allowances: []Allowance{{
		Rule: RuleInMemoryStateStore, Service: "shop/web", Reason: "unrelated",
	}}}
	require.Error(t, Validate(manifest, subject, "workload.yaml", otherService))
	require.Error(t, Validate(manifest, subject, "workload.yaml", otherRule))
}

func TestDeclarationValidationRefusesWhatARenderCouldNotActOn(t *testing.T) {
	require.NoError(t, (*Declaration)(nil).Validate())
	require.NoError(t, (&Declaration{
		Asserts:    map[string]bool{AssertMeshProtectedTransport: true},
		Allowances: []Allowance{{Rule: RuleNonScratchMount, Service: "shop/web", Reason: "reviewed by the platform owner"}},
	}).Validate())

	require.ErrorContains(t,
		(&Declaration{Asserts: map[string]bool{"internal-transport/mesh": true}}).Validate(),
		`posture asserts unknown fact "internal-transport/mesh"`)
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: "no-mounts", Service: "api", Reason: "why"}}}).Validate(),
		`posture allowance 1 names unknown rule "no-mounts"`)
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: RuleNonScratchMount, Reason: "why"}}}).Validate(),
		"must name a service")
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: RuleNonScratchMount, Service: "api"}}}).Validate(),
		"must state a reason")
}

// Every declared allowance is reported, exercised or not: a render that stopped
// needing one must still say the exception stands.
func TestReportNamesEveryDeclaredAllowance(t *testing.T) {
	declaration := &Declaration{Allowances: []Allowance{
		{Rule: RuleNonScratchMount, Service: "shop/web", Reason: "the theme is a build artifact"},
		{Rule: RulePeerTransportMaterial, Service: "shop/api", Reason: "an external peer pins its own CA"},
	}}
	require.Equal(t, []string{
		"security posture: service shop/web is allowed to break rule non-scratch-mount — the theme is a build artifact",
		"security posture: service shop/api is allowed to break rule peer-transport-material — an external peer pins its own CA",
	}, declaration.Report())
	require.Nil(t, (*Declaration)(nil).Report())
	require.Nil(t, (&Declaration{}).Report())
}

func TestSubjectFromPathNamesTheUnitThatRenderedTheManifest(t *testing.T) {
	defaults := Subject{Module: "shop", Service: "api"}
	cases := map[string]Subject{
		"services/store/base/stateful-set.yaml":                {Module: "shop", Service: "store"},
		"solutions/checkout/overlays/staging/deployment.yaml":  {Module: "shop", Service: "checkout"},
		"kustomize:services/store/overlays/staging#2":          {Module: "shop", Service: "store"},
		"modules/billing/services/ledger/base/deployment.yaml": {Module: "billing", Service: "ledger"},
		"base/deployment.yaml":                                 defaults,
		"module/workspace.codefly.yaml":                        defaults,
	}
	for path, expected := range cases {
		require.Equal(t, expected, SubjectFromPath(path, defaults), path)
	}
}

// A tree on disk is read through the effective-manifest pipeline, and a refusal
// names the service the manifest belongs to and where it came from.
func TestEffectiveTreeRefusalNamesTheServiceAndItsManifest(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	write("modules/shop/services/api/overlays/staging/kustomization.yaml", "resources:\n  - workload.yaml\n")
	write("modules/shop/services/api/overlays/staging/workload.yaml", scratchWorkload)
	documents, err := EffectiveTree(root, "staging")
	require.NoError(t, err)
	require.NoError(t, ValidateDocuments(documents, Subject{Module: "shop", Service: "api"}, meshed()))

	write("modules/shop/services/store/overlays/staging/kustomization.yaml", "resources:\n  - workload.yaml\n")
	write("modules/shop/services/store/overlays/staging/workload.yaml", `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
spec:
  template:
    spec:
      containers:
        - name: store
          command: ["store", "server", "-dev"]
          env:
            - name: STORE_ROOT_TOKEN
              value: seeded
`)
	documents, err = EffectiveTree(root, "staging")
	require.NoError(t, err)
	err = ValidateDocuments(documents, Subject{Module: "shop", Service: "api"}, meshed())
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, "modules/shop/services/store/overlays/staging")
	require.ErrorContains(t, err, RuleInMemoryStateStore)
}
