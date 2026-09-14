package orchestration

import (
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func multiPlatformPlan() *builderv0.DockerBuildPlan {
	return &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{
		{Name: "app", Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64", "linux/arm64"}},
		{Name: "migration", Image: "ghcr.io/org/migration:v1", Platforms: []string{"linux/amd64"}},
	}}
}

func TestProducedSubjectsCoverEveryPlatformOfAPushedBuild(t *testing.T) {
	subjects := producedSubjects("app/api", multiPlatformPlan(), true)

	require.Len(t, subjects, 3, "a push writes every declared platform into one manifest list")
	platforms := map[string][]string{}
	for _, subject := range subjects {
		platforms[subject.GetRole()] = append(platforms[subject.GetRole()], subject.GetPlatform())
	}
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, platforms["app"])
	require.Equal(t, []string{"linux/amd64"}, platforms["migration"])
}

// A local build loads one platform per recipe. Expecting the declared set here
// demanded evidence for an image buildx was never asked to build, so every
// multi-platform recipe failed its own build.
func TestProducedSubjectsCoverOnlyTheLoadedPlatformOfALocalBuild(t *testing.T) {
	subjects := producedSubjects("app/api", multiPlatformPlan(), false)

	require.Len(t, subjects, 2, "only the first platform of each recipe is loaded")
	for _, subject := range subjects {
		require.Equal(t, "linux/amd64", subject.GetPlatform(), "role %s", subject.GetRole())
	}
	roles := []string{subjects[0].GetRole(), subjects[1].GetRole()}
	require.ElementsMatch(t, []string{"app", "migration"}, roles, "every recipe still owes evidence")
}

// A recipe that states no platform is built for the host's own, and the plan
// says nothing about which that is, so the subject survives either way.
func TestProducedSubjectsKeepARecipeThatStatesNoPlatform(t *testing.T) {
	plan := &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{
		{Name: "app", Image: "ghcr.io/org/app:v1"},
	}}

	require.Len(t, producedSubjects("app/api", plan, false), 1)
	require.Len(t, producedSubjects("app/api", plan, true), 1)
}
