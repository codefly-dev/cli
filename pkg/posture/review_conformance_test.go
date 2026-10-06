package posture

import (
	"fmt"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The cases in this file were produced by the reviews of this change. They are kept
// here, permanently, because the defect that cost four review rounds was not any one
// of them: it was retiring a check without keeping the cases that proved it worked.
// A rule may be rewritten; these must keep passing, or the rewrite lost something.
//
// Each case is named for the invariant it holds, not for the review that found it.

// declaring builds a contract with everything declared, then applies overrides.
func declaring(mutate func(*ServiceContract)) Contracts {
	contract := ServiceContract{
		Module: "shop", Service: "store",
		StorageMode: StorageModeDurable, Transport: TransportMesh,
		ScratchVolumes: []ScratchVolume{{Name: "tmp", Mount: "/tmp"}},
	}
	if mutate != nil {
		mutate(&contract)
	}
	contracts := Contracts{}
	contracts.Add(&contract)
	return contracts
}

// container wraps container fields into a workload, for each of the three container
// lists — all three run in the cell, so a case must hold for all three.
func containerWorkload(list, body string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  template:
    spec:
      %s:
%s      volumes:
        - name: tmp
          emptyDir: {}
`, list, body)
}

const scratchMount = `          volumeMounts:
            - name: tmp
              mountPath: /tmp
`

// INVARIANT: absence is never conformance. A deployed workload that declares no
// storage mode, or no transport, is refused by name — whatever shape it has, and
// whether or not it happens to carry a volume the guard could have noticed.
func TestADeployedWorkloadWithoutItsDeclarationsIsRefused(t *testing.T) {
	bodies := map[string]string{
		"Deployment":  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: store\n  namespace: acme\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n",
		"StatefulSet": "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: store\n  namespace: acme\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n",
		"Job":         "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: store\n  namespace: acme\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n",
		"DaemonSet":   "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: store\n  namespace: acme\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n",
	}
	for kind, body := range bodies {
		t.Run("no storage declaration, "+kind, func(t *testing.T) {
			contracts := declaring(func(contract *ServiceContract) { contract.StorageMode = "" })
			err := ValidateDocuments(documents(t, body), storeSubject, contracts, meshed())
			require.ErrorContains(t, err, RuleInMemoryStateStore)
			require.ErrorContains(t, err, "declares no storage mode")
			require.ErrorContains(t, err, "spec.deployment.storage")
		})
		t.Run("no transport declaration, "+kind, func(t *testing.T) {
			contracts := declaring(func(contract *ServiceContract) { contract.Transport = "" })
			err := ValidateDocuments(documents(t, body), storeSubject, contracts, meshed())
			require.ErrorContains(t, err, RulePeerTransportMaterial)
			require.ErrorContains(t, err, "declares no transport")
			require.ErrorContains(t, err, "spec.deployment.transport")
		})
		t.Run("no contract at all, "+kind, func(t *testing.T) {
			require.Error(t, ValidateDocuments(documents(t, body), storeSubject, Contracts{}, meshed()))
		})
	}
}

// INVARIANT: a declaration the render cannot act on is refused where it is read, so
// a malformed one never becomes an absent one.
func TestAMalformedDeclarationIsRefusedWhereItIsRead(t *testing.T) {
	for name, deployment := range map[string]any{
		"an unknown storage mode":                  map[string]any{"storage": "durabl"},
		"a storage mode that is a mode of nothing": map[string]any{"storage": "memory"},
		"a storage field holding a list":           map[string]any{"storage": []any{"durable"}},
		"an unknown transport":                     map[string]any{"transport": "htttps"},
		"a scratch volume with no mount":           map[string]any{"scratch-volumes": []any{map[string]any{"name": "tmp"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ContractFromService("shop", serviceWithSpec(deployment))
			require.Error(t, err)
		})
	}
}

// INVARIANT: the rendered configuration must agree with the declaration. A service
// declaring that the platform carries its transport, beside configuration that
// terminates TLS of its own, is a contradiction — in any container list, in either
// flag form, through a reference, behind a prefix, or inside a shell wrapper.
func TestTransportDeclarationAndRenderedConfigurationMustAgree(t *testing.T) {
	cases := map[string]string{
		"a certificate file setting": `          env:
            - name: TLS_CERT_FILE
              value: /keys/listener.crt
`,
		"a certificate path under a name that says only path": `          env:
            - name: CERTIFICATE_PATH
              value: /keys/listener.crt
`,
		"an inline certificate under an unconventional name": `          env:
            - name: ROOT_CA
              value: |
                -----BEGIN CERTIFICATE-----
                MIIB
`,
		"a TLS switch that is on": `          env:
            - name: SERVICE_TLS_ENABLED
              value: "true"
`,
		"an https listener, joined":    "          args: [\"--listen=https://0.0.0.0:8443\"]\n",
		"an https listener, split":     "          args: [\"--listen\", \"https://0.0.0.0:8443\"]\n",
		"a certificate option, joined": "          args: [\"--cert=certificate\"]\n",
		"a certificate option, split":  "          args: [\"--cert\", \"certificate\"]\n",
	}
	for name, body := range cases {
		for _, list := range containerFields {
			t.Run(name+", "+list, func(t *testing.T) {
				manifest := containerWorkload(list, "        - name: store\n"+scratchMount+body)
				err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
				require.ErrorContains(t, err, RulePeerTransportMaterial)
				require.ErrorContains(t, err, "declares the platform carries its transport")
				require.ErrorContains(t, err, "contradict")
			})
		}
	}

	t.Run("through a shell wrapper, in any spelling of its flag", func(t *testing.T) {
		for _, flag := range []string{"-c", "-ec", "-euxc", "--command"} {
			manifest := containerWorkload("containers", fmt.Sprintf(
				"        - name: store\n%s          command: [\"/bin/sh\", %q]\n          args: [\"exec store server --tls-cert-file=/keys/listener.crt\"]\n",
				scratchMount, flag))
			require.ErrorContains(t,
				ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()),
				RulePeerTransportMaterial, flag)
		}
	})

	t.Run("through an envFrom reference, with its prefix applied", func(t *testing.T) {
		manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-settings
  namespace: acme
data:
  CERT_FILE: /keys/listener.crt
---
` + containerWorkload("containers", "        - name: store\n"+scratchMount+
			"          envFrom:\n            - configMapRef:\n                name: store-settings\n              prefix: TLS_\n")
		err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
		require.ErrorContains(t, err, RulePeerTransportMaterial)
		require.ErrorContains(t, err, "TLS_CERT_FILE")
	})

	// A value the render cannot read is not evidence of agreement.
	t.Run("an unreadable transport setting fails closed", func(t *testing.T) {
		manifest := containerWorkload("containers", "        - name: store\n"+scratchMount+
			"          env:\n            - name: TLS_CERT_FILE\n              valueFrom:\n                secretKeyRef:\n                  name: secret-store\n                  key: TLS_CERT_FILE\n")
		require.ErrorContains(t,
			ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()),
			"cannot read")
	})
}

