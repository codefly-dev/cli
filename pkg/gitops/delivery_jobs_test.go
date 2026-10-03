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
	file, err := renderDeliveryJob(directory, deliveryJobName(deliveryPresence, "crm", "prod", [][]byte{[]byte("carrier")}), "crm", deliveryServiceAccount,
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
	// Named per settled set: the kind, the module, the environment and a
	// digest of the carriers, so a changed set is a new Job and two
	// environments of one workspace never replace each other's.
	if job.APIVersion != "batch/v1" || job.Kind != kindJob || job.Metadata.Namespace != "crm" || !strings.HasPrefix(job.Metadata.Name, "deliver-presence-crm-prod-") || len(job.Metadata.Name) != len("deliver-presence-crm-prod-")+10 {
		t.Fatalf("job identity %+v", job.Metadata)
	}
	if other := deliveryJobName(deliveryPresence, "crm", "prod", [][]byte{[]byte("another carrier")}); other == job.Metadata.Name {
		t.Fatalf("a changed set must be a new Job, got %q twice", other)
	}
	if staging := deliveryJobName(deliveryPresence, "crm", "staging", [][]byte{[]byte("carrier")}); staging == job.Metadata.Name {
		t.Fatalf("two environments must not share a Job name, got %q twice", staging)
	}
	for key, want := range map[string]string{
		argoHookAnnotation: argoHookSync, argoHookDeletePolicy: argoHookBeforeHookCreation, argoSyncWaveAnnotation: deliveryWave,
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
	for _, want := range []string{`"$DELIVERY_URL$DELIVERY_PATH"`, `Authorization: Bearer $(cat "$DELIVERY_IDENTITY_FILE")`, "--max-time 30", "400|401|403|409|422)", "2??)", "sleep", `[ "$refused" -eq 0 ]`} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	// The script is a manifest value: a ${…} in it is an unresolved placeholder
	// to the promotable ruleset, and a *_TOKEN variable with a value is a
	// credential to it. Both refused the Job at publish once; neither is
	// allowed back.
	if strings.Contains(script, "${") {
		t.Fatalf("script spells a variable as ${…}, which the promotable ruleset refuses as a placeholder:\n%s", script)
	}
	if _, present := env["DELIVERY_TOKEN"]; present {
		t.Fatal("an environment variable named *_TOKEN carrying a path is refused as a credential at publish")
	}
	if spec.Containers[0].SecurityContext["readOnlyRootFilesystem"] != true {
		t.Fatalf("the delivery container writes nothing and runs read-only: %v", spec.Containers[0].SecurityContext)
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
