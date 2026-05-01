package cmd

import (
	"github.com/codefly-dev/cli/cmd/publish"
)

// PublishCmd is the unified release entrypoint — `codefly publish`.
// Replaces the per-repo scripts/publish/{tag,re_tag,push}.sh that
// existed across every codefly-dev repo (16+ scripts, all with
// `git push -f`, all with zero pre-flight checks). One command
// per repo: `codefly publish` (auto-detects mode from cwd).
var PublishCmd = publish.Cmd
