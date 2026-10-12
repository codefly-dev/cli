package environments

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The classification is the rule: a declared host resolves to an address, to a
// public name, or to nothing — and nothing is a refusal. These cases are the
// shapes the rule has to get right, not a list it recognises.
func TestResolveOriginClassifiesEveryDeclaration(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     string
		reason   string
	}{
		// Accepted, and canonical.
		{declared: "app.example.com", want: "https://app.example.com"},
		{declared: "APP.Example.COM", want: "https://app.example.com"},
		{declared: "https://app.example.com", want: "https://app.example.com"},
		{declared: "https://app.example.com/", want: "https://app.example.com"},
		{declared: "https://app.example.com:443", want: "https://app.example.com"},
		{declared: "http://app.example.com:80", want: "http://app.example.com"},
		{declared: "app.example.com:8443", want: "https://app.example.com:8443"},
		{declared: "http://app.example.com", want: "http://app.example.com"},
		{declared: "  app.example.com  ", want: "https://app.example.com"},
		{declared: "203.0.113.7", want: "https://203.0.113.7"},
		{declared: "[2001:db8::1]", want: "https://[2001:db8::1]"},
		{declared: "[::ffff:203.0.113.7]", want: "https://203.0.113.7"},

		// A trailing dot is the DNS root. One is dropped; the name underneath is
		// then judged like any other, so a local name stays local.
		{declared: "app.example.com.", want: "https://app.example.com"},
		{declared: "localhost.", want: "https://localhost"},
		{declared: "APP.LOCALHOST.", want: "https://app.localhost"},
		{declared: "app.example.com..", reason: originUnresolvable},

		// Ends in a number, so it is an address — in every form a resolver takes.
		// These resolve: whether an address is a PUBLIC origin is the next
		// question, asked by TestRequirePublicOrigin.
		{declared: "127.1", want: "https://127.0.0.1"},
		{declared: "2130706433", want: "https://127.0.0.1"},
		{declared: "0x7f000001", want: "https://127.0.0.1"},
		{declared: "0177.0.0.1", want: "https://127.0.0.1"},
		{declared: "127.0.0.1", want: "https://127.0.0.1"},
		{declared: "0", want: "https://0.0.0.0"},
		{declared: "[::1]", want: "https://[::1]"},
		{declared: "[::1%25lo0]", want: "https://[::1]"},
		{declared: "[::ffff:127.0.0.1]", want: "https://127.0.0.1"},
		{declared: "0.0.0.0", want: "https://0.0.0.0"},
		{declared: "[::]", want: "https://[::]"},
		{declared: "169.254.0.1", want: "https://169.254.0.1"},
		{declared: "[fe80::1]", want: "https://[fe80::1]"},
		{declared: "[fe80::1%25eth0]", want: "https://[fe80::1]"},
		{declared: "239.0.0.1", want: "https://239.0.0.1"},
		{declared: "localhost", want: "https://localhost"},
		{declared: "app.localhost", want: "https://app.localhost"},

		// Not an origin at all.
		{declared: "", reason: originEmpty},
		{declared: "   ", reason: originEmpty},
		{declared: "https://", reason: originNoHost},
		{declared: "ftp://app.example.com", reason: originNotWeb},
		{declared: "https://app.example.com/path", reason: originNotBare},
		{declared: "app.example.com?x=1", reason: originNotBare},
		{declared: "https://app.example.com#f", reason: originNotBare},
		{declared: "https://app.example.com:0", reason: originBadPort},
		{declared: "https://app.example.com:https", reason: originUnresolvable},

		// Neither an address nor a public name.
		{declared: "example", reason: originUnresolvable},
		{declared: "-app.example.com", reason: originUnresolvable},
		{declared: "app-.example.com", reason: originUnresolvable},
		{declared: "app_x.example.com", reason: originUnresolvable},
		{declared: "app..example.com", reason: originUnresolvable},
		{declared: "app.exämple.com", reason: originUnresolvable},
		{declared: strings.Repeat("a", 64) + ".example.com", reason: originUnresolvable},
		{declared: "1.2.3.4.5", reason: originUnresolvable},
		{declared: "256.0.0.1", reason: originUnresolvable},
		{declared: "0x1g", reason: originUnresolvable},
		{declared: "08.0.0.1", reason: originUnresolvable},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			got, err := ResolveOrigin(tc.declared)
			if tc.reason != "" {
				require.Error(t, err)
				require.Equal(t, tc.reason, OriginRefusalReason(err), "the refusal must name what is wrong")
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// The second question, asked only where it matters: is a well-formed origin one
// something outside the workload reaches? A developer's own cluster IS reached at
// a loopback origin, so this is a cell's question and not a declaration's.
func TestRequirePublicOrigin(t *testing.T) {
	for _, origin := range []string{
		"https://app.example.com", "https://203.0.113.7", "https://[2001:db8::1]",
		"https://app.example.com:8443", "http://app.example.com",
	} {
		require.NoError(t, RequirePublicOrigin(origin), origin)
	}
	for origin, reason := range map[string]string{
		"https://localhost":     originLocalName,
		"https://app.localhost": originLocalName,
		"https://127.0.0.1":     originNotReachable,
		"https://0.0.0.0":       originNotReachable,
		"https://[::1]":         originNotReachable,
		"https://[::]":          originNotReachable,
		"https://169.254.0.1":   originNotReachable,
		"https://[fe80::1]":     originNotReachable,
		"https://239.0.0.1":     originNotReachable,
	} {
		require.Equal(t, reason, OriginRefusalReason(RequirePublicOrigin(origin)), origin)
	}
}

// A declaration is operator input and can carry a credential, so the refusal is
// decided before any message exists and the message never repeats the value.
func TestResolveOriginNeverRepeatsTheDeclaration(t *testing.T) {
	marker := "synthetic-sensitive-marker"
	for _, declared := range []string{
		"https://reader:" + marker + "@localhost",
		"https://" + marker + "@app.example.com",
		"https://app." + marker + ".com/" + marker + "?q=" + marker,
		"ftp://" + marker + ".example.com",
		marker + "_not_a_host",
	} {
		_, err := ResolveOrigin(declared)
		require.Error(t, err, declared)
		require.NotContains(t, err.Error(), marker, "a refusal repeated the declared value")
	}

	// Userinfo is refused as userinfo, not as whatever the host happens to be.
	_, err := ResolveOrigin("https://reader:" + marker + "@app.example.com")
	require.Equal(t, originHasCredential, OriginRefusalReason(err))
}

// A local environment declaring the hosts a developer's cluster answers at must
// validate: that is the truth about it, and Validate runs on every path that
// reads an environment, including a local run.
func TestLocalIngressDeclarationValidates(t *testing.T) {
	env := &Environment{
		Name:      "local",
		Namespace: "platform",
		Cluster:   &EnvironmentCluster{Kind: ClusterKindK3d},
		Ingress: []EnvironmentIngressRoute{
			{Name: "marketing", Service: "marketing", Endpoint: "http", Hosts: []string{"shop.localhost", "www.shop.localhost"}},
			{Name: "product", Service: "frontend", Endpoint: "http", Hosts: []string{"app.shop.localhost", "localhost"}},
		},
	}
	require.NoError(t, env.Validate())
	require.NoError(t, env.ValidateIngress())
}
