package run

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/fixturefragments"
)

// provisionFixtureComposition writes, before a run loads its configuration,
// the local authority's fixture-composition document: the merge of the
// fragment every module of the workspace ships (fixturefragments), which
// `codefly generate fixture-composition` writes on its own. Nothing is written
// unless a service declares the group. A conflict between two fragments
// refuses the run naming both. A hand-written document is never overwritten:
// the run reads it as it stands and says that no module's fragment is applied.
func provisionFixtureComposition(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) error {
	declared, err := fixturefragments.Declared(ctx, workspace)
	if err != nil || !declared {
		return err
	}
	prepared, err := fixturefragments.Prepare(ctx, workspace, env)
	if err != nil {
		return fmt.Errorf("%s: %w", fixturefragments.Group, err)
	}
	changed, err := fixturefragments.Write(prepared)
	if errors.Is(err, fixturefragments.ErrHandWritten) {
		cli.Warning("%v. This run reads it as it stands: no module's fragment is applied", err)
		return nil
	}
	if err != nil {
		return err
	}
	if changed {
		cli.Info("merged %s from %d fragment(s): %s", prepared.Target, len(prepared.Sources), strings.Join(prepared.Sources, ", "))
	}
	return nil
}
