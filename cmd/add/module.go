package add

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	modulesync "github.com/codefly-dev/cli/cmd/sync"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsmodule "github.com/codefly-dev/core/actions/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Create a module to group related services and jobs",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		if moduleSource != "" || moduleWorktree != "" {
			return addComposedModule(args[0])
		}
		if cmd.Flags().Changed("version") {
			return fmt.Errorf("--version only applies when composing a module with --source or --worktree")
		}
		return addModule(args[0])
	},
}

var moduleAgentInput string
var moduleSource string
var moduleWorktree string
var moduleVersion string
var moduleWithDefault bool

// addComposedModule composes an out-of-repo module without vendoring a copy.
//
// It splits identity from location: the machine-specific location goes into the
// gitignored codefly.local.yaml overlay (a `resolve:` directive), and — only
// when the module is not already composed — a portable identity (source +
// version) is added to the committed workspace config. So the committed file
// never carries a machine path, and choosing a local source leaves `git status`
// clean when the module is already declared.
//
//   - --source <path>:            overlay `path: <abs>` (edit this dir in place).
//   - --worktree <repo>@<ref>:    overlay `worktree: <repo>@<ref>` (find my checkout).
func addComposedModule(name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	directive := &resources.ModuleResolveDirective{}
	var identitySource string
	var location string
	if moduleWorktree != "" {
		repo, ref, ok := strings.Cut(moduleWorktree, "@")
		if !ok || repo == "" || ref == "" {
			return fmt.Errorf("--worktree %q must be <owner/repo>@<ref>", moduleWorktree)
		}
		directive.Worktree = moduleWorktree
		identitySource = repo
		location = fmt.Sprintf("worktree %s", moduleWorktree)
	} else {
		source, err := filepath.Abs(moduleSource)
		if err != nil {
			return fmt.Errorf("cannot resolve source path %q: %w", moduleSource, err)
		}
		mod, err := resources.LoadModuleFromDir(ctx, source)
		if err != nil {
			return fmt.Errorf("cannot load module at %s: %w", source, err)
		}
		if mod.Name != name {
			return fmt.Errorf("module at %s is named <%s>, not <%s>", source, mod.Name, name)
		}
		directive.Path = source
		// Derive identity from the checkout's origin remote when the source is a
		// git repo, so the committed reference can name the canonical repo. A
		// non-git source composes location-only (overlay just points at it).
		identitySource = gitRemoteIdentity(source)
		location = source
	}

	// 1. Identity → the committed config, only if not already composed. Never a
	// path: the committed reference stays portable across every checkout. This
	// durable declaration is written first: if the overlay write below then
	// fails, the module is declared-but-unlocated (the resolver treats it as
	// pinned) and re-running the command completes it — a coherent partial state
	// rather than an overlay entry orphaned to a module that was never declared.
	if !workspace.ExistsModule(name) {
		ref := &resources.ModuleReference{Name: name}
		if identitySource != "" {
			ref.Source = identitySource
			ref.Version = moduleVersion
		}
		if err := workspace.AddModuleReference(ref); err != nil {
			return fmt.Errorf("cannot add module reference: %w", err)
		}
		if err := workspace.Save(ctx); err != nil {
			return fmt.Errorf("cannot save workspace: %w", err)
		}
	}

	// 2. Location → the gitignored overlay. Read the workspace's own overlay
	// (not an ancestor's) so sibling entries are preserved, then upsert.
	overlay := &resources.LocalOverlay{Resolve: map[string]*resources.ModuleResolveDirective{}}
	if _, statErr := os.Stat(filepath.Join(workspace.Dir(), resources.LocalOverlayConfigurationName)); statErr == nil {
		loaded, err := resources.LoadLocalOverlay(ctx, workspace.Dir())
		if err != nil {
			return fmt.Errorf("cannot load local overlay: %w", err)
		}
		if loaded != nil {
			overlay = loaded
		}
	}
	if overlay.Resolve == nil {
		overlay.Resolve = map[string]*resources.ModuleResolveDirective{}
	}
	overlay.Resolve[name] = directive
	if saveErr := resources.SaveLocalOverlay(ctx, workspace.Dir(), overlay); saveErr != nil {
		return fmt.Errorf("cannot save local overlay: %w", saveErr)
	}
	// Keep the machine-local overlay out of git so a local source choice never
	// shows up in `git status`. Idempotent: the committed .gitignore gains the
	// rule once, then subsequent composes touch only the ignored overlay.
	if ignoreErr := ensureGitignoreEntry(workspace.Dir(), resources.LocalOverlayConfigurationName); ignoreErr != nil {
		return fmt.Errorf("cannot gitignore %s: %w", resources.LocalOverlayConfigurationName, ignoreErr)
	}

	cli.Header(2, "Module <%s> composed from %s (location in %s).", name, location, resources.LocalOverlayConfigurationName)
	return nil
}

// gitRemoteIdentity returns "<owner>/<repo>" from dir's origin remote when dir
// is a git checkout, or "" when it is not a repo or has no origin. The value
// feeds a committed module reference's canonical source identity.
func gitRemoteIdentity(dir string) string {
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return ""
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		return ""
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return ""
	}
	return normalizeRemoteToOwnerRepo(urls[0])
}

