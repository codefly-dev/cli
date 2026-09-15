package orchestration

import (
	"testing"

	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

const (
	pushedIndexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	loadedImageID     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func multiPlatformPlan() *builderv0.DockerBuildPlan {
	return &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{
		{Name: "app", Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64", "linux/arm64"}},
		{Name: "migration", Image: "ghcr.io/org/migration:v1", Platforms: []string{"linux/amd64"}},
	}}
}

// A push writes every platform into one manifest list, so each of them resolves
// to that index digest.
func pushedImages() []coresbom.ResolvedImage {
	return []coresbom.ResolvedImage{
		{Recipe: "app", Platform: "linux/amd64", Digest: pushedIndexDigest},
		{Recipe: "app", Platform: "linux/arm64", Digest: pushedIndexDigest},
		{Recipe: "migration", Platform: "linux/amd64", Digest: pushedIndexDigest},
	}
}

func loadedImages() []coresbom.ResolvedImage {
	return []coresbom.ResolvedImage{
		{Recipe: "app", Platform: "linux/amd64", Digest: loadedImageID, Source: coresbom.SourceDockerDaemon},
		{Recipe: "migration", Platform: "linux/amd64", Digest: loadedImageID, Source: coresbom.SourceDockerDaemon},
	}
}

func TestProducedSubjectsCoverEveryPlatformOfAPushedBuild(t *testing.T) {
	subjects, err := producedSubjects("app/api", multiPlatformPlan(), true, pushedImages())
	require.NoError(t, err)

	require.Len(t, subjects, 3, "a push writes every declared platform into one manifest list")
	platforms := map[string][]string{}
	for _, subject := range subjects {
		platforms[subject.GetRole()] = append(platforms[subject.GetRole()], subject.GetPlatform())
	}
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, platforms["app"])
	require.Equal(t, []string{"linux/amd64"}, platforms["migration"])
	require.Equal(t, "ghcr.io/org/app@"+pushedIndexDigest, subjects[0].GetReference(),
		"a subject carrying only the recipe's tag binds evidence to whatever that tag serves")
}

// A local build loads one platform per recipe. Expecting the declared set here
// demanded evidence for an image buildx was never asked to build, so every
// multi-platform recipe failed its own build.
func TestProducedSubjectsCoverOnlyTheLoadedPlatformOfALocalBuild(t *testing.T) {
	subjects, err := producedSubjects("app/api", multiPlatformPlan(), false, loadedImages())
	require.NoError(t, err)

	require.Len(t, subjects, 2, "only the first platform of each recipe is loaded")
	for _, subject := range subjects {
		require.Equal(t, "linux/amd64", subject.GetPlatform(), "role %s", subject.GetRole())
	}
	roles := []string{subjects[0].GetRole(), subjects[1].GetRole()}
	require.ElementsMatch(t, []string{"app", "migration"}, roles, "every recipe still owes evidence")

	// A never-pushed image has no registry manifest, so it keeps the tag the
	// daemon knows and binds to the local image ID instead.
	require.Equal(t, "ghcr.io/org/app:v1", subjects[0].GetReference())
	require.Equal(t, loadedImageID, subjects[0].GetDigest())
}

// A recipe that states no platform is built for the host's own, and the plan
// says nothing about which that is, so the subject survives either way.
func TestProducedSubjectsKeepARecipeThatStatesNoPlatform(t *testing.T) {
	plan := &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{
		{Name: "app", Image: "ghcr.io/org/app:v1"},
	}}

	local, err := producedSubjects("app/api", plan, false,
		[]coresbom.ResolvedImage{{Recipe: "app", Digest: loadedImageID, Source: coresbom.SourceDockerDaemon}})
	require.NoError(t, err)
	require.Len(t, local, 1)

	pushed, err := producedSubjects("app/api", plan, true,
		[]coresbom.ResolvedImage{{Recipe: "app", Digest: pushedIndexDigest}})
	require.NoError(t, err)
	require.Len(t, pushed, 1)
}

// Evidence is owed against the image the build produced, which only the digest
// the build resolved can name. Deriving a subject for a recipe nothing resolved
// would ask the agent to scan a floating tag and accept the answer as coverage.
func TestProducedSubjectsRefuseARecipeTheBuildResolvedNothingFor(t *testing.T) {
	_, err := producedSubjects("app/api", multiPlatformPlan(), true, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "app")
}

// The plan belongs to the agent that emitted it and is recorded as the durable
// build recipe; narrowing it to the loaded platform must not edit it in place.
func TestProducedSubjectsLeaveTheEmittedPlanUntouched(t *testing.T) {
	plan := multiPlatformPlan()

	_, err := producedSubjects("app/api", plan, false, loadedImages())
	require.NoError(t, err)

	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, plan.GetRecipes()[0].GetPlatforms())
}
