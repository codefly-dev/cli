package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testDeliveryTarget() *DeliveryTarget {
	return &DeliveryTarget{URL: "https://accounts.platform.svc.cluster.local:8443", Audience: "accounts"}
}

// TestDeliveryJobIsASyncHookThatPostsEveryCarrier pins the carrier Job's shape:
// an Argo Sync hook re-run on every sync, in the workloads' wave, running as the
// delivery ServiceAccount with a token projected for the host's audience, the
// documents mounted from their ConfigMaps, and a script that retries what may
// succeed later and fails on the host's verdicts.
func TestDeliveryJobIsASyncHookThatPostsEveryCarrier(t *testing.T) {
	directory := t.TempDir()
	file, err := renderDeliveryJob(directory, deliveryJobName(deliveryPresence, "crm"), "crm", deliveryServiceAccount,
		deliveryPresence, presenceDeliveryPath, testDeliveryTarget(), []deliveryDocument{
			{ConfigMap: "solution-host-binding-example.prod.crm", Key: presenceCarrierKey, Name: "example.prod.crm.json"},
			{ConfigMap: "solution-host-binding-example.prod.billing", Key: presenceCarrierKey, Name: "example.prod.billing.json"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if file != "deliver-presence.yaml" {
		t.Fatalf("job file %q", file)
	}
	data, err := os.ReadFile(filepath.Join(directory, file))
	if err != nil {
		t.Fatal(err)
	}
	var job deliveryJobManifest
	if err = yaml.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != kindJob || job.Metadata.Namespace != "crm" || job.Metadata.Name != "deliver-presence-crm" {
		t.Fatalf("job identity %+v", job.Metadata)
	}
	for key, want := range map[string]string{
		argoHookAnnotation: argoHookSync, argoHookDeletePolicy: argoHookBeforeHookCreation, argoSyncWaveAnnotation: consumerUnitWave,
	} {
		if job.Metadata.Annotations[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, job.Metadata.Annotations[key], want)
		}
	}
	spec := job.Spec.Template.Spec
	if spec.ServiceAccountName != deliveryServiceAccount || spec.AutomountServiceAccountToken || spec.RestartPolicy != "OnFailure" {
		t.Fatalf("pod spec %+v", spec)
	}
	if len(spec.Containers) != 1 || spec.Containers[0].Image != deliveryImage || !strings.Contains(deliveryImage, "@sha256:") {
		t.Fatalf("container %+v", spec.Containers)
	}
	env := map[string]string{}
	for _, entry := range spec.Containers[0].Env {
		env[entry.Name] = entry.Value
	}
	if env["DELIVERY_URL"] != "https://accounts.platform.svc.cluster.local:8443" || env["DELIVERY_PATH"] != presenceDeliveryPath {
		t.Fatalf("delivery env %v", env)
	}
	script := spec.Containers[0].Command[2]
	for _, want := range []string{`"${DELIVERY_URL}${DELIVERY_PATH}"`, "Authorization: Bearer $(cat", "400|401|403|409|422)", "2??)", "sleep"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	encoded := string(data)
	for _, want := range []string{
		"audience: accounts", "expirationSeconds: 600", "path: token",
		"name: solution-host-binding-example.prod.billing", "path: example.prod.billing.json",
		"name: solution-host-binding-example.prod.crm", "key: " + presenceCarrierKey,
	} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("job lacks %q:\n%s", want, encoded)
		}
	}
	// Documents mount in a stable order, so a re-render with no change is
	// byte-identical.
	if strings.Index(encoded, "example.prod.billing") > strings.Index(encoded, "example.prod.crm.json") {
		t.Fatal("documents are not projected in name order")
	}
}

func TestDeliveryJobRefusesWithoutATargetOrADocument(t *testing.T) {
	directory := t.TempDir()
	if _, err := renderDeliveryJob(directory, "deliver", "crm", deliveryServiceAccount, deliveryPresence, presenceDeliveryPath, nil,
		[]deliveryDocument{{ConfigMap: "a", Key: presenceCarrierKey, Name: "a.json"}}); err == nil || !strings.Contains(err.Error(), "no delivery endpoint") {
		t.Fatalf("a missing target was not refused: %v", err)
	}
	if _, err := renderDeliveryJob(directory, "deliver", "crm", deliveryServiceAccount, deliveryAuthority, authorityDeliveryPath, testDeliveryTarget(), nil); err == nil || !strings.Contains(err.Error(), "no authority document") {
		t.Fatalf("an empty document list was not refused: %v", err)
	}
}

func TestDeliveryServiceAccountMountsNoDefaultToken(t *testing.T) {
	directory := t.TempDir()
	file, err := renderDeliveryServiceAccount(directory, "crm")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, file))
	if err != nil {
		t.Fatal(err)
	}
	var account map[string]any
	if err = yaml.Unmarshal(data, &account); err != nil {
		t.Fatal(err)
	}
	if account["kind"] != kindServiceAccount || account["automountServiceAccountToken"] != false {
		t.Fatalf("service account %v", account)
	}
	if metadataString(account, "name") != deliveryServiceAccount || metadataString(account, "namespace") != "crm" {
		t.Fatalf("service account identity %v", account["metadata"])
	}
}
