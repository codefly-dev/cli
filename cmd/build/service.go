package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/imageevidence"
	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the build command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Build a service container image for a target environment",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}

		flow, err := initBuildService(ctx, workspace, module, service, standAlone)
		if err != nil {
			return fmt.Errorf("cannot initialize service: %w", err)
		}
		reportDigest := func() {
			switch digest := flow.OriginImageDigest(); {
			case digest != "":
				cli.Info("Image digest %s", digest)
			case push:
				cli.Warning("Pushed %s but could not resolve its image digest", service.Name)
			}
		}
		cleaned := false
		cleanup := func() error {
			if cleaned {
				return nil
			}
			cleaned = true
			return cleanBuildService(flow)
		}
		defer func() { _ = cleanup() }()

		buildErr := common.WithHeartbeat(ctx, "building "+service.Name, func() error {
			return buildService(ctx, flow)
		})
		stopErr := cleanup()
		var result []error
		if buildErr != nil {
			result = append(result, fmt.Errorf("service build failed: %w", buildErr))
		}
		if stopErr != nil {
			result = append(result, fmt.Errorf("cannot stop flow: %w", stopErr))
		}
		if len(result) > 0 {
			return errors.Join(result...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publishImageEvidence(workspace, flow); err != nil {
			return err
		}
		reportDigest()
		cli.Header(1, "Work done!")
		return nil
	},
}

func initBuildService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("buildService", wool.ThisField(resources.WithUnique(service)))

	// Resolve the target environment so we can pick up its registry,
	// namespace, etc.
	env, err := orchestration.SelectEnvironment(workspace, envInput)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Image-registry resolution order:
	//   1. --org flag (explicit override; wins everything).
	//   2. env.Registry.URL declared in workspace.codefly.yaml.
	//   3. legacy default (the existing hardcoded ECR URL, kept for
	//      back-compat until every workspace declares an env).
	var registryURL string
	switch {
	case org != "":
		registryURL = org
	case env.Registry != nil && env.Registry.URL != "":
		registryURL = env.Registry.URL
	}
	if registryURL != "" {
		builder.SetRepository(registryURL)
	}

	if push {
		// Authenticate against the registry before the build runs.
		// Skip when we don't have a registry to push to (anonymous
		// build), or when the env doesn't declare an Auth method
		// (assume pre-existing docker creds in ~/.docker/config.json).
		if registryURL != "" && env.Registry != nil && env.Registry.Auth != "" {
			if err := builder.RegistryLogin(ctx, registryURL, env.Registry.Auth); err != nil {
				return nil, w.Wrapf(err, "registry login failed")
			}
		}
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.BuildMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	cache, err := buildCacheFlags.Policy()
	if err != nil {
		return nil, err
	}
	flow.WithBuildCache(cache)
	flow.WithPush(push)
	flow.WithBuildxBuilder(buildxBuilder)
	flow.WithImageDigest(true)
	flow.WithImageSBOM(imageSBOM)
	flow.WithOutputSink(cli.NewOutputSink())
	flow.WithStandAlone(standAlone)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func cleanBuildService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func buildService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("buildService")
	err := flow.Build(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

// publishImageEvidence writes the evidence this build collected into a
// directory that travels with the images it pushed, so published evidence is
// retrievable from the release itself rather than only from the run that
// produced it.
//
// Every service's evidence is published, not only the origin's: a build that is
// not stand-alone builds its dependencies, and a pushed one publishes their
// images too, so discarding their evidence would leave shipped images uncovered.
func publishImageEvidence(workspace *resources.Workspace, flow *orchestration.Flow) error {
	return publishCollectedImageEvidence(workspace, "", flow.ImageEvidence())
}

// publishCollectedImageEvidence publishes evidence keyed by service as one
// directory with one index. A module builds its services through separate flows,
// and publishing each flow on its own would leave an index describing only the
// service published last.
//
// It owns the opt-in gate so that every caller honours it, rather than each call
// site repeating a check one of them can forget.
//
// scope, when set, is a subdirectory of the configured directory. Publishing
// removes generated documents the new index no longer names, so two modules
// publishing into one directory would delete each other's evidence.
func publishCollectedImageEvidence(workspace *resources.Workspace, scope string, collected map[string][]*builderv0.ImageSBOM) error {
	if !imageSBOM {
		return nil
	}
	uniques := make([]string, 0, len(collected))
	for unique := range collected {
		uniques = append(uniques, unique)
	}
	sort.Strings(uniques)
	var evidence []*builderv0.ImageSBOM
	for _, unique := range uniques {
		evidence = append(evidence, collected[unique]...)
	}
	documents, err := imageevidence.Documents(evidence)
	if err != nil {
		return err
	}
	directory := imageSBOMDirectory
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(workspace.Dir(), directory)
	}
	if scope != "" {
		directory = filepath.Join(directory, scope)
	}
	index, err := imageevidence.Publish(directory, documents, push)
	if err != nil {
		return err
	}
	if len(index.Images) == 0 {
		// Recorded rather than skipped: an absent directory cannot be told apart
		// from a build that never collected, while an empty index states that
		// this build owed evidence for no image.
		cli.Info("No image to cover; recorded empty image SBOM evidence at %s", directory)
		return nil
	}
	if !push {
		cli.Warning("Image SBOM evidence at %s covers locally loaded images; their digests are not in a registry", directory)
	}
	cli.Info("Published image SBOM evidence for %d image(s) to %s", len(index.Images), directory)
	return nil
}

var buildCacheFlags common.BuildCacheFlags

var standAlone bool
var org string
var push bool
var envInput string
var buildxBuilder string
var imageSBOM bool
var imageSBOMDirectory string

func init() {
	buildCacheFlags.Bind(ServiceCmd)
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&org, "org", "", "Image registry override (e.g. ghcr.io/myorg). Wins over the env's registry.url.")
	ServiceCmd.Flags().BoolVar(&push, "push", false, "Push the image to the repository")
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to build for (looks up registry/cluster from workspace.codefly.yaml)")
	ServiceCmd.Flags().StringVar(&buildxBuilder, "builder", "", "docker buildx builder to run the build on (e.g. a native amd64 buildkit) to avoid local QEMU emulation")
	ServiceCmd.Flags().BoolVar(&imageSBOM, "image-sbom", false, "Require digest-bound image SBOM evidence for every image the build produces")
	ServiceCmd.Flags().StringVar(&imageSBOMDirectory, "image-sbom-dir", filepath.Join(".codefly", "sbom", "image"), "Directory to publish image SBOM evidence into")
}
