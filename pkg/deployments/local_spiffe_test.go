package deployments

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	calls := 0
	kubectl := func(_ context.Context, stdin string, args ...string) (string, error) {
		calls++
		require.Empty(t, stdin)
		if args[1] == "csidriver" {
			return "csidriver.storage.k8s.io/spiffe.csi.cert-manager.io", nil
		}
		return "", nil
	}
	installed, err := EnsureWorkloadIssuer(t.Context(), kubectl, func(context.Context, string) ([]byte, error) { t.Fatal("fetched an existing issuer"); return nil, nil }, "local.example")
	require.NoError(t, err)
	require.False(t, installed)
	require.Equal(t, 2, calls)
	_, err = EnsureWorkloadIssuer(t.Context(), func(context.Context, string, ...string) (string, error) { return "", errors.New("permission denied") }, fetching(cmFixture), "local.example")
	require.ErrorContains(t, err, "permission denied")
}

func TestExistingCertManagerMustDisableDefaultApprover(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		record := &recordedKubectl{}
		kubectl := func(ctx context.Context, stdin string, args ...string) (string, error) {
			_, err := record.run(ctx, stdin, args...)
			if len(args) > 1 && args[0] == "get" && args[1] == "crd" {
				return "certificaterequests.cert-manager.io", nil
			}
			if len(args) > 2 && args[0] == "get" && args[1] == "deployment" {
				arguments := []string{"--v=1"}
				if disabled {
					arguments = append(arguments, disableDefaultApprover)
				}
				body, _ := json.Marshal(arguments)
				return string(body), nil
			}
			return "", err
		}
		installed, err := EnsureWorkloadIssuer(t.Context(), kubectl, func(context.Context, string) ([]byte, error) {
			t.Fatal("fetched existing cert-manager")
			return nil, nil
		}, "local.example")
		if disabled {
			require.NoError(t, err)
			require.True(t, installed)
		} else {
			require.ErrorContains(t, err, disableDefaultApprover)
			for _, call := range record.calls {
				require.True(t, strings.HasPrefix(call, "get "))
			}
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
		return "", err
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
