package environment

import (
	"context"

	"github.com/spf13/cobra"
)

// Cmd groups the commands that declare and inspect deploy environments in
// workspace.codefly.yaml.
var Cmd = &cobra.Command{
	Use:   "environment",
	Short: "Declare and inspect deploy environments",
}

// PostImportValidate runs `codefly doctor workspace --env <env>`'s readiness
// validation against a resolved environment and prints the result. It is wired
// from package cmd, which owns the doctor engine, to avoid an import cycle
// (package cmd already imports this package to register Cmd). It is nil in unit
// tests, where the post-write validation is not under test.
var PostImportValidate func(ctx context.Context, dir, env string)

func init() {
	Cmd.AddCommand(importCmd)
	Cmd.AddCommand(showCmd)
}