// INVARIANT: the same agreement, for storage. A service declaring durable storage
// beside a development or in-memory setting is a contradiction.
func TestStorageDeclarationAndRenderedConfigurationMustAgree(t *testing.T) {
	cases := map[string]string{
		"a bare development flag":      "          args: [\"server\", \"-dev\"]\n",
		"a development flag set true":  "          args: [\"--dev=true\"]\n",
		"an in-memory storage, joined": "          args: [\"--storage=memory\"]\n",
		"an in-memory storage, split":  "          args: [\"--storage\", \"inmem\"]\n",
		"an in-memory switch by name": `          env:
            - name: STORE_IN_MEMORY
              value: "true"
`,
		"a storage backend set to memory": `          env:
            - name: STORAGE_BACKEND
              value: memory
`,
	}
	for name, body := range cases {
		for _, list := range containerFields {
			t.Run(name+", "+list, func(t *testing.T) {
				manifest := containerWorkload(list, "        - name: store\n"+scratchMount+body)
				err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
				require.ErrorContains(t, err, RuleInMemoryStateStore)
				require.ErrorContains(t, err, "declares durable storage")
				require.ErrorContains(t, err, "contradict")
			})
		}
	}
	t.Run("inside a shell wrapper", func(t *testing.T) {
		for _, flag := range []string{"-c", "-ec", "-euxc"} {
			manifest := containerWorkload("containers", fmt.Sprintf(
				"        - name: store\n%s          command: [\"/bin/sh\", %q]\n          args: [\"exec store server -dev\"]\n",
				scratchMount, flag))
			require.ErrorContains(t,
				ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()),
				RuleInMemoryStateStore, flag)
		}
	})
	// A setting that is off contradicts nothing.
	t.Run("a setting that is off is not a contradiction", func(t *testing.T) {
		for _, body := range []string{
			"          args: [\"--dev=false\"]\n",
			"          env:\n            - name: STORE_INMEM\n              value: \"false\"\n",
			"          env:\n            - name: MUTUAL_TLS\n              value: \"false\"\n",
		} {
			manifest := containerWorkload("containers", "        - name: store\n"+scratchMount+body)
			require.NoError(t, ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()), body)
		}
	})
}

