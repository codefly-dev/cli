package status

import "testing"

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		owner, repo string
	}{
		{"https", "https://github.com/codefly-dev/service-minio", "codefly-dev", "service-minio"},
		{"https .git", "https://github.com/codefly-dev/service-minio.git", "codefly-dev", "service-minio"},
		{"ssh", "git@github.com:codefly-dev/service-s3.git", "codefly-dev", "service-s3"},
		{"non-github", "https://gitlab.com/codefly-dev/service-x.git", "", ""},
		{"garbage", "not-a-url", "", ""},
		{"owner only", "https://github.com/codefly-dev", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := parseGitHubRemote(tc.url)
			if owner != tc.owner || repo != tc.repo {
				t.Fatalf("parseGitHubRemote(%q) = (%q, %q), want (%q, %q)", tc.url, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}
