package cmd

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var updateReference = flag.Bool("update-reference", false, "rewrite docs/cli-reference.md from the command tree")

// referencePath is the generated reference every non-hidden command appears in.
const referencePath = "../docs/cli-reference.md"

// TestCLIReferenceIsCurrent is the completeness guard for the command
// documentation. docs/commands.md is narrative — it explains the chains a verb
// takes part in, and it covers what its authors chose to cover. That is the
// wrong shape for "is every command documented", which is why 94 commands and 83
// flags had drifted out of it unnoticed.
//
// This reference is generated from the command tree instead, so a command or a
// flag cannot exist without appearing in it, and the text is the same Short,
// Long and flag usage `--help` prints rather than a second description that can
// disagree with the first. Adding a verb or a flag and not regenerating fails
// here, naming the command.
//
// Regenerate with:
//
//	go test ./cmd -run TestCLIReferenceIsCurrent -update-reference
func TestCLIReferenceIsCurrent(t *testing.T) {
	generated := renderCLIReference(RootCmd)
	if *updateReference {
		if err := os.WriteFile(filepath.Clean(referencePath), []byte(generated), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote " + referencePath)
		return
	}
	current, err := os.ReadFile(filepath.Clean(referencePath))
	if err != nil {
		t.Fatalf("%v — regenerate with `go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`", err)
	}
	if string(current) == generated {
		return
	}
	// Name what moved, so the failure is actionable without diffing 200 sections.
	for _, line := range commandHeadings(generated) {
		if !strings.Contains(string(current), line) {
			t.Errorf("%s is not in %s", strings.TrimPrefix(line, "## "), referencePath)
		}
	}
	for _, line := range commandHeadings(string(current)) {
		if !strings.Contains(generated, line) {
			t.Errorf("%s is in %s but no longer exists", strings.TrimPrefix(line, "## "), referencePath)
		}
	}
	t.Fatalf("%s is out of date — regenerate with `go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`", referencePath)
}

func commandHeadings(reference string) []string {
	var headings []string
	for _, line := range strings.Split(reference, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
		}
	}
	return headings
}

// renderCLIReference walks the command tree into one deterministic document.
// Every string in it comes from the command's own definition, so the reference
// and `--help` cannot disagree.
func renderCLIReference(root *cobra.Command) string {
	var out strings.Builder
	out.WriteString("# codefly command reference\n\n")
	out.WriteString("Generated from the command tree by\n")
	out.WriteString("`go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`. Do not edit by hand:\n")
	out.WriteString("a command's text lives on the command, so `--help` and this page always agree.\n\n")
	out.WriteString("This page is the complete list. [commands.md](commands.md) is the narrative guide —\n")
	out.WriteString("what a verb is for, and the chains it takes part in.\n")
	renderCommand(&out, root, "")
	return out.String()
}

func renderCommand(out *strings.Builder, command *cobra.Command, path string) {
	full := strings.TrimSpace(path + " " + command.Name())
	// Cobra's generated `help` command documents this page's own mechanism, not a
	// capability of the CLI.
	if command.Name() == "help" && command.Parent() != nil {
		return
	}
	if !command.Hidden {
		fmt.Fprintf(out, "\n## `%s`\n\n", full)
		if short := strings.TrimSpace(command.Short); short != "" {
			fmt.Fprintf(out, "%s\n\n", short)
		}
		if long := strings.TrimSpace(command.Long); long != "" && long != strings.TrimSpace(command.Short) {
			fmt.Fprintf(out, "```\n%s\n```\n\n", long)
		}
		fmt.Fprintf(out, "```\n%s\n```\n", usageLine(command, full))
		if flags := strings.TrimRight(command.NonInheritedFlags().FlagUsagesWrapped(0), "\n"); flags != "" {
			fmt.Fprintf(out, "\nFlags:\n\n```\n%s\n```\n", flags)
		}
		if subs := visibleSubcommands(command); len(subs) > 0 {
			out.WriteString("\nSubcommands:\n\n")
			for _, sub := range subs {
				fmt.Fprintf(out, "- [`%s %s`](#codefly-%s)\n", full, sub.Name(),
					strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(full, "codefly")+" "+sub.Name()), " ", "-"))
			}
		}
	}
	for _, sub := range visibleSubcommands(command) {
		renderCommand(out, sub, full)
	}
}

// usageLine is the command's declared use, qualified by its full path, so the
// line is one a reader can paste.
func usageLine(command *cobra.Command, full string) string {
	use := command.Use
	if index := strings.IndexByte(use, ' '); index >= 0 {
		return full + use[index:]
	}
	if command.Runnable() {
		return full + " [flags]"
	}
	return full + " <subcommand>"
}

func visibleSubcommands(command *cobra.Command) []*cobra.Command {
	var subs []*cobra.Command
	for _, sub := range command.Commands() {
		if sub.Hidden || (sub.Name() == "help" && sub.Parent() != nil) {
			continue
		}
		subs = append(subs, sub)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	return subs
}

// TestCLIReferenceGuardCatchesANewCommand proves the guard fires. A coverage
// test that cannot fail documents nothing, and this one's whole value is that
// shipping a verb without documenting it is impossible — so that is asserted
// rather than assumed.
func TestCLIReferenceGuardCatchesANewCommand(t *testing.T) {
	current, err := os.ReadFile(filepath.Clean(referencePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != renderCLIReference(RootCmd) {
		t.Fatal("the reference is already out of date; regenerate before running this")
	}

	added := &cobra.Command{Use: "undocumented-verb", Short: "A verb added without regenerating the reference", Run: func(*cobra.Command, []string) {}}
	added.Flags().String("undocumented-flag", "", "A flag added without regenerating the reference")
	RootCmd.AddCommand(added)
	defer RootCmd.RemoveCommand(added)

	regenerated := renderCLIReference(RootCmd)
	if regenerated == string(current) {
		t.Fatal("adding a command did not change the reference: the guard would not catch a new verb")
	}
	for _, want := range []string{"## `codefly undocumented-verb`", "--undocumented-flag"} {
		if !strings.Contains(regenerated, want) {
			t.Errorf("the regenerated reference does not carry %q", want)
		}
	}
}

// A flag added to an existing command must move the reference too: the drift
// that went unnoticed was flags changing on commands that were already written
// up, not only whole verbs going missing.
func TestCLIReferenceGuardCatchesANewFlagOnAnExistingCommand(t *testing.T) {
	before := renderCLIReference(RootCmd)
	target, _, err := RootCmd.Find([]string{"deploy", "secrets"})
	if err != nil {
		t.Fatal(err)
	}
	target.Flags().String("undocumented-flag", "", "A flag added without regenerating the reference")
	defer func() {
		// pflag has no removal, so rebuild the set without the probe flag.
		rebuilt := pflag.NewFlagSet(target.Name(), pflag.ContinueOnError)
		target.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Name != "undocumented-flag" {
				rebuilt.AddFlag(f)
			}
		})
		target.ResetFlags()
		target.Flags().AddFlagSet(rebuilt)
	}()

	if after := renderCLIReference(RootCmd); after == before {
		t.Fatal("adding a flag to `deploy secrets` did not change the reference: flag drift would go unnoticed")
	}
}
