package gitops

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewECFProjectedSourceCollision inverts the review's full-render
// overwrite probe, including sources before the token and directory conflicts.
func TestReviewECFProjectedSourceCollision(t *testing.T) {
	for _, kind := range []string{"downwardAPI", "configMap", "secret", "serviceAccountToken", "clusterTrustBundle"} {
		for _, file := range []string{"token", "token/child", ".", "nested"} {
			for _, before := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/before=%t", kind, file, before), func(t *testing.T) {
					file := file
					tokenPath, mountPath := "token", identityTokenMount
					if file == "nested" {
						tokenPath, mountPath = "codefly/token", "/var/run/secrets"
						file = "codefly" // a proper prefix of the token path
					}
					body := withTokenMount("accounts", mountPath, tokenPath, "")
					var projection string
					switch kind {
					case "serviceAccountToken":
						projection = "              - serviceAccountToken:\n                  audience: other\n                  expirationSeconds: 600\n                  path: " + file + "\n"
					case "clusterTrustBundle":
						projection = "              - clusterTrustBundle:\n                  name: trust\n                  path: " + file + "\n"
					default:
						projection = "              - " + kind + ":\n"
						if kind != "downwardAPI" {
							projection += "                  name: payload\n"
						}
						projection += "                  items:\n                    - path: " + file + "\n"
						if kind == "downwardAPI" {
							projection += "                      fieldRef:\n                        apiVersion: v1\n                        fieldPath: metadata.name\n"
						} else {
							projection += "                      key: payload\n"
						}
					}
					marker := "      containers:\n"
					if before {
						marker = "              - serviceAccountToken:\n"
					}
					body = strings.Replace(body, marker, projection+marker, 1)
					_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(filepath.Join(t.TempDir(), "modules", "crm")), kustomizedWorkload(body))
					if err == nil || !strings.Contains(err.Error(), "collides with token path") {
						t.Fatalf("projection collision was not diagnosed: %v", err)
					}
				})
			}
		}
	}
}

func TestProjectedTokenReuseRequiresBoundedDisjointSources(t *testing.T) {
	for _, kind := range []string{"configMap", "secret"} {
		for _, bounded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bounded=%t", kind, bounded), func(t *testing.T) {
				projection := "              - " + kind + ":\n                  name: payload\n"
				if bounded {
					projection += "                  items:\n                    - key: ca\n                      path: ca.crt\n"
				}
				body := strings.Replace(withTokenMount("accounts", identityTokenMount, "token", ""), "      containers:\n", projection+"      containers:\n", 1)
				destination := filepath.Join(t.TempDir(), "modules", "crm")
				_, err := RenderOwnedTree(context.Background(), solutionRenderOptions(destination), kustomizedWorkload(body))
				if bounded {
					if err != nil {
						t.Fatal(err)
					}
					if added, _ := renderedVolume(t, destination, identityTokenVolume); added != nil {
						t.Fatal("disjoint projection was not reused")
					}
				} else if err == nil || !strings.Contains(err.Error(), "unbounded") {
					t.Fatalf("unbounded projection was reused: %v", err)
				}
			})
		}
	}
}
