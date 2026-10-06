package posture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func meshed(allowances ...Allowance) *Declaration {
	return &Declaration{
		Asserts:    map[string]bool{AssertMeshProtectedTransport: true},
		Allowances: allowances,
	}
}

var storeSubject = Subject{Module: "shop", Service: "store"}

// documents decodes a manifest body as one selected set.
func documents(t *testing.T, body string) []Document {
	t.Helper()
	values, err := decodeDocuments([]byte(body))
	require.NoError(t, err)
	decoded := make([]Document, 0, len(values))
	for _, value := range values {
		decoded = append(decoded, Document{Location: "services/store/overlays/staging (Kustomize output)", Value: value})
	}
	return expand(decoded)
}

// workload wraps a pod specification body in a StatefulSet.
func workload(body string) string {
	return "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: store\n  namespace: acme\nspec:\n  template:\n    spec:\n" + body
}

// conformingWorkload is what the platform's own projection produces: the standard
// scratch volume, mounted at the standard scratch path, and nothing else.
const conformingWorkload = `      containers:
        - name: store
          image: registry.example.com/acme/store@sha256:aaaa
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`

// durable is the contract of a service that declared what the platform renders for
// it: one scratch volume at one path, and durable storage.
func durable() Contracts {
	contracts := Contracts{}
	contracts.Add(&ServiceContract{
		Module: "shop", Service: "store", StorageMode: StorageModeDurable, Transport: TransportMesh,
		ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
	})
	return contracts
}

// scratchOnly declared its scratch volume and its transport but NO storage mode,
// which a deployed render refuses on its own.
func scratchOnly() Contracts {
	contracts := Contracts{}
	contracts.Add(&ServiceContract{
		Module: "shop", Service: "store", Transport: TransportMesh,
		ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
	})
	return contracts
}

// declaresNothing is the contract of a service that has declared nothing at all.
func declaresNothing() Contracts {
	contracts := Contracts{}
	contracts.Add(&ServiceContract{Module: "shop", Service: "store", Transport: TransportMesh})
	return contracts
}

func TestAWorkloadThatCarriesWhatItDeclaredNeedsNoAllowance(t *testing.T) {
	require.NoError(t, ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, durable(), meshed()))
}

// The same workload, from a service that declared nothing, is refused: a deployed
// workload carries only what its service declares.
func TestAVolumeNobodyDeclaredIsRefused(t *testing.T) {
	// Declares its storage and its transport, but no scratch volume.
	noScratch := Contracts{}
	noScratch.Add(&ServiceContract{
		Module: "shop", Service: "store", StorageMode: StorageModeDurable, Transport: TransportMesh,
	})
	err := ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, noScratch, meshed())
	require.ErrorContains(t, err, RuleNonScratchMount)
	require.ErrorContains(t, err, `volume "tmp" (emptyDir) is not declared by service shop/store`)
	require.ErrorContains(t, err, "spec.deployment.scratch-volumes (declared: none)")

	// And a declaration cannot launder a source that is not scratch at all.
	laundered := workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          configMap:
            name: store-config
`)
	require.ErrorContains(t,
		ValidateDocuments(documents(t, laundered), storeSubject, durable(), meshed()),
		"is declared as scratch by service shop/store but is rendered from a configMap source")
}

// Rule 2 is an identity, not a list of sources or destinations: the standard
// scratch volume, mounted where it belongs, and nothing else. Every case here is
// one way of carrying something else, including the ones that only changed the
// destination.
func TestOnlyTheStandardScratchVolumeIsCarried(t *testing.T) {
	cases := map[string]string{
		"a ConfigMap volume": `      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
        - name: config
          configMap:
            name: store-config
`,
		"a Secret volume": `      containers:
        - name: store
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`,
		"a projected volume": `      containers:
        - name: store
      volumes:
        - name: identity
          projected:
            sources: []
`,
		"a host path": `      containers:
        - name: store
      volumes:
        - name: host
          hostPath:
            path: /var/lib/store
`,
		"a claim from a service that has not declared durable storage": `      containers:
        - name: store
          args: ["--config=/app/config/settings.yaml"]
          volumeMounts:
            - name: config
              mountPath: /app/config
              readOnly: true
      volumes:
        - name: config
          persistentVolumeClaim:
            claimName: store-config
