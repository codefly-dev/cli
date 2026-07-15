package list

import "testing"

func TestJobsCommandReturnsErrorsThroughCobra(t *testing.T) {
	if JobsCmd.RunE == nil || JobsCmd.Run != nil {
		t.Fatal("jobs command is not exclusively RunE")
	}
	if err := JobsCmd.Args(JobsCmd, []string{"extra"}); err == nil {
		t.Fatal("jobs command accepted a positional argument")
	}
}

func TestListJobsMissingWorkspaceReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := listJobs(); err == nil {
		t.Fatal("listJobs returned success without a workspace")
	}
}
