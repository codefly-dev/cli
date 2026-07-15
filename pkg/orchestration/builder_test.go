package orchestration

import (
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestBuildResultKindAssertionIsSafeForNonDockerResults(t *testing.T) {
	if got := dockerBuildResult(nil); got != nil {
		t.Fatalf("nil result returned %#v", got)
	}
	if got := dockerBuildResult(&builderv0.BuildResult{}); got != nil {
		t.Fatalf("empty result returned %#v", got)
	}
	want := &builderv0.DockerBuildResult{Images: []string{"example:test"}}
	result := &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildResult{DockerBuildResult: want}}
	if got := dockerBuildResult(result); got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
