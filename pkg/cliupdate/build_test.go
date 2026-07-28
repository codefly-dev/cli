package cliupdate

import "testing"

func TestBuildInfoReleased(t *testing.T) {
	tests := []struct {
		name string
		info BuildInfo
		want bool
	}{
		{
			name: "release",
			info: BuildInfo{Version: "v1.2.3", Commit: "abc123", BuildDate: "2026-07-28T16:23:31Z"},
			want: true,
		},
		{
			name: "development version",
			info: BuildInfo{Version: "development", Commit: "abc123", BuildDate: "2026-07-28T16:23:31Z"},
		},
		{
			name: "unknown commit",
			info: BuildInfo{Version: "1.2.3", Commit: "unknown", BuildDate: "2026-07-28T16:23:31Z"},
		},
		{
			name: "invalid date",
			info: BuildInfo{Version: "1.2.3", Commit: "abc123", BuildDate: "28 July 2026"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.info.Released(); got != test.want {
				t.Fatalf("Released() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCurrentBuildInfoTrimsInjectedValues(t *testing.T) {
	previousVersion, previousCommit, previousDate := version, commit, buildDate
	t.Cleanup(func() {
		version, commit, buildDate = previousVersion, previousCommit, previousDate
	})
	version = " v1.2.3 "
	commit = " abc123 "
	buildDate = " 2026-07-28T16:23:31Z "

	got := CurrentBuildInfo()
	want := BuildInfo{Version: "v1.2.3", Commit: "abc123", BuildDate: "2026-07-28T16:23:31Z"}
	if got != want {
		t.Fatalf("CurrentBuildInfo() = %#v, want %#v", got, want)
	}
}
