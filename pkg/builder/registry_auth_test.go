package builder

import "testing"

func TestACRName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"acmestaging.azurecr.io", "acmestaging"},
		{"acmestaging.azurecr.io/team/app:tag", "acmestaging"},
		{"acmestaging.azurecr.io:443/team/app", "acmestaging"},
		{"https://acmestaging.azurecr.io/team/app", "acmestaging"},
		{"http://acmestaging.azurecr.io", "acmestaging"},
		{"123abc.dkr.ecr.us-east-1.amazonaws.com", ""},
		{"docker.io/library/nginx", ""},
		// Typo'd hosts must be rejected, not loosely matched: the trailing
		// boundary stops <name>.azurecr.io from matching inside a longer host.
		{"acmestaging.azurecr.io.evil.com", ""},
		{"acmestaging.azurecr.iox.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := acrName(c.url); got != c.want {
			t.Errorf("acrName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
