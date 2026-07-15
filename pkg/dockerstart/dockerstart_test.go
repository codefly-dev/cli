package dockerstart

import "testing"

func fakeProvider(name string, installed bool, hints ...string) provider {
	return provider{
		name:         name,
		contextHints: hints,
		detect:       func() bool { return installed },
	}
}

func TestSelectFrom(t *testing.T) {
	orbstack := fakeProvider("OrbStack", true, "orbstack")
	desktop := fakeProvider("Docker Desktop", true, "desktop-linux", "default")
	colima := fakeProvider("colima", true, "colima")

	cases := []struct {
		name    string
		provs   []provider
		context string
		want    string // "" == nil
	}{
		{"active context wins over preference order", []provider{orbstack, desktop, colima}, "colima", "colima"},
		{"no context → first installed in preference order", []provider{orbstack, desktop, colima}, "", "OrbStack"},
		{"context naming an uninstalled provider falls through to first installed",
			[]provider{fakeProvider("colima", false, "colima"), desktop}, "colima", "Docker Desktop"},
		{"none installed → nil", []provider{fakeProvider("OrbStack", false, "orbstack")}, "", ""},
		{"unknown context → first installed", []provider{orbstack, desktop}, "some-remote", "OrbStack"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := selectFrom(c.provs, c.context)
			if c.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %q", got.name)
				}
				return
			}
			if got == nil || got.name != c.want {
				t.Fatalf("want %q, got %v", c.want, got)
			}
		})
	}
}