// INVARIANT: a manifest this guard cannot read is refused, not skipped. A shape it
// drops is a shape it has not checked.
func TestMalformedManifestsFailClosed(t *testing.T) {
	cases := map[string]string{
		"a document with no kind":           "apiVersion: v1\nmetadata:\n  name: store\n",
		"containers that are not a list":    "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: store\nspec:\n  template:\n    spec:\n      containers: store\n",
		"a container that is not an object": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: store\nspec:\n  template:\n    spec:\n      containers:\n        - store\n",
		"volumes that are not a list":       "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: store\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n      volumes: tmp\n",
		"a volume that is not an object":    "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: store\nspec:\n  template:\n    spec:\n      containers:\n        - name: store\n      volumes:\n        - tmp\n",
		"a volume with several sources": `apiVersion: apps/v1
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
          configMap:
            name: store-config
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, ValidateDocuments(documents(t, body), storeSubject, declaring(nil), meshed()))
		})
	}
	t.Run("a Kustomization cycle is refused rather than selected as empty", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"services/store/base/kustomization.yaml":             "resources:\n  - ../overlays/staging\n",
			"services/store/overlays/staging/kustomization.yaml": "resources:\n  - ../../base\n",
		})
		_, err := SelectManifests(root, "staging")
		require.ErrorContains(t, err, "cycle")
	})
}

// INVARIANT: certificate material is established by parsing, never by a file name. An
// application's own key is not transport material, and a certificate is one wherever
// it is put.
func TestCertificateMaterialIsEstablishedByParsing(t *testing.T) {
	const certificate = "-----BEGIN CERTIFICATE-----\n    MIIB\n"
	refused := map[string]string{
		"a conventional Kubernetes key":           "data:\n  ca.crt: anything\n",
		"a certificate under an unrecognised key": "data:\n  ROOT_CA: |\n    " + certificate,
		"a certificate under a label":             "data:\n  CERTIFICATE: |\n    " + certificate,
		"a certificate in a trust bundle":         "data:\n  TRUST_BUNDLE: |\n    " + certificate,
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: store-trust\n  namespace: acme\n" + body
			require.ErrorContains(t,
				ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()),
				RulePeerTransportMaterial)
		})
	}
	accepted := map[string]string{
		"an application licence":       "data:\n  license.key: a-licence-string\n",
		"an ordinary cache identifier": "data:\n  cache.key: request-id\n",
		"a schema file":                "data:\n  schema.der: a-schema\n",
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: store-settings\n  namespace: acme\n" + body
			require.NoError(t,
				ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()),
				"ordinary application data is not transport material")
		})
	}
}

// INVARIANT: a volume's source is resolved, not inferred from its name. A claim
// template named like the scratch volume is a claim.
func TestAClaimTemplateIsNotScratchWhateverItIsCalled(t *testing.T) {
	manifest := `apiVersion: apps/v1
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
  volumeClaimTemplates:
    - metadata:
        name: tmp
      spec:
        accessModes: ["ReadWriteOnce"]
`
	err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
	require.ErrorContains(t, err, RuleNonScratchMount)
	require.ErrorContains(t, err, "not an emptyDir")

	t.Run("a raw block device is an attachment too", func(t *testing.T) {
		devices := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: store
          volumeDevices:
            - name: raw
              devicePath: /dev/xvda
      volumes:
        - name: tmp
          emptyDir: {}
`
		require.ErrorContains(t,
			ValidateDocuments(documents(t, devices), storeSubject, declaring(nil), meshed()),
			"raw device")
	})
}

