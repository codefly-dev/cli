package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/prerelease"
	"github.com/spf13/cobra"
)

// Report formats the gate renders. Named because goconst counts "json" across
// this package and a bare literal in a fourth place is one too many.
const (
	prereleaseFormatText = "text"
	prereleaseFormatJSON = "json"
)

// PrereleaseCmd is the merge gate for version pins. It is a sibling of the other
// `codefly ci` gates rather than a new top-level mechanism, because the
// repositories that need it are the module and composition repositories that
// already call `codefly ci …` on every pull request.
//
// Deliberately not a stage of `codefly agent ci`: an agent repository's own
// manifest version is a prerelease exactly while `codefly publish dev` is doing
// its job, so a gate there would refuse the dev loop this one is built to
// preserve.
var PrereleaseCmd = &cobra.Command{
	Use:   "prerelease",
	Short: "Refuse a prerelease version pin, so one never reaches the default branch or a release",
	Long: `Fail when a committed version pin names a build that was never released.

A prerelease version — a semver prerelease component (0.1.48-dev.e87db5e08865,
-rc.1, -alpha) or a Go pseudo-version (v0.0.0-20260930123456-abcdef123456, the
same defect spelled differently) — belongs on a branch or in a gitignored
codefly.local.yaml, never on the default branch, and therefore never inside a
released tag. Detection is the shape of the version, never the literal "dev".

What is read, in files git tracks and nothing else:

  *.codefly.yaml          every version: key at any depth — a service's
                          agent.version, a workspace's modules[].version and
                          solutions[].version, a module's or library's own
                          version
  workspace.codefly.yaml  the top-level agent-overrides block, whose values are
                          versions under an agent identity
  go.mod                  first-party requires (codefly-dev/*, obin-ai/*),
                          reported but not refused unless --go-modules

testdata/ trees are skipped, because a fixture's job can be to carry a bad pin;
--include-testdata reads them.

The one exception is agent-overrides, the sanctioned prerelease carrier a dev
loop publishes into with codefly publish dev and codefly update workspace
--agent-override. A prerelease is allowed there on the default branch when the
entry carries a label — a comment naming the issue it stands in for — and is
refused under --release, which is the scope a tag is cut in. That is what
guarantees the property a released tag has to have.

Needs no workspace, no agent and no network: it reads committed text, so it runs
in a fresh clone in milliseconds.`,
	Example: `  # On every pull request
  codefly ci prerelease

  # Before cutting a tag: no exception, not even a labelled dev override
  codefly ci prerelease --release

  # Also refuse first-party Go pseudo-versions
  codefly ci prerelease --go-modules

  # A repository elsewhere, machine-readable
  codefly ci prerelease --dir ../module-runtime --format json`,
	Args: cobra.NoArgs,
	RunE: runPrereleaseCommand,
}

func runPrereleaseCommand(cmd *cobra.Command, _ []string) error {
	dir, err := cmd.Flags().GetString("dir")
	if err != nil {
		return err
	}
	if strings.TrimSpace(dir) == "" {
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve directory: %w", err)
	}
	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format != prereleaseFormatText && format != prereleaseFormatJSON {
		return fmt.Errorf("unsupported format %q (use %s or %s)", format, prereleaseFormatText, prereleaseFormatJSON)
	}
	release, err := cmd.Flags().GetBool("release")
	if err != nil {
		return err
	}
	goModules, err := cmd.Flags().GetBool("go-modules")
	if err != nil {
		return err
	}
	firstParty, err := cmd.Flags().GetStringSlice("first-party")
	if err != nil {
		return err
	}
	includeTestdata, err := cmd.Flags().GetBool("include-testdata")
	if err != nil {
		return err
	}

	result, err := prerelease.Scan(dir, prerelease.Options{
		Release:         release,
		GoModules:       goModules,
		FirstParty:      firstParty,
		IncludeTestdata: includeTestdata,
	})
	if err != nil {
		return err
	}

	if format == prereleaseFormatJSON {
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		payload, marshalErr := result.JSON()
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := cmd.OutOrStdout().Write(payload); err != nil {
			return err
		}
		if failure := result.Failure(); failure != nil {
			return machineReadablePrereleaseError{failure}
		}
		return nil
	}

	cli.Header(1, "Codefly prerelease gate (%s scope)", result.Scope())
	if !result.Tracked {
		// Said out loud rather than silently widened: outside a git work tree the
		// scan cannot tell a committed file from a local one, so a finding may be
		// about something that never reaches the default branch.
		cli.Warning("%s is not a git work tree, so the scan walked it instead of reading what git tracks; a gitignored file may have been read", dir)
	}
	// The report is this command's output, not a diagnostic, and it is printed
	// whether the scan passed or failed. Returning it as one multi-line error
	// instead would send it through the error-chain renderer, which pads every
	// line of a block to the width of the longest one.
	fmt.Fprintln(cmd.OutOrStdout(), result.Report())
	if err := result.Err(); err != nil {
		// The detail is above; the root only needs the one line and the exit code.
		cmd.SilenceUsage = true
		return err
	}
	return nil
}

type machineReadablePrereleaseError struct{ error }

func (machineReadablePrereleaseError) MachineReadable() bool { return true }

func init() {
	PrereleaseCmd.Flags().String("dir", "", "Repository directory to scan (default: current directory)")
	PrereleaseCmd.Flags().String("format", prereleaseFormatText, "Report format: text or json")
	PrereleaseCmd.Flags().Bool("release", false, "Release scope: refuse a prerelease in agent-overrides too, so a tag cannot be cut over a dev override")
	PrereleaseCmd.Flags().Bool("go-modules", false, "Also refuse first-party Go pseudo-versions in go.mod, instead of only reporting them")
	PrereleaseCmd.Flags().StringSlice("first-party", prerelease.DefaultFirstParty, "Go module path prefixes treated as first-party")
	PrereleaseCmd.Flags().Bool("include-testdata", false, "Also scan testdata/ trees, whose fixtures often carry a bad pin on purpose")
}
