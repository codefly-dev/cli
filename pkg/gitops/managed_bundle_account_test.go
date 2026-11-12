package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeManagedStoreRender(t *testing.T, rendered string) string {
	t.Helper()
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "services", "store")
	require.NoError(t, os.MkdirAll(serviceRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(serviceRoot, "rendered.yaml"), []byte(rendered), 0o644))
	return serviceRoot
}

func retainedBundle(t *testing.T, serviceRoot string) string {
	t.Helper()
	var manifests string
	require.NoError(t, walkRegularFiles(serviceRoot, func(path, _ string, _ os.FileInfo) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests += "\n--- " + filepath.Base(path) + "\n" + string(data)
		return nil
	}))
	return manifests
}

const managedStoreJob = `apiVersion: batch/v1
kind: Job
metadata:
  name: store-migrate-aaaaaaaaaaaa
  labels:
    codefly.dev/bootstrap-service: store
spec:
  template:
    spec:
      serviceAccountName: store
      restartPolicy: Never
      containers:
        - name: migrate
          image: ghcr.io/codefly-dev/store-migrate@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

// A managed store's agent binds its bootstrap Job to the account that carries
// the Job's cloud identity (codefly-dev/cli#1346); the bundle keeps that account
// beside the Job and nothing else the render emitted, an account the Job does
// not name included.
func TestManagedServicePromotionRetainsTheAccountItsJobRunsAs(t *testing.T) {
	serviceRoot := writeManagedStoreRender(t, `apiVersion: v1
kind: ServiceAccount
metadata:
  name: store
  annotations:
    iam.gke.io/gcp-service-account: owner@project.iam.gserviceaccount.com
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: unrelated
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
---
`+managedStoreJob)
	retained, err := retainManagedBundle(serviceRoot, "store", "production", "payments", nil)
	require.NoError(t, err)
	require.True(t, retained)
	bundle := retainedBundle(t, serviceRoot)
	require.Contains(t, bundle, "kind: Job")
	require.Contains(t, bundle, "kind: ServiceAccount")
	require.Contains(t, bundle, "iam.gke.io/gcp-service-account: owner@project.iam.gserviceaccount.com")
	require.NotContains(t, bundle, "name: unrelated")
	require.NotContains(t, bundle, "kind: StatefulSet")
	require.Equal(t, 1, strings.Count(bundle, "kind: ServiceAccount"))
	kustomization, err := os.ReadFile(filepath.Join(serviceRoot, "base", "kustomization.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "service-account-1.yaml")
	require.Contains(t, string(kustomization), "job-1.yaml")
}

// A Job bound to an account the render never emitted would run as nothing the
// cluster knows; the bundle refuses it rather than ship the Job alone.
func TestManagedServicePromotionRefusesAJobNamingAnAbsentAccount(t *testing.T) {
	serviceRoot := writeManagedStoreRender(t, managedStoreJob)
	_, err := retainManagedBundle(serviceRoot, "store", "production", "payments", nil)
	require.ErrorContains(t, err, `bootstrap Job runs as ServiceAccount "store", which its render does not emit`)
}
