package builder

import "testing"

func TestACRName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"obinstaging.azurecr.io", "obinstaging"},
		{"obinstaging.azurecr.io/team/app:tag", "obinstaging"},
		{"https://obinstaging.azurecr.io/team/app", "obinstaging"},
		{"http://obinstaging.azurecr.io", "obinstaging"},
		{"123abc.dkr.ecr.us-east-1.amazonaws.com", ""},
		{"docker.io/library/nginx", ""},
		{"obinstaging.azurecr.io.evil.com", "obinstaging"},
		{"", ""},
	}
	for _, c := range cases {
		if got := acrName(c.url); got != c.want {
			t.Errorf("acrName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
