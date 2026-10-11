package deployments

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const cmFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager
spec:
  template:
    spec:
      containers:
        - name: cert-manager-controller
          image: quay.io/jetstack/cert-manager-controller:v1.21.2
          args:
            - --cluster-resource-namespace=$(POD_NAMESPACE)
            - --leader-election-namespace=kube-system
`

func TestWorkloadIssuerPinningPrecedesEveryApply(t *testing.T) {
	for _, tc := range []struct{ name, cm, cmSHA, csi, csiSHA, domain, want string }{
		{"cert-manager changed", cmFixture, "wrong", string(spiffeManifest), spiffeManifestSHA256, "example.test", "not the reviewed"},
		{"driver changed", cmFixture, digestOf(cmFixture), string(spiffeManifest) + "# changed\n", spiffeManifestSHA256, "example.test", "not the reviewed"},
		{"unpinned controller", strings.ReplaceAll(cmFixture, "quay.io/jetstack/cert-manager-controller:v1.21.2", "example.test/unreviewed:latest"), "", string(spiffeManifest), spiffeManifestSHA256, "example.test", "no digest pins"},
		{"unpinned driver", cmFixture, digestOf(cmFixture), strings.ReplaceAll(string(spiffeManifest), "quay.io/jetstack/livenessprobe:v2.19.0", "example.test/evil:latest"), "", "example.test", "no digest pins"},
		{"invalid domain", cmFixture, digestOf(cmFixture), string(spiffeManifest), spiffeManifestSHA256, "bad/domain", "trust_domain"},
		{"missing approver anchor", strings.ReplaceAll(cmFixture, "--cluster-resource-namespace=$(POD_NAMESPACE)", "--other-flag"), "", string(spiffeManifest), spiffeManifestSHA256, "example.test", "insertion point"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cmSHA == "" {
				tc.cmSHA = digestOf(tc.cm)
			}
			if tc.csiSHA == "" {
				tc.csiSHA = digestOf(tc.csi)
			}
			kubectl := &recordedKubectl{}
			_, err := ensureWorkloadIssuer(t.Context(), kubectl.run, fetching(tc.cm), tc.domain, tc.cmSHA, certManagerImages, []byte(tc.csi), tc.csiSHA)
			require.ErrorContains(t, err, tc.want)
			for _, call := range kubectl.calls {
				require.True(t, strings.HasPrefix(call, "get "), call)
			}
		})
	}
}

func TestWorkloadIssuerInstallsPinnedComponentsInOrder(t *testing.T) {
	kubectl := &recordedKubectl{}
	installed, err := ensureWorkloadIssuer(t.Context(), kubectl.run, fetching(cmFixture), "local.example", digestOf(cmFixture), certManagerImages, spiffeManifest, spiffeManifestSHA256)
	require.NoError(t, err)
	require.True(t, installed)
	joined := strings.Join(kubectl.stdins, "\n")
	require.Contains(t, joined, "--cluster-resource-namespace=$(POD_NAMESPACE)\n            - "+disableDefaultApprover)
	require.Equal(t, 2, strings.Count(joined, "--trust-domain=local.example"))
	require.NotContains(t, joined, "--trust-domain=cluster.local")
	for _, match := range imageLine.FindAllStringSubmatch(joined, -1) {
		require.Contains(t, match[2], "@sha256:")
	}
	calls := strings.Join(kubectl.calls, "\n")
	require.Contains(t, calls, "rollout status deployment/cert-manager-webhook")
	require.Contains(t, calls, "rollout status daemonset/cert-manager-csi-driver-spiffe-driver")
	require.Contains(t, calls, "rollout status deployment/cert-manager-csi-driver-spiffe-approver")
	webhook, ca, driver := -1, -1, -1
	for i, call := range kubectl.calls {
		if strings.Contains(call, "deployment/cert-manager-webhook") {
			webhook = i
		}
		if strings.Contains(kubectl.stdins[i], `"tls.key"`) {
			ca = i
		}
		if strings.Contains(kubectl.stdins[i], "kind: CSIDriver") {
			driver = i
		}
	}
	require.Less(t, webhook, ca)
	require.Less(t, ca, driver)
}

func TestExistingIssuerIsUsedAsIsAndReadFailuresRefuse(t *testing.T) {
	workloads := reviewedIssuerWorkloads(t)
	calls := 0
	kubectl := func(_ context.Context, stdin string, args ...string) (string, error) {
		calls++
		require.Empty(t, stdin)
		if args[1] == "csidriver" {
			return "csidriver.storage.k8s.io/spiffe.csi.cert-manager.io", nil
		}
		return issuerWorkloadJSON(t, workloads, args), nil
	}
	installed, err := EnsureWorkloadIssuer(t.Context(), kubectl, func(context.Context, string) ([]byte, error) { t.Fatal("fetched an existing issuer"); return nil, nil }, "local.example")
	require.NoError(t, err)
	require.False(t, installed)
	require.Equal(t, 7, calls)
	_, err = EnsureWorkloadIssuer(t.Context(), func(context.Context, string, ...string) (string, error) { return "", errors.New("permission denied") }, fetching(cmFixture), "local.example")
	require.ErrorContains(t, err, "permission denied")
}

func TestExistingCertManagerMustDisableDefaultApprover(t *testing.T) {
	for _, driver := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("driver=%t/disabled=%t", driver, disabled), func(t *testing.T) {
				workloads := reviewedIssuerWorkloads(t)
				if !disabled {
					workloads["cert-manager"].Spec.Template.Spec.Containers[0].Args = nil
				}
				cert, _, err := newLocalSPIFFECA(time.Now())
				require.NoError(t, err)
				record := &recordedKubectl{}
				kubectl := func(ctx context.Context, stdin string, args ...string) (string, error) {
					_, err := record.run(ctx, stdin, args...)
					if args[0] == "get" {
						switch args[1] {
						case "crd":
							return "certificaterequests.cert-manager.io", nil
						case "csidriver":
							if driver {
								return "spiffe.csi.cert-manager.io", nil
							}
						case "secret":
							return "kubernetes.io/tls\n" + base64.StdEncoding.EncodeToString(cert), nil
						}
					}
					return issuerWorkloadJSON(t, workloads, args), err
				}
				installed, err := EnsureWorkloadIssuer(t.Context(), kubectl, func(context.Context, string) ([]byte, error) {
					t.Fatal("fetched existing cert-manager")
					return nil, nil
				}, "local.example")
				if disabled {
					require.NoError(t, err)
					require.Equal(t, !driver, installed)
				} else {
					require.ErrorContains(t, err, disableDefaultApprover)
					for _, call := range record.calls {
						require.True(t, strings.HasPrefix(call, "get "), call)
					}
					require.NotContains(t, strings.Join(record.calls, "\n"), "get secret")
				}
			})
		}
	}
}

func TestLocalSPIFFECAReusesAndRotatesWithoutExportingKey(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := newLocalSPIFFECA(now)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	require.True(t, ok)
	require.Equal(t, 256, key.Curve.Params().BitSize)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.True(t, cert.IsCA)
	require.NoError(t, cert.CheckSignatureFrom(cert))
	for _, tc := range []struct {
		name   string
		at     time.Time
		rotate bool
	}{{"reuse", now, false}, {"expiring", now.Add(29 * 24 * time.Hour), true}, {"expired", now.Add(31 * 24 * time.Hour), true}} {
		t.Run(tc.name, func(t *testing.T) {
			record := &recordedKubectl{}
			require.NoError(t, ensureLocalSPIFFECA(t.Context(), record.run, base64.StdEncoding.EncodeToString(certPEM), tc.at))
			applied := strings.Join(record.stdins, "\n")
			require.Equal(t, tc.rotate, strings.Contains(applied, `"tls.key"`))
			require.NotContains(t, applied, base64.StdEncoding.EncodeToString(keyPEM))
			if !tc.rotate {
				require.Contains(t, applied, base64.StdEncoding.EncodeToString(certPEM))
			}
		})
	}
	require.ErrorContains(t, ensureLocalSPIFFECA(t.Context(), (&recordedKubectl{}).run, "broken", now), "encoding")
	err = ensureLocalSPIFFECA(t.Context(), func(context.Context, string, ...string) (string, error) { return "", errors.New("SECRET KEY ECHO") }, "", now)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET KEY ECHO")
}

func TestExistingLocalIssuerReusesOnlyThePublicCACertificate(t *testing.T) {
	workloads := reviewedIssuerWorkloads(t)
	cert, key, err := newLocalSPIFFECA(time.Now())
	require.NoError(t, err)
	record := &recordedKubectl{}
	kubectl := func(ctx context.Context, stdin string, args ...string) (string, error) {
		_, err := record.run(ctx, stdin, args...)
		if len(args) > 1 && args[0] == "get" && args[1] == "csidriver" {
			return "spiffe.csi.cert-manager.io", nil
		}
		if len(args) > 1 && args[0] == "get" && args[1] == "secret" {
			require.Equal(t, `jsonpath={.type}{"\n"}{.data.tls\.crt}`, args[len(args)-1])
			return "kubernetes.io/tls\n" + base64.StdEncoding.EncodeToString(cert), nil
		}
		return issuerWorkloadJSON(t, workloads, args), err
	}
	installed, err := EnsureWorkloadIssuer(t.Context(), kubectl, func(context.Context, string) ([]byte, error) { t.Fatal("fetched existing issuer"); return nil, nil }, "local.example")
	require.NoError(t, err)
	require.False(t, installed)
	applied := strings.Join(record.stdins, "\n")
	require.Contains(t, applied, base64.StdEncoding.EncodeToString(cert))
	require.NotContains(t, applied, "tls.key")
	require.NotContains(t, applied, base64.StdEncoding.EncodeToString(key))
	_, err = readLocalSPIFFECA(t.Context(), func(context.Context, string, ...string) (string, error) { return "kubernetes.io/tls\n", nil })
	require.ErrorContains(t, err, "existing local SPIFFE CA Secret")
}

// Cluster reads use real Kubernetes JSON shapes, including every driver sidecar.
func reviewedIssuerWorkloads(t *testing.T) map[string]*issuerWorkload {
	t.Helper()
	out := map[string]*issuerWorkload{}
	for _, component := range []string{"controller", "webhook", "cainjector"} {
		name := "cert-manager-" + component
		workload := &issuerWorkload{Kind: "Deployment"}
		workload.Metadata.Name = name
		image := "quay.io/jetstack/" + name + ":v1.21.2"
		container := issuerContainer{Name: name, Image: image + "@" + certManagerImages[image]}
		if component == "controller" {
			workload.Metadata.Name = "cert-manager"
			container.Args = []string{disableDefaultApprover}
		}
		workload.Spec.Template.Spec.Containers = []issuerContainer{container}
		out[workload.Metadata.Name] = workload
	}
	pinned, err := pinnedSPIFFEManifest(spiffeManifest, spiffeManifestSHA256, "local.example")
	require.NoError(t, err)
	decoder := yaml.NewDecoder(bytes.NewReader(pinned))
	for {
		var workload issuerWorkload
		err := decoder.Decode(&workload)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if workload.Kind == "Deployment" || workload.Kind == "DaemonSet" {
			out[workload.Metadata.Name] = &workload
		}
	}
	return out
}

func issuerWorkloadJSON(t *testing.T, workloads map[string]*issuerWorkload, args []string) string {
	t.Helper()
	if len(args) < 3 || args[0] != "get" || (args[1] != "deployment" && args[1] != "daemonset") {
		return ""
	}
	workload := workloads[args[2]]
	require.NotNil(t, workload, args)
	require.Equal(t, "json", args[len(args)-1])
	body, err := json.Marshal(workload)
	require.NoError(t, err)
	return string(body)
}

func TestExistingIssuerRefusesEveryUnreviewedComponentImage(t *testing.T) {
	for name, workload := range reviewedIssuerWorkloads(t) {
		for index, container := range workload.Spec.Template.Spec.Containers {
			for _, mutation := range []string{"tag-only", "different-digest", "wrong-component"} {
				t.Run(name+"/"+container.Name+"/"+mutation, func(t *testing.T) {
					workloads := reviewedIssuerWorkloads(t)
					tagged, _, _ := strings.Cut(container.Image, "@")
					bad := tagged
					if mutation == "different-digest" {
						bad += "@sha256:" + strings.Repeat("0", 64)
					}
					if mutation == "wrong-component" {
						bad = workloads["cert-manager-webhook"].Spec.Template.Spec.Containers[0].Image
						if bad == container.Image {
							bad = workloads["cert-manager"].Spec.Template.Spec.Containers[0].Image
						}
					}
					workloads[name].Spec.Template.Spec.Containers[index].Image = bad
					_, err := EnsureWorkloadIssuer(t.Context(), func(_ context.Context, stdin string, args ...string) (string, error) {
						require.Empty(t, stdin)
						require.Equal(t, "get", args[0])
						if args[1] == "csidriver" {
							return "spiffe.csi.cert-manager.io", nil
						}
						return issuerWorkloadJSON(t, workloads, args), nil
					}, nil, "local.example")
					require.ErrorContains(t, err, name)
					require.ErrorContains(t, err, bad)
				})
			}
		}
	}
}

func TestExistingIssuerRefusesChangedSecurityArguments(t *testing.T) {
	for _, tc := range []struct{ name, flag, value string }{
		{"cert-manager", "--controllers", "*"},
		{"cert-manager-csi-driver-spiffe-approver", "--issuer-name", "other"},
		{"cert-manager-csi-driver-spiffe-approver", "--issuer-kind", "Issuer"},
		{"cert-manager-csi-driver-spiffe-approver", "--issuer-group", "other.io"},
		{"cert-manager-csi-driver-spiffe-approver", "--trust-domain", "other.test"},
		{"cert-manager-csi-driver-spiffe-driver", "--issuer-name", "other"},
		{"cert-manager-csi-driver-spiffe-driver", "--trust-domain", "other.test"},
	} {
		for _, change := range []string{"missing", "replaced", "duplicate", "split"} {
			t.Run(tc.name+"/"+tc.flag+"/"+change, func(t *testing.T) {
				workloads := reviewedIssuerWorkloads(t)
				containers := workloads[tc.name].Spec.Template.Spec.Containers
				for i := range containers {
					var args []string
					for _, arg := range containers[i].Args {
						if !strings.HasPrefix(arg, tc.flag+"=") {
							args = append(args, arg)
							continue
						}
						switch change {
						case "replaced":
							args = append(args, tc.flag+"="+tc.value)
						case "duplicate":
							args = append(args, arg, tc.flag+"="+tc.value)
						case "split":
							args = append(args, tc.flag, tc.value)
						}
					}
					containers[i].Args = args
				}
				_, err := EnsureWorkloadIssuer(t.Context(), func(_ context.Context, stdin string, args ...string) (string, error) {
					require.Empty(t, stdin)
					require.Equal(t, "get", args[0])
					if args[1] == "csidriver" {
						return "spiffe.csi.cert-manager.io", nil
					}
					return issuerWorkloadJSON(t, workloads, args), nil
				}, nil, "local.example")
				require.ErrorContains(t, err, tc.name)
				require.ErrorContains(t, err, tc.flag)
			})
		}
	}
}
