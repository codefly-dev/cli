package localcluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

func TestApplicationOwnerMustBeTheAppliedSet(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"same name different UID": func(owner map[string]any) { owner["uid"] = "replaced-set" },
		"different name":          func(owner map[string]any) { owner["name"] = "foreign" },
		"different kind":          func(owner map[string]any) { owner["kind"] = "Application" },
		"different API":           func(owner map[string]any) { owner["apiVersion"] = "other/v1" },
		"not controller":          func(owner map[string]any) { owner["controller"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			argo := newFakeArgo("https://kubernetes.default.svc")
			mutate(argo.app["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any))
			driver := &ClusterDriver{AwaitTimeout: time.Millisecond, Poll: time.Millisecond, nonce: "run", Kubectl: argo.run}
			if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{{Name: "j", Namespace: "payments", Application: deliveryApplication()}}); err == nil || len(argo.patches) != 0 {
				t.Fatalf("foreign owner acquired a sync: %v (%d patches)", err, len(argo.patches))
			}
		})
	}
}

func TestDeliveryOwnersBindToBootstrapApplyResponse(t *testing.T) {
	object := map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "metadata": map[string]any{"name": "payments-local", "namespace": "argocd", "uid": "applied-set"}}
	for _, format := range []string{"single", "list", "stream"} {
		var response any = object
		if format == "list" {
			response = map[string]any{"kind": "List", "items": []any{map[string]string{"kind": "AppProject"}, object}}
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if format == "stream" {
			encoded = append([]byte(`{"kind":"AppProject"}`+"\n"), encoded...)
		}
		sets, err := appliedApplicationSets(string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		for _, attack := range []string{"none", "missing", "foreign name", "foreign namespace", "no UID", "ambiguous"} {
			t.Run(attack, func(t *testing.T) {
				jobs := []gitops.DeliveryJob{{Application: deliveryApplication()}}
				jobs[0].Application.Owner.UID = ""
				applied := append([]gitops.ApplicationSetIdentity(nil), sets...)
				switch attack {
				case "missing":
					applied = nil
				case "foreign name":
					applied[0].Name = "elsewhere"
				case "foreign namespace":
					applied[0].Namespace = "elsewhere"
				case "no UID":
					applied[0].UID = ""
				case "ambiguous":
					applied = append(applied, applied[0])
				}
				err := bindApplicationOwners(jobs, applied)
				if attack != "none" {
					if err == nil {
						t.Fatal("unbound owner accepted")
					}
					return
				}
				if err != nil || jobs[0].Application.Owner != deliveryApplication().Owner {
					t.Fatalf("owner not bound from apply: %+v %v", jobs, err)
				}
			})
		}
	}
}