`,
		"an undeclared second scratch volume": `      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: extra
              mountPath: /app/secrets
      volumes:
        - name: tmp
          emptyDir: {}
        - name: extra
          emptyDir:
            medium: Memory
`,
		"a declared volume mounted somewhere else as well": `      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: tmp
              mountPath: /app/cache
      volumes:
        - name: tmp
          emptyDir: {}
`,
		"a durable claim declared but never mounted": `      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
        - name: data
          persistentVolumeClaim:
            claimName: store-data
`,
		"a scratch volume under a name nobody declared": `      containers:
        - name: store
          volumeMounts:
            - name: cache
              mountPath: /tmp
      volumes:
        - name: cache
          emptyDir: {}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			contracts := durable()
			expected := RuleNonScratchMount
			if strings.Contains(name, "claim") {
				// A claim is the workload's own state, admitted only for a service
				// that declared durable storage — and a service that declared no
				// storage mode is refused for that before anything else.
				contracts = scratchOnly()
				expected = RuleInMemoryStateStore
			}
			err := ValidateDocuments(documents(t, workload(body)), storeSubject, contracts, meshed())
			require.Error(t, err)
			var violation *Violation
			require.ErrorAs(t, err, &violation)
			require.Equal(t, expected, violation.Rule)
			require.NotEmpty(t, violation.Field)
			require.Contains(t, err.Error(), "deployed render refuses service shop/store")
		})
	}
}

