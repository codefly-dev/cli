package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// baseManifest mirrors tools/base-manifest.json, written by a module's
// `tools/base-integrity.mjs gen` against canonical and shipped into each
// consumer by codefly sync. It records a sha256 of every base file.
type baseManifest struct {
	Files map[string]string `json:"files"`
}

// VerifyCmd checks base-file integrity across the workspace's composed modules.
//
// codefly composes a published module into a consumer by install/sync — a copy.
// The contract is that a consumer ADDS files on the side and never MODIFIES the
// base it synced from canonical. This makes that contract enforceable: for every
// module carrying a tools/base-manifest.json, re-hash the listed base files and
// fail on any modified or missing one. Files NOT in the manifest are legal
// side-additions. A consumer-local tools/base-integrity-allow.json whitelists
// genuinely per-consumer base files (e.g. module.codefly.yaml — the module's own
// name/composition), logged loudly so it can never hide drift silently.
//
// This is the codefly-native counterpart to running the shipped
// `node tools/base-integrity.mjs check` by hand in each consumer's CI.
var VerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify base-file integrity of composed modules (consumers ADD, never modify the base)",
	Long: `Verify that every composed module still matches the base it synced from canonical.

For each module with a tools/base-manifest.json, re-hash the recorded base files
and fail on any modified or missing one. Files not in the manifest are legal
side-additions. To change a base file, promote the change upstream into canonical
(making the original stronger) or express it as a side-addition — never edit the
synced copy.`,
	Run: func(cmd *cobra.Command, args []string) {
		verifyCommand()
	},
}

func verifyCommand() {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)
	modules, err := workspace.LoadModules(ctx)
	cli.ExitOnError(err, "cannot load workspace modules")

	checked, failed := 0, 0
	for _, mod := range modules {
		dir := mod.Dir()
		data, err := os.ReadFile(filepath.Join(dir, "tools", "base-manifest.json"))
		if err != nil {
			// No manifest → this module isn't derived from a guarded base. Skip.
			continue
		}
		checked++

		var m baseManifest
		if err := json.Unmarshal(data, &m); err != nil {
			cli.Warning("module <%s>: invalid base-manifest.json: %v", mod.Name, err)
			failed++
			continue
		}
		allow := loadBaseIntegrityAllow(filepath.Join(dir, "tools", "base-integrity-allow.json"))

		// A consumer may compose a SUBSET of the base's services (e.g. mind takes
		// the backend and brings its own gateway, omitting auth-sidecar + frontend).
		// Files under an omitted service are legitimately absent, not "missing".
		composed := map[string]bool{}
		for _, ref := range mod.ServiceReferences {
			composed[ref.Name] = true
		}
		omitted := map[string]int{}

		var modified, missing []string
		for rel, want := range m.Files {
			if svc := serviceOf(rel); svc != "" && len(composed) > 0 && !composed[svc] {
				omitted[svc]++
				continue
			}
			got, err := sha256File(filepath.Join(dir, rel))
			if err != nil {
				if _, ok := allow[rel]; !ok {
					missing = append(missing, rel)
				}
				continue
			}
			if got != want {
				if reason, ok := allow[rel]; ok {
					cli.Info("  module <%s>: ALLOWED divergence (%s): %s", mod.Name, reason, rel)
				} else {
					modified = append(modified, rel)
				}
			}
		}

		if len(omitted) > 0 {
			total := 0
			svcs := make([]string, 0, len(omitted))
			for s, n := range omitted {
				total += n
				svcs = append(svcs, s)
			}
			sort.Strings(svcs)
			cli.Info("  module <%s>: composed subset — skipped %d base files for non-composed service(s): %s",
				mod.Name, total, strings.Join(svcs, ", "))
		}
		if len(modified) == 0 && len(missing) == 0 {
			cli.Info("✓ module <%s>: base intact (%d base files)", mod.Name, len(m.Files))
			continue
		}
		failed++
		cli.Warning("✗ module <%s>: %d modified, %d missing base file(s)", mod.Name, len(modified), len(missing))
		for _, rel := range missing {
			cli.Warning("    MISSING  %s", rel)
		}
		for _, rel := range modified {
			cli.Warning("    MODIFIED %s", rel)
		}
	}

	if checked == 0 {
		cli.Info("base-integrity: no module carries a base-manifest.json — nothing to verify")
		return
	}
	if failed > 0 {
		cli.ExitOnError(
			fmt.Errorf("%d module(s) failed: promote the change upstream into canonical, or express it as a side-addition", failed),
			"base-integrity verification failed")
	}
	cli.Info("base-integrity OK across %d module(s)", checked)
}

// serviceOf returns the service a base file belongs to (services/<svc>/...),
// or "" for module-level files (always enforced regardless of composition).
func serviceOf(rel string) string {
	const p = "services/"
	if !strings.HasPrefix(rel, p) {
		return ""
	}
	rest := rel[len(p):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return ""
}

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func loadBaseIntegrityAllow(path string) map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}
