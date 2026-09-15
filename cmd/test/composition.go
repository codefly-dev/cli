package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/cli"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// runContributedCompositionTests runs every composition test the workspace's
// composed modules contribute. A module that composes a package declares them
// under contributions.tests, and they ran only as a step inside
// composition.Renderer.Render — so a module could ship a test asserting
// something about any solution composing it ("a solution registered with me is
// reachable through my gateway") with no way for that solution to run it.
//
// A workspace whose modules contribute nothing is silent: the solution's own
// entry tests are the subject of the command, and announcing the absence of
// contributed suites on every run would bury them.
func runContributedCompositionTests(ctx context.Context, workspace *resources.Workspace) error {
	suites, err := contributedSuites(ctx, workspace)
	if err != nil {
		return err
	}
	if len(suites) == 0 {
		return nil
	}

	runner := composition.ExecCommandRunner{}
	var failures []error
	for _, suite := range suites {
		cli.Header(1, "Composition test %s contributed by %s", suite.spec.Name, suite.module)
		// Every suite runs even after one fails: stopping at the first would hide
		// which of the composed modules are actually broken, which is the whole
		// question a test command answers.
		if runErr := runner.Run(ctx, suite.spec); runErr != nil {
			failures = append(failures, fmt.Errorf("%s contributed by %s: %w", suite.spec.Name, suite.module, runErr))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("composition tests failed: %w", errors.Join(failures...))
	}
	cli.Header(1, "Composition tests passed: %d contributed by the modules %s composes", len(suites), workspace.Name)
	return nil
}

// contributedSuite is one composition test, paired with the module shipping it
// so a failure names who owns it.
type contributedSuite struct {
	module string
	spec   composition.CommandSpec
}

// contributedSuites collects every composition test contributed by a module the
// workspace composes, in workspace declaration order. Each runs in the
// contributing module's own checkout at the declared path — the directory
// core's renderer runs it in.
//
// A module that composes no package contributes nothing — most modules compose
// none — so it is skipped. A descriptor that exists but does not load is not:
// that is a defect in the module, and swallowing it would report "no tests" for
// a module that ships them.
func contributedSuites(ctx context.Context, workspace *resources.Workspace) ([]contributedSuite, error) {
	var suites []contributedSuite
	for _, ref := range workspace.Modules {
		dir, err := clicomposition.ResolveComposedModuleDir(ctx, workspace, ref)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve module %q: %w", ref.Name, err)
		}
		composed, err := composesPackage(dir)
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", ref.Name, err)
		}
		if !composed {
			continue
		}
		descriptor, err := composition.LoadDescriptor(dir)
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", ref.Name, err)
		}
		for _, contribution := range descriptor.Contributions.Tests {
			suites = append(suites, contributedSuite{
				module: ref.Name,
				spec: composition.CommandSpec{
					Name:      contribution.Path,
					Command:   contribution.Command,
					Directory: filepath.Join(dir, filepath.FromSlash(contribution.Path)),
				},
			})
		}
	}
	return suites, nil
}

// composesPackage reports whether dir holds a composition descriptor.
//
// A composition descriptor and an ordinary module's resource configuration
// share the file name module.codefly.yaml and are told apart by their kind, so
// reading the file is not enough to know which one it is: decoding an ordinary
// module as a descriptor fails on its unknown fields, which would turn every
// workspace of plain modules into an error. A file that cannot be parsed at all
// is still reported — that is a broken module, not an absent descriptor.
func composesPackage(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, composition.DescriptorFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var header struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return false, fmt.Errorf("cannot parse %s: %w", composition.DescriptorFileName, err)
	}
	return header.Kind == composition.DescriptorKind, nil
}
