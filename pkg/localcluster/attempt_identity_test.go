package localcluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

func TestAcceptedSyncStampsItsHookAndRefusesForeignAttempts(t *testing.T) {
	for _, attack := range []string{"own", "missing nonce", "foreign nonce", "ambiguous jobs"} {
		t.Run(attack, func(t *testing.T) {
			argo := newFakeArgo("https://kubernetes.default.svc")
			job := gitops.DeliveryJob{Name: "delivery", Namespace: "payments", Application: deliveryApplication()}
			driver := &ClusterDriver{AwaitTimeout: 10 * time.Millisecond, Poll: time.Millisecond, nonce: "run"}
			logs := 0
			driver.Kubectl = func(ctx context.Context, stdin string, args ...string) (string, error) {
				call := strings.Join(args, " ")
				switch {
				case strings.Contains(call, "get jobs"):
					// Read the annotation from the operation the server accepted,
					// as Kustomize applies it to the hook. Never from driver state.
					state := argo.app["status"].(map[string]any)["operationState"].(map[string]any)
					operation := state["operation"].(map[string]any)
					sync := operation["sync"].(map[string]any)
					source := sync["source"].(map[string]any)
					if source["repoURL"] != job.Application.RepoURL || source["path"] != job.Application.Path || source["targetRevision"] != job.Application.TargetRevision {
						t.Fatal("sync override changed the rendered source")
					}
					kustomize := source["kustomize"].(map[string]any)
					if kustomize["forceCommonAnnotations"] != true {
						t.Fatal("nonce does not override a stale authored annotation")
					}
					nonce := kustomize["commonAnnotations"].(map[string]any)[deliveryAttemptAnnotation].(string)
					switch attack {
					case "missing nonce":
						nonce = ""
					case "foreign nonce":
						nonce = "previous-operation"
					}
					answer := jobsAnswer("fresh", syncStarted, "Complete", nonce)
					if attack == "ambiguous jobs" {
						var list map[string]any
						if err := json.Unmarshal([]byte(answer), &list); err != nil {
							t.Fatal(err)
						}
						items := list["items"].([]any)
						list["items"] = append(items, items[0])
						encoded, err := json.Marshal(list)
						if err != nil {
							t.Fatal(err)
						}
						answer = string(encoded)
					}
					return answer, nil
				case strings.Contains(call, "logs"):
					logs++
					return "delivered own.json: {}\n", nil
				}
				return argo.run(ctx, stdin, args...)
			}
			if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{job}); err != nil {
				t.Fatal(err)
			}
			delivery, err := driver.Await(context.Background(), &job)
			if attack == "own" {
				if err != nil || !delivery.Succeeded || delivery.Attempt != "fresh" || logs != 1 {
					t.Fatalf("own hook not accepted: %+v %v, logs=%d", delivery, err, logs)
				}
			} else if err == nil || delivery.Succeeded || logs != 0 {
				t.Fatalf("unbound hook accepted: %+v %v, logs=%d", delivery, err, logs)
			}
		})
	}
}

func TestSyncAcceptanceRequiresTheHookStamp(t *testing.T) {
	argo := newFakeArgo("https://kubernetes.default.svc")
	driver := &ClusterDriver{AwaitTimeout: 10 * time.Millisecond, Poll: time.Millisecond, nonce: "run"}
	driver.Kubectl = func(ctx context.Context, stdin string, args ...string) (string, error) {
		answer, err := argo.run(ctx, stdin, args...)
		if strings.Contains(strings.Join(args, " "), "get applications") {
			// Model an API/controller that accepted the info but dropped the
			// source override: it cannot prove which hook this operation made.
			var response map[string]any
			if err := json.Unmarshal([]byte(answer), &response); err != nil {
				t.Fatal(err)
			}
			app := response["items"].([]any)[0].(map[string]any)
			if state, ok := app["status"].(map[string]any)["operationState"].(map[string]any); ok {
				delete(state["operation"].(map[string]any), "sync")
			}
			encoded, marshalErr := json.Marshal(response)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			answer = string(encoded)
		}
		return answer, err
	}
	if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{{Name: "delivery", Namespace: "payments", Application: deliveryApplication()}}); err == nil {
		t.Fatal("accepted an operation that cannot stamp its hook")
	}
}