// ensureGitignoreEntry appends entry to the workspace's .gitignore unless an
// exact-match line is already present, creating the file when absent. It leaves
// any other content untouched.
func ensureGitignoreEntry(dir, entry string) error {
	gitignore := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitignore)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	var buf []byte
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		buf = append(buf, '\n')
	}
	buf = append(buf, []byte(entry+"\n")...)
	f, err := os.OpenFile(gitignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(buf)
	return err
}

// normalizeRemoteToOwnerRepo reduces a git remote URL (SSH or HTTPS, any host)
// to a lowercase "<owner>/<repo>" slug by stripping the scheme, optional
// userinfo, and host. It mirrors core's resolver repo normalization so a
// committed source identity compares equal to the origin remote the resolver
// matches worktrees against — and, unlike a substring match on "github.com", it
// is not fooled by a look-alike host such as "github.com.example".
func normalizeRemoteToOwnerRepo(remote string) string {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		if slash := strings.Index(s, "/"); slash >= 0 {
			s = s[slash+1:]
		}
	} else if at := strings.Index(s, "@"); at >= 0 {
		// scp-like syntax: git@host:org/repo
		s = s[at+1:]
		if colon := strings.Index(s, ":"); colon >= 0 {
			s = s[colon+1:]
		}
	}
	return strings.ToLower(strings.Trim(s, "/"))
}

func addModule(name string) (result error) {
	ctx, done := common.NewContext()
	defer done()

	w := wool.Get(ctx).In("cmd.add.module")

	// Non-interactive mode: skip all TUI prompts (for MCP, CI, scripts)
	cli.SetWithDefault(moduleWithDefault)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	// In non-interactive mode (--yes), skip confirmation
	if !moduleWithDefault {
		confirm := models.Confirm(ctx, fmt.Sprintf("Add a module in your workspace <%s>?", workspace.Name), true)
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			return nil
		}
	}

	// Resolve module agent if specified
	var agent *resources.Agent
	if moduleAgentInput != "" {
		var err error
		agent, err = common.GetModuleAgent(ctx, moduleAgentInput)
		if err != nil {
			return fmt.Errorf("cannot resolve module agent: %w", err)
		}
		cli.Header(2, "Using module template: %s", agent.Identifier())
	}

	input := &actionsmodule.AddModule{
		Name: name,
	}
	if agent != nil {
		// Assign through input.Agent rather than a temporary: a `:=` here would
		// shadow the enclosing err.
		input.Agent, err = agent.Proto()
		if err != nil {
			return fmt.Errorf("cannot resolve agent %s: %w", agent.Identifier(), err)
		}
	}
	var preparedSource *modulesync.PreparedModuleSource
	if agent != nil {
		targetRoot := workspace.ModulePath(ctx, &resources.ModuleReference{Name: name})
		preparedSource, err = modulesync.PrepareModuleSource(ctx, targetRoot, agent)
		if err != nil {
			return fmt.Errorf("prepare module scaffold source: %w", err)
		}
		defer preparedSource.Close()
	}

	action, err := actionsmodule.NewActionAddModule(ctx, input)
	if err != nil {
		return fmt.Errorf("cannot create action: %w", err)
	}
	out, err := actions.Run(ctx, action, &actions.Space{Workspace: workspace})
	if err != nil {
		return fmt.Errorf("cannot add module: %w", err)
	}
	moduleAdded := true
	moduleDir := workspace.ModulePath(ctx, &resources.ModuleReference{Name: name})
	defer func() {
		if result == nil || !moduleAdded {
			return
		}
		if rollbackErr := workspace.DeleteModule(ctx, name); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module reference: %w", rollbackErr))
			return
		}
		if rollbackErr := os.RemoveAll(moduleDir); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module directory: %w", rollbackErr))
		}
	}()

	mod, err := actions.As[resources.Module](out)
	if err != nil {
		return fmt.Errorf("cannot read added module: %w", err)
	}

	// If a module agent was specified, execute it to scaffold services and templates
	if agent != nil {
		binPath, err := agent.Path(ctx)
		if err != nil {
			return fmt.Errorf("cannot resolve module agent binary path: %w", err)
		}
		w.Info("executing module agent", wool.Field("binary", binPath), wool.Field("dir", mod.Dir()))

		cmd := exec.CommandContext(ctx, binPath, mod.Dir(), name)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("module agent failed: %w", err)
		}
		if err := preparedSource.Pin(mod); err != nil {
			return fmt.Errorf("pin module scaffold source: %w", err)
		}
		cli.Header(2, "Module agent scaffolded services for <%s>", name)
	}

	moduleAdded = false
	cli.Header(2, "Module <%s> added.", mod.Name)
	return nil
}

func init() {
	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ModuleCmd.Flags().StringVar(&moduleAgentInput, "agent", "", "Module template agent (e.g. user-management, rag)")
	ModuleCmd.Flags().StringVar(&moduleSource, "source", "", "Compose an out-of-repo module at a local path; the path lands in codefly.local.yaml, not committed config")
	ModuleCmd.Flags().StringVar(&moduleWorktree, "worktree", "", "Compose an out-of-repo module by <owner/repo>@<ref>; the resolver finds your local worktree (location in codefly.local.yaml)")
	ModuleCmd.Flags().StringVar(&moduleVersion, "version", "latest", "Committed version constraint recorded with a composed module's identity")
	ModuleCmd.MarkFlagsMutuallyExclusive("agent", "source", "worktree")
	ModuleCmd.Flags().BoolVar(&moduleWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