// Rule 1 reads what the render delivers and what the service declared. A
// container's environment and command line are not read at all, which is why an
// external HTTPS destination is not mistaken for a listener of its own.
func TestCertificateMaterialIsRefusedByWhatTheRenderDelivers(t *testing.T) {
	refused := map[string]string{
		"a Secret of a certificate type": `apiVersion: v1
kind: Secret
metadata:
  name: store-peer
  namespace: acme
type: kubernetes.io/tls
`,
		"a ConfigMap carrying certificate material": `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-trust
  namespace: acme
data:
  ca.crt: |
    -----BEGIN CERTIFICATE-----
`,
		"a ConfigMap key that is a material file": `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-trust
  namespace: acme
data:
  internal-root.pem: |
    -----BEGIN CERTIFICATE-----
`,
		"a projected source delivering a certificate": workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
        - name: trust
          projected:
            sources:
              - configMap:
                  name: store-trust
                  items:
                    - key: ca.crt
                      path: ca.crt
`),
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			err := ValidateDocuments(documents(t, body), storeSubject, durable(), meshed())
			require.Error(t, err)
			var violation *Violation
			require.ErrorAs(t, err, &violation)
			require.Equal(t, RulePeerTransportMaterial, violation.Rule)
		})
	}

	// Without the mesh assertion, material the render delivers is this
	// environment's only transport protection and the rule does not apply. (The
	// projected-volume case above is also a mount violation, which is not
	// mesh-dependent, so it is not part of this check.)
	t.Run("delivered material is not refused where no mesh is asserted", func(t *testing.T) {
		for name, body := range refused {
			if name == "a projected source delivering a certificate" {
				continue
			}
			require.NoError(t, ValidateDocuments(documents(t, body), storeSubject, durable(), &Declaration{}), name)
		}
	})

	// The cases the previous, sniffing implementation got wrong in both
	// directions: an external destination is not this service's own transport, and
	// an ordinary credential is not certificate material.
	accepted := map[string]string{
		"an external HTTPS destination": workload(`      containers:
        - name: store
          env:
            - name: WEBHOOK_URL
              value: https://hooks.example.com/events
            - name: OIDC_ISSUER_URL
              value: https://login.example.com
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`),
		"a credential delivered as a secret reference": workload(`      containers:
        - name: store
          env:
            - name: APP_CLIENT_KEY
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: APP_CLIENT_KEY
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
`),
		"a Secret of an ordinary type carrying no material": `apiVersion: v1
kind: Secret
metadata:
  name: secret-store
  namespace: acme
type: Opaque
`,
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, ValidateDocuments(documents(t, body), storeSubject, durable(), meshed()),
				"a correct configuration must not need an allowance")
		})
	}
}

// A service's own declaration is where transport protocol is decided.
func TestADeclaredTLSEndpointIsRefusedOnAMeshedEnvironment(t *testing.T) {
	contracts := Contracts{}
	contracts.Add(&ServiceContract{
		Module: "shop", Service: "store", StorageMode: StorageModeDurable, Transport: TransportMesh,
		ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
		Endpoints:      []EndpointContract{{Name: "peer", Protocol: "https"}},
	})
	err := ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, contracts, meshed())
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, RulePeerTransportMaterial)
	require.ErrorContains(t, err, `endpoints[peer].api = https`)

	plain := Contracts{}
	plain.Add(&ServiceContract{
		Module: "shop", Service: "store", StorageMode: StorageModeDurable, Transport: TransportMesh,
		ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
		Endpoints:      []EndpointContract{{Name: "grpc", Protocol: "grpc"}, {Name: "http", Protocol: "http"}},
	})
	require.NoError(t, ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, plain, meshed()))
	require.NoError(t, ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, contracts, &Declaration{}),
		"without the mesh assertion a service's own TLS may be the only protection there is")
}

// Rule 3 is decided by the declaration, and a store that has not declared is
// refused rather than assumed conformant.
func TestStorageModeIsDecidedByTheDeclaration(t *testing.T) {
	stateful := workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
`)
	t.Run("an ephemeral declaration is refused", func(t *testing.T) {
		contracts := Contracts{}
		contracts.Add(&ServiceContract{
			Module: "shop", Service: "store", StorageMode: StorageModeEphemeral, Transport: TransportMesh,
			ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
		})
		err := ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, contracts, meshed())
		require.ErrorContains(t, err, RuleInMemoryStateStore)
		require.ErrorContains(t, err, "declares ephemeral storage")
	})
	t.Run("a store that declares no storage mode is refused by name", func(t *testing.T) {
		err := ValidateDocuments(documents(t, stateful), storeSubject, scratchOnly(), meshed())
		_ = err
		require.ErrorContains(t, err, RuleInMemoryStateStore)
		require.ErrorContains(t, err, "declares no storage mode")
		require.ErrorContains(t, err, "spec.deployment.storage")
	})
	t.Run("a declared durable store is admitted, and may mount its own state", func(t *testing.T) {
		require.NoError(t, ValidateDocuments(documents(t, stateful), storeSubject, durable(), meshed()))
		mounted := workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: data
              mountPath: /var/lib/store
      volumes:
        - name: tmp
          emptyDir: {}
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
`)
		require.NoError(t, ValidateDocuments(documents(t, mounted), storeSubject, durable(), meshed()))
		// Without the durable declaration the mount of that state is not admitted
		// either, and it is the mount that is named — the volume list is conforming.
		require.ErrorContains(t,
			ValidateDocuments(documents(t, mounted), storeSubject, scratchOnly(), meshed()),
			"declares no storage mode", "without the declaration nothing about its state is admitted")
	})
	t.Run("even a workload that keeps no state declares a storage mode", func(t *testing.T) {
		err := ValidateDocuments(documents(t, workload(conformingWorkload)), storeSubject, scratchOnly(), meshed())
		require.ErrorContains(t, err, RuleInMemoryStateStore)
		require.ErrorContains(t, err, "declares no storage mode")
	})
}

// An allowance excuses one rule for one service, and nothing else.
func TestAnAllowanceExcusesExactlyItsRuleAndService(t *testing.T) {
	body := workload(`      containers:
        - name: store
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: config
              mountPath: /etc/store
      volumes:
        - name: tmp
          emptyDir: {}
        - name: config
          configMap:
            name: store-config
`)
	allowed := meshed(Allowance{
		Rule: RuleNonScratchMount, Service: "shop/store", Reason: "a ConfigMap mount, reviewed by the platform owner",
	})
	require.NoError(t, ValidateDocuments(documents(t, body), storeSubject, durable(), allowed))

	bare := meshed(Allowance{Rule: RuleNonScratchMount, Service: "store", Reason: "the same, by bare name"})
	require.NoError(t, ValidateDocuments(documents(t, body), storeSubject, durable(), bare))

	other := meshed(Allowance{Rule: RuleNonScratchMount, Service: "shop/api", Reason: "unrelated"})
	require.Error(t, ValidateDocuments(documents(t, body), storeSubject, durable(), other))

	wrongRule := meshed(Allowance{Rule: RuleInMemoryStateStore, Service: "shop/store", Reason: "unrelated"})
	require.Error(t, ValidateDocuments(documents(t, body), storeSubject, durable(), wrongRule))
}

func TestContractFromServiceReadsWhatAServiceDeclares(t *testing.T) {
	service := &resources.Service{
		Name:      "store",
		Endpoints: []*resources.Endpoint{{Name: "tcp", API: "tcp"}},
		Spec: map[string]any{"deployment": map[string]any{
			"storage": "durable",
			"scratch-volumes": []any{
				map[string]any{"name": "tmp", "mount": "/tmp"},
				map[string]any{"name": "run", "mount": "/var/run/store"},
			},
		}},
	}
	contract, err := ContractFromService("shop", service)
	require.NoError(t, err)
	require.Equal(t, StorageModeDurable, contract.StorageMode)
	require.Equal(t, []EndpointContract{{Name: "tcp", Protocol: "tcp"}}, contract.Endpoints)
	require.Equal(t, []ScratchVolume{{Name: "tmp", Mount: "/tmp"}, {Name: "run", Mount: "/var/run/store"}},
		contract.ScratchVolumes, "the volumes the agent declares are what the render admits")

	// A declaration the render could not act on is refused where it is read.
	service.Spec = map[string]any{"deployment": map[string]any{
		"scratch-volumes": []any{map[string]any{"name": "tmp"}},
	}}
	_, err = ContractFromService("shop", service)
	require.ErrorContains(t, err, "must declare both name and mount")

	service.Spec = map[string]any{"deployment": map[string]any{"storage": "ephemeral"}}
	contract, err = ContractFromService("shop", service)
	require.NoError(t, err)
	require.Equal(t, StorageModeEphemeral, contract.StorageMode,
		"what the service declared is what the render acts on")

	service.Spec = map[string]any{"deployment": map[string]any{"storage": "in-memory"}}
	_, err = ContractFromService("shop", service)
	require.ErrorContains(t, err, "is not a storage mode")

	service.Spec = nil
	contract, err = ContractFromService("shop", service)
	require.NoError(t, err)
	require.Empty(t, contract.StorageMode, "a service that declares nothing declares nothing")
}

func TestDeclarationValidationRefusesWhatARenderCouldNotActOn(t *testing.T) {
	require.NoError(t, (*Declaration)(nil).Validate())
	require.ErrorContains(t,
		(&Declaration{Asserts: map[string]bool{"internal-transport/mesh": true}}).Validate(),
		`posture asserts unknown fact "internal-transport/mesh"`)
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: "no-mounts", Service: "api", Reason: "why"}}}).Validate(),
		`names unknown rule "no-mounts"`)
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: RuleNonScratchMount, Reason: "why"}}}).Validate(),
		"must name a service")
	require.ErrorContains(t,
		(&Declaration{Allowances: []Allowance{{Rule: RuleNonScratchMount, Service: "api"}}}).Validate(),
		"must state a reason")
}

func TestReportNamesEveryDeclaredAllowance(t *testing.T) {
	declaration := &Declaration{Allowances: []Allowance{
		{Rule: RuleNonScratchMount, Service: "shop/web", Reason: "the asset bundle is built into the image"},
	}}
	require.Equal(t, []string{
		"security posture: service shop/web is allowed to break rule non-scratch-mount — the asset bundle is built into the image",
	}, declaration.Report())
	require.Nil(t, (*Declaration)(nil).Report())
}

func TestSubjectFromPathNamesTheUnitThatRenderedTheManifest(t *testing.T) {
	defaults := Subject{Module: "shop", Service: "api"}
	cases := map[string]Subject{
		"services/store/base/stateful-set.yaml":                {Module: "shop", Service: "store"},
		"solutions/checkout/overlays/staging/deployment.yaml":  {Module: "shop", Service: "checkout"},
		"services/store/overlays/staging (Kustomize output)":   {Module: "shop", Service: "store"},
		"modules/billing/services/ledger/base/deployment.yaml": {Module: "billing", Service: "ledger"},
		"base/deployment.yaml":                                 defaults,
	}
	for path, expected := range cases {
		require.Equal(t, expected, SubjectFromPath(path, defaults), path)
	}
}

// writeTree materializes a tree on disk for the selector tests.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	return root
}
