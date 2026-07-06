package self

import "testing"

// The --with-agents path forwards -j/--jobs into BuildOptions.Jobs for parity
// with `agent build --all`, so the flag must exist with the same shorthand and
// a default of 0 (which BuildAllAgents interprets as runtime.NumCPU()).
func TestBuildCmdJobsFlag(t *testing.T) {
	f := BuildCmd.Flags().Lookup("jobs")
	if f == nil {
		t.Fatal("self build is missing the --jobs flag")
	}
	if f.Shorthand != "j" {
		t.Errorf("--jobs shorthand = %q, want \"j\"", f.Shorthand)
	}
	if f.DefValue != "0" {
		t.Errorf("--jobs default = %q, want \"0\" (NumCPU)", f.DefValue)
	}

	jobs, err := BuildCmd.Flags().GetInt("jobs")
	if err != nil {
		t.Fatalf("GetInt(jobs): %v", err)
	}
	if jobs != 0 {
		t.Errorf("default jobs = %d, want 0", jobs)
	}
}
