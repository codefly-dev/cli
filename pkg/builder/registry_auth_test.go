package builder

import "testing"

func TestACRName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"obinstaging.azurecr.io", "obinstaging"},
		{"obinstaging.azurecr.io/team/app:tag", "obinstaging"},
		{"obinstaging.azurecr.io:443/team/app", "obinstaging"},
		{"https://obinstaging.azurecr.io/team/app", "obinstaging"},
		{"http://obinstaging.azurecr.io", "obinstaging"},
		{"123abc.dkr.ecr.us-east-1.amazonaws.com", ""},
		{"docker.io/library/nginx", ""},
		// Typo'd hosts must be rejected, not loosely matched: the trailing
		// boundary stops <name>.azurecr.io from matching inside a longer host.
		{"obinstaging.azurecr.io.evil.com", ""},
		{"obinstaging.azurecr.iox.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := acrName(c.url); got != c.want {
			t.Errorf("acrName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