// INVARIANT: a durable claim is the workload's own state. One that delivers the
// container's configuration contradicts the declaration that admitted it.
func TestADurableClaimThatDeliversConfigurationIsRefused(t *testing.T) {
	for _, mountPath := range []string{"/etc/service", "/app/settings", "/run/credentials"} {
		t.Run(mountPath, func(t *testing.T) {
			manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  template:
    spec:
      containers:
        - name: store
          args: ["--config=%s/settings.yaml"]
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: data
              mountPath: %s
      volumes:
        - name: tmp
          emptyDir: {}
        - name: data
          persistentVolumeClaim:
            claimName: store-config
`, mountPath, mountPath)
			err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
			require.ErrorContains(t, err, RuleNonScratchMount)
			require.ErrorContains(t, err, "delivers configuration")
		})
	}
}

// INVARIANT: identity is carried, not parsed out of a diagnostic string. A service
// whose manifests come from a fallback kustomization is the same service.
func TestServiceIdentitySurvivesTheDiagnosticAnnotation(t *testing.T) {
	root := writeTree(t, map[string]string{
		"services/store/kustomization.yaml": "resources:\n  - workload.yaml\n",
		"services/store/workload.yaml": `apiVersion: apps/v1
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
`,
	})
	documents, err := SelectManifests(root, "staging")
	require.NoError(t, err)
	for _, document := range documents {
		require.Equal(t, "store", document.Subject.Service, "identity comes from the path, not the annotation")
	}
	// An explicitly ephemeral declaration for that service must be found…
	ephemeral := declaring(func(contract *ServiceContract) { contract.StorageMode = StorageModeEphemeral })
	require.ErrorContains(t,
		ValidateDocuments(documents, Subject{Module: "shop"}, ephemeral, meshed()),
		"declares ephemeral storage")
	// …and so must an allowance naming it.
	allowed := meshed(Allowance{Rule: RuleNonScratchMount, Service: "shop/store", Reason: "reviewed"})
	err = ValidateDocuments(documents, Subject{Module: "shop"}, declaring(nil), allowed)
	require.NoError(t, err, "the allowance names the same service the refusal would")
}

// INVARIANT: an endpoint's declared protocol decides, and an endpoint outside the
// cell is not in-cell transport.
func TestEndpointProtocolAndLocationDecideTheTransportRule(t *testing.T) {
	workloadBody := containerWorkload("containers", "        - name: store\n"+scratchMount)
	t.Run("an external endpoint is somewhere else's transport", func(t *testing.T) {
		for _, protocol := range []string{"https", "grpcs", "tls"} {
			contracts := declaring(func(contract *ServiceContract) {
				contract.Endpoints = []EndpointContract{{Name: "upstream", Protocol: protocol, External: true}}
			})
			require.NoError(t,
				ValidateDocuments(documents(t, workloadBody), storeSubject, contracts, meshed()), protocol)
		}
	})
	t.Run("an endpoint's name is not its protocol", func(t *testing.T) {
		contracts := declaring(func(contract *ServiceContract) {
			contract.Endpoints = []EndpointContract{{Name: "https", Protocol: "http"}}
		})
		require.NoError(t, ValidateDocuments(documents(t, workloadBody), storeSubject, contracts, meshed()))
	})
	t.Run("an in-cell TLS protocol is refused", func(t *testing.T) {
		contracts := declaring(func(contract *ServiceContract) {
			contract.Endpoints = []EndpointContract{{Name: "peer", Protocol: "https"}}
		})
		require.ErrorContains(t,
			ValidateDocuments(documents(t, workloadBody), storeSubject, contracts, meshed()),
			RulePeerTransportMaterial)
	})
}

// INVARIANT: what conformed before still conforms. These are the acceptance cases
// carried forward from every review round.
func TestConformingRendersStillNeedNoAllowance(t *testing.T) {
	cases := map[string]string{
		"an external destination it dials": `          env:
            - name: WEBHOOK_URL
              value: https://hooks.example.com/events
            - name: OIDC_ISSUER_URL
              value: https://login.example.com
`,
		"a credential delivered by reference": `          env:
            - name: APP_CLIENT_KEY
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: APP_CLIENT_KEY
`,
		"an ordinary name that merely contains the letters": `          env:
            - name: SESSION_MAX_ACTIVE_DEVICES
              value: "4"
            - name: API_KEY_FILE
              value: /run/api/key
`,
		"a retired option named in a comment": "          command: [\"/bin/sh\", \"-c\"]\n          args: [\"# TLS_CERT_FILE was retired when the mesh took over\\nexec store server --listen=http://0.0.0.0:8080\"]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := containerWorkload("containers", "        - name: store\n"+scratchMount+body)
			require.NoError(t, ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()), name)
		})
	}
	t.Run("a declared durable store with its own state", func(t *testing.T) {
		manifest := `apiVersion: apps/v1
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
`
		require.NoError(t, ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed()))
	})
}

// serviceWithSpec is a service carrying one spec.deployment block.
func serviceWithSpec(deployment any) *resources.Service {
	return &resources.Service{Name: "store", Spec: map[string]any{"deployment": deployment}}
}

// INVARIANT: a reference resolves in the consuming workload's own namespace. Two
// ConfigMaps of the same name in different namespaces are two objects, and the
// second must not answer for the first.
func TestAReferenceResolvesInItsOwnNamespace(t *testing.T) {
	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: acme
data:
  TLS_CERT_FILE: /keys/listener.crt
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: other
data:
  TLS_CERT_FILE: ""
---
` + containerWorkload("containers", "        - name: store\n"+scratchMount+
		"          envFrom:\n            - configMapRef:\n                name: settings\n")
	err := ValidateDocuments(documents(t, manifest), storeSubject, declaring(nil), meshed())
	require.ErrorContains(t, err, RulePeerTransportMaterial,
		"the workload is in acme, so acme's ConfigMap is the one that answers")
	require.ErrorContains(t, err, "/keys/listener.crt")
}
