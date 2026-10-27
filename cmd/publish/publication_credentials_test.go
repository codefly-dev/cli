package publish

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

type publicationTransport func(*http.Request) (*http.Response, error)

func (f publicationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise both real afterPush branches, including the workflow tag/commit
// match. A gh login can publish a private release but must not certify that an
// installer without that credential can fetch it; public downloads need none.
func TestPublicationOwnersUseInstallerCredentials(t *testing.T) {
	for _, owner := range []string{publicationCLI, publicationWorkflow} {
		for _, scenario := range []string{"anonymous public", "private with only gh login", "GH_TOKEN", "GITHUB_TOKEN wins", "bad archive"} {
			t.Run(owner+"/"+scenario, func(t *testing.T) {
				t.Setenv("GITHUB_TOKEN", "")
				t.Setenv("GH_TOKEN", "")
				t.Setenv("GITHUB_API_URL", "")
				bin := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf publisher-login"), 0700))
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				expectedInstaller := ""
				if scenario == "GH_TOKEN" {
					t.Setenv("GH_TOKEN", "installer-gh")
					expectedInstaller = "Bearer installer-gh"
				}
				if scenario == "GITHUB_TOKEN wins" {
					t.Setenv("GH_TOKEN", "ignored-gh")
					t.Setenv("GITHUB_TOKEN", "installer-github")
					expectedInstaller = "Bearer installer-github"
				}
				reg, err := resources.AgentKindRegistrationFor(resources.AgentKind("codefly:service"))
				require.NoError(t, err)
				target := platform{os: "linux", arch: "amd64"}
				name := loaderArchiveName(&reg, "go", "0.0.16", target)
				assets := stageAssets(t, name)
				assets[0].platform = target
				files, err := stageReleaseChecksums(assets)
				require.NoError(t, err)
				payloads := make([]string, len(files))
				var entries []string
				for i, file := range files {
					raw, err := os.ReadFile(file)
					require.NoError(t, err)
					payloads[i] = string(raw)
					url := "https://github.com/codefly-dev/service-go/releases/download/v0.0.16/" + filepath.Base(file)
					entries = append(entries, fmt.Sprintf(`{"id":%d,"name":%q,"size":%d,"digest":%q,"browser_download_url":%q}`, i+1, filepath.Base(file), len(raw), digestOf(string(raw)), url))
				}
				dir, _, _ := releaseRepo(t, "0.0.16")
				gitIn(t, dir, "tag", "v0.0.16")
				head := gitIn(t, dir, "rev-parse", "HEAD")
				var installerSeen, publisherSeen bool
				original := http.DefaultTransport
				http.DefaultTransport = publicationTransport(func(req *http.Request) (*http.Response, error) {
					require.Equal(t, "api.github.com", req.URL.Host, "unexpected network call")
					require.Equal(t, http.MethodGet, req.Method, "complete release retry must not mutate")
					auth := req.Header.Get("Authorization")
					if auth == "Bearer publisher-login" {
						publisherSeen = true
					}
					if auth == expectedInstaller {
						installerSeen = true
					}
					status, contentType, body := http.StatusOK, "application/json", ""
					if scenario == "private with only gh login" && auth == "" {
						status, body = http.StatusNotFound, `{"message":"not found"}`
					} else {
						switch req.URL.Path {
						case "/repos/codefly-dev/service-go/releases/tags/v0.0.16":
							body = fmt.Sprintf(`{"id":42,"tag_name":"v0.0.16","assets":[%s]}`, strings.Join(entries, ","))
						case "/repos/codefly-dev/service-go/releases/assets/1":
							contentType, body = "application/octet-stream", payloads[0]
							if scenario == "bad archive" {
								body = "corrupted"
							}
						case "/repos/codefly-dev/service-go/releases/assets/2":
							contentType, body = "application/octet-stream", payloads[1]
						case "/repos/codefly-dev/service-go/actions/workflows/release.yml/runs":
							body = fmt.Sprintf(`{"workflow_runs":[{"id":1,"head_sha":%q,"head_branch":"v0.0.16","event":"push","status":"completed","conclusion":"success"}]}`, head)
						default:
							t.Fatalf("unexpected request: %s", req.URL)
						}
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})
				t.Cleanup(func() { http.DefaultTransport = original })
				r := &agentReleaser{agentDir: dir, reg: &reg, publisher: "codefly.dev", name: "go", assets: assets, publication: agentPublication{Owner: owner, Workflow: "release.yml"}}
				// afterPush detaches a caller timeout after a tag ships. For the
				// reachability refusal, call its shared publication verifier with
				// a short context so this regression never sleeps through retries.
				if scenario == "private with only gh login" {
					ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
					defer cancel()
					client, err := gh.NewClient()
					require.NoError(t, err)
					err = r.verifyPublication(ctx, client, "codefly-dev", "service-go", "v0.0.16")
					require.Error(t, err)
				} else {
					err = r.afterPush(t.Context(), "v0.0.16")
					if scenario == "bad archive" {
						require.ErrorIs(t, err, errAssetVerdict)
					} else {
						require.NoError(t, err)
					}
				}
				if expectedInstaller == "" {
					require.True(t, publisherSeen, "fixture must exercise gh login publication")
				}
				if scenario == "bad archive" {
					require.False(t, installerSeen, "invalid archive must fail before installer reachability")
				} else {
					require.True(t, installerSeen, "must check actual installer credential")
				}
			})
		}
	}
}
