package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// seedCellFile commits a cell file directly onto main of the bare delivery
// repository, so a publication has a base branch's approved-build list to be
// held to.
func seedCellFile(t *testing.T, remote, gitopsPath, environment string, data []byte) {
	t.Helper()
	seedDelivered(t, remote, "seed the cell file", map[string][]byte{deliveryCellPath(gitopsPath, environment): data})
}

// seedDelivered commits files directly onto main of the bare delivery
// repository, at the repository-relative paths given.
func seedDelivered(t *testing.T, remote, message string, files map[string][]byte) {
	t.Helper()
	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	for relative, data := range files {
		path := filepath.Join(work, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", message)
	gitRun(t, work, "push", "origin", "main")
}

// committedWorkspace makes the fixture workspace a git checkout with its
// composition committed, so the cell file it renders names the revision it
// was rendered from and a later publication can be judged against it.
func committedWorkspace(t *testing.T, workspace *resources.Workspace) string {
	t.Helper()
	dir := workspace.Dir()
	if exec.Command("git", "-C", dir, "rev-parse", "HEAD").Run() != nil {
		gitRun(t, dir, "init", "-q", "--initial-branch=main")
		gitRun(t, dir, "config", "user.name", "Codefly Test")
		gitRun(t, dir, "config", "user.email", "codefly@example.com")
		gitRun(t, dir, "config", "commit.gpgsign", "false")
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "the composition")
	}
	return gitOutput(t, dir, "rev-parse", "HEAD")
}

// renderedFrom is the cell file data as the base carries it after a render
// from revision: the base's provenance, stated as the render that produced it
// states it (cellSource), whatever this fixture's own render recorded.
func renderedFrom(t *testing.T, data []byte, revision string) []byte {
	t.Helper()
	file, err := cellfile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	file.Source = &cellfile.Source{Repo: "https://example.test/delivery.git", Revision: revision, Branch: "main"}
	encoded, err := file.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// legacyDelivery is a delivered tree of a module the fixture's composition
// does not declare, plus a directory under the modules path that is not a
// delivery, as files to seed on the delivery base.
func legacyDelivery() (string, map[string][]byte) {
	legacy := deliveryModulePath("environments", "legacy")
	return legacy, map[string][]byte{
		legacy + "/" + InventoryFilename:                         []byte(`{"module":"legacy","environment":"production"}`),
		legacy + "/services/worker/kustomization.yaml":           []byte("resources: []\n"),
		deliveryModulesPath("environments") + "/notes/README.md": []byte("not a delivery\n"),
	}
}

// TestPublishRemovesADeliveredModuleTheCompositionNoLongerDeclares: a module
// removed from `modules:` left its delivered tree in the delivery repository,
// and with it the ApplicationSet that kept stamping Applications whose every
// pod the cell — which no longer named the module — refused. A forward
// publication to a hosted environment, from a checkout that has seen the
// base's composition, now removes such a tree with the rest of what it
// writes, shows it in the plan, and leaves alone a directory under the
// modules path that is not a delivery (no render record).
func TestPublishRemovesADeliveredModuleTheCompositionNoLongerDeclares(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	head := committedWorkspace(t, workspace)
	env := selectedEnvironment(t, workspace, "production")
	_, data := renderedCell(t, ctx, workspace, env)
	legacy, files := legacyDelivery()
	files[deliveryCellPath("environments", "production")] = renderedFrom(t, data, head)
	seedDelivered(t, remote, "a module since removed from the composition", files)

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.RemovedModules, []string{legacy}) {
		t.Fatalf("the plan removes %v, want %v", plan.RemovedModules, []string{legacy})
	}
	for _, path := range []string{legacy + "/" + InventoryFilename, legacy + "/services/worker/kustomization.yaml"} {
		if !slices.Contains(plan.Changed, path) {
			t.Fatalf("the plan's changed files %v do not carry the removal of %s", plan.Changed, path)
		}
	}
	if slices.ContainsFunc(plan.Changed, func(path string) bool { return strings.Contains(path, "/notes/") }) {
		t.Fatalf("a directory that is not a delivery was removed: %v", plan.Changed)
	}

	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if exec.Command("git", "--git-dir", remote, "cat-file", "-e", result.Commit+":"+legacy+"/"+InventoryFilename).Run() == nil {
		t.Fatalf("the published commit still delivers %s", legacy)
	}
	if exec.Command("git", "--git-dir", remote, "cat-file", "-e", result.Commit+":"+deliveryModulesPath("environments")+"/notes/README.md").Run() != nil {
		t.Fatal("the published commit removed a directory that is not a delivery")
	}
}

// TestAPromotionOwnsTheRemovalItsOwnBranchCarries: the branch guard holds a
// promotion branch to what this module's publication writes, and a removed
// tree is no longer in the clone to be listed — the branch's earlier commit
// deleted it — so a second publish on the module's own branch (its promotion
// still open, or a retry) must own the removal by rule rather than by what it
// finds on disk, or it refuses its own earlier work as someone else's.
func TestAPromotionOwnsTheRemovalItsOwnBranchCarries(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	head := committedWorkspace(t, workspace)
	env := selectedEnvironment(t, workspace, "production")
	_, data := renderedCell(t, ctx, workspace, env)
	legacy, files := legacyDelivery()
	files[deliveryCellPath("environments", "production")] = renderedFrom(t, data, head)
	seedDelivered(t, remote, "a module since removed from the composition", files)

	// The module's earlier promotion: the cell file carried, legacy removed.
	work := t.TempDir()
	gitRun(t, "", "clone", "-q", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "checkout", "-q", "-b", "codefly/promote-payments-production")
	gitRun(t, work, "rm", "-r", "-q", legacy)
	gitRun(t, work, "commit", "-q", "-m", "an earlier promotion of payments, removing legacy")
	gitRun(t, work, "push", "-q", "origin", "codefly/promote-payments-production")

	plan, err := PlanPublish(ctx, workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	})
	if err != nil {
		t.Fatalf("a promotion branch carrying the module's own removal was refused: %v", err)
	}
	if len(plan.RemovedModules) != 0 {
		t.Fatalf("the continued branch already removed %s; the plan removes %v again", legacy, plan.RemovedModules)
	}
}

// TestAStaleCheckoutStillNeedsTheFlagToWithdrawAModule: a checkout that never
// had a module and one that dropped it derive the same cell file. Only a
// checkout that has seen the composition the base's file was rendered from
// may remove on the composition's word; from any other the removal is still
// the flag's to confirm, and even named, the delivered tree is not pruned.
func TestAStaleCheckoutStillNeedsTheFlagToWithdrawAModule(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	committedWorkspace(t, workspace)
	env := selectedEnvironment(t, workspace, "production")
	_, data := renderedCell(t, ctx, workspace, env)
	// The base's file: rendered from a composition this checkout never saw,
	// which named a module legacy this checkout does not declare.
	file, err := cellfile.Parse(renderedFrom(t, data, strings.Repeat("e", 40)))
	if err != nil {
		t.Fatal(err)
	}
	file.Namespaces = append(file.Namespaces, cellfile.Namespace{Name: "legacy", Module: "legacy", Workloads: []cellfile.Workload{{Name: "worker", Kind: "Deployment"}}})
	base, err := file.Encode()
	if err != nil {
		t.Fatal(err)
	}
	legacy, files := legacyDelivery()
	files[deliveryCellPath("environments", "production")] = base
	seedDelivered(t, remote, "a composition this checkout never saw", files)

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	_, err = PlanPublish(ctx, workspace, &request)
	if err == nil || !strings.Contains(err.Error(), "--allow-cell-removal (legacy)") || !strings.Contains(err.Error(), "may be a stale checkout's") {
		t.Fatalf("a stale checkout withdrew a module on the composition's word: %v", err)
	}
	request.AllowCellRemovals = []string{"legacy"}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.RemovedModules) != 0 || slices.ContainsFunc(plan.Changed, func(path string) bool { return strings.HasPrefix(path, legacy) }) {
		t.Fatalf("a removal the flag confirms pruned the delivered tree: %v %v", plan.RemovedModules, plan.Changed)
	}
	if len(plan.CellRemovals) != 1 || len(plan.UndeclaredCellRemovals) != 0 {
		t.Fatalf("the plan reports %v named and %v explained; want one named", plan.CellRemovals, plan.UndeclaredCellRemovals)
	}
}

// TestAnUnfetchedCheckoutOfTheDeliveryRepositoryStillNeedsTheFlag: when the
// workspace repository is the delivery repository, the base's cell file names
// the render's parent as its source, and a checkout at that parent that has
// not fetched the base commit descends from it — yet never saw the `modules:`
// edit that rode in the render's commit. The delivery clone holding that
// workspace revision is what tells one repository from two, so the base
// commit stays the one to contain, and the removal is the flag's to confirm.
func TestAnUnfetchedCheckoutOfTheDeliveryRepositoryStillNeedsTheFlag(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	head := committedWorkspace(t, workspace)
	// One repository: the delivery remote holds the workspace's history.
	gitRun(t, workspace.Dir(), "push", "-q", remote, "HEAD:refs/heads/workspace-history")
	env := selectedEnvironment(t, workspace, "production")
	_, data := renderedCell(t, ctx, workspace, env)
	file, err := cellfile.Parse(renderedFrom(t, data, head))
	if err != nil {
		t.Fatal(err)
	}
	file.Namespaces = append(file.Namespaces, cellfile.Namespace{Name: "legacy", Module: "legacy", Workloads: []cellfile.Workload{{Name: "worker", Kind: "Deployment"}}})
	base, err := file.Encode()
	if err != nil {
		t.Fatal(err)
	}
	legacy, files := legacyDelivery()
	files[deliveryCellPath("environments", "production")] = base
	seedDelivered(t, remote, "the render that named legacy, committed past this checkout", files)

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	_, err = PlanPublish(ctx, workspace, &request)
	if err == nil || !strings.Contains(err.Error(), "--allow-cell-removal (legacy)") || !strings.Contains(err.Error(), "may be a stale checkout's") {
		t.Fatalf("an unfetched checkout of the delivery repository withdrew a module on the composition's word: %v", err)
	}
	request.AllowCellRemovals = []string{"legacy"}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.RemovedModules) != 0 || slices.ContainsFunc(plan.Changed, func(path string) bool { return strings.HasPrefix(path, legacy) }) {
		t.Fatalf("a removal the flag confirms pruned the delivered tree: %v %v", plan.RemovedModules, plan.Changed)
	}
}

// renderedCell renders the environment's cell file from the workspace's module
// trees and answers it with its bytes.
func renderedCell(t *testing.T, ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (CellResult, []byte) {
	t.Helper()
	result, err := RenderCell(ctx, workspace, env)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	return result, data
}

// TestPublishCarriesTheCellFileThatGatesIt is the defect this closes: a
// module's promotion delivered its own tree and left behind the environment's
// cell file — the approved-build list its workloads are admitted by — so a
// cell went on refusing every new build with nothing but a notice to say why.
// The publication now carries that file, at the path the cell's infrastructure
// reads it from, in the same signed commit as the tree, and delivers the
// render's own bytes unsigned: the commit is the custody, and the file's
// consumer reads values, not carriers.
func TestPublishCarriesTheCellFileThatGatesIt(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	env := selectedEnvironment(t, workspace, "production")
	cell, data := renderedCell(t, ctx, workspace, env)
	delivered := deliveryCellPath("environments", "production")
	if cell.Path != filepath.Join(workspace.Dir(), filepath.FromSlash(delivered)) {
		t.Fatalf("the render wrote the cell file at %s, the delivery path is %s", cell.Path, delivered)
	}

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CellFile != delivered {
		t.Fatalf("the plan carries cell file %q, want %q", plan.CellFile, delivered)
	}
	if plan.CellNotice != "" {
		t.Fatalf("a publication that carries the cell file still reports %q", plan.CellNotice)
	}
	if !slices.Contains(plan.Changed, delivered) {
		t.Fatalf("the plan's changed files %v do not name the cell file %s", plan.Changed, delivered)
	}
	if !strings.Contains(plan.Diff, delivered) {
		t.Fatalf("the plan's diff does not show the cell file:\n%s", plan.Diff)
	}

	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	published := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+delivered)
	if published != strings.TrimRight(string(data), "\n") {
		t.Fatalf("the published cell file is not the rendered one:\n%s", published)
	}
	if strings.Contains(published, solutionhost.SignedFileName) {
		t.Fatalf("the published cell file carries a signed envelope:\n%s", published)
	}
	if !strings.Contains(gitOutput(t, "", "--git-dir", remote, "cat-file", "-p", result.Commit), "\ngpgsig ") {
		t.Fatalf("the commit carrying the cell file is not signed")
	}
	// The immutable snapshot is the module's own units; the cell file is the
	// environment's and is read from the branch, so it is not in it.
	if exec.Command("git", "--git-dir", remote, "cat-file", "-e", result.SnapshotRevision+":"+delivered).Run() == nil {
		t.Fatal("the immutable service snapshot carries the environment's cell file")
	}
}

// TestPublishContinuesAPromotionBranchCarryingTheCellFile: the branch guard
// holds a promotion branch to what this publication writes, and the cell file
// is now one of those paths. A branch carrying it is the module's own earlier
// promotion — it was refused as someone else's change while publish wrote the
// module tree alone.
func TestPublishContinuesAPromotionBranchCarryingTheCellFile(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	env := selectedEnvironment(t, workspace, "production")
	_, data := renderedCell(t, ctx, workspace, env)
	delivered := deliveryCellPath("environments", "production")

	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "checkout", "-b", "codefly/promote-payments-production")
	path := filepath.Join(work, filepath.FromSlash(delivered))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "an earlier promotion of payments")
	gitRun(t, work, "push", "origin", "codefly/promote-payments-production")

	plan, err := PlanPublish(ctx, workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	})
	if err != nil {
		t.Fatalf("a promotion branch carrying the module's own cell file was refused: %v", err)
	}
	if plan.CellFile != delivered {
		t.Fatalf("the plan carries cell file %q, want %q", plan.CellFile, delivered)
	}
	// Anything else on the branch is still someone else's work.
	if err := os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("outside promotion\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "unrelated.txt")
	gitRun(t, work, "commit", "-m", "unrelated")
	gitRun(t, work, "push", "origin", "codefly/promote-payments-production")
	if _, err := PlanPublish(ctx, workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}); err == nil || !strings.Contains(err.Error(), "unrelated change") {
		t.Fatalf("plan error = %v", err)
	}
}

// TestPublishHoldsTheCellFileItCarriesToTheBaseBranch: carrying the file does
// not loosen the hold on it. A carried file that drops what the base branch's
// admits is refused by name, with the module to allow — and the refusal comes
// before anything is carried; naming that module carries the file, and the
// plan reports the removal it let through.
func TestPublishHoldsTheCellFileItCarriesToTheBaseBranch(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	env := selectedEnvironment(t, workspace, "production")
	cell, data := renderedCell(t, ctx, workspace, env)
	seedCellFile(t, remote, "environments", "production", data)

	// The cell file a checkout missing a module's render would leave: the
	// workload is gone, and with it the cell's approval of its build.
	file, err := cellfile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	workloads := file.Namespaces[0].Workloads
	if len(workloads) != 2 || workloads[1].Name != "host" {
		t.Fatalf("the fixture's cell file does not hold the two workloads: %+v", workloads)
	}
	file.Namespaces[0].Workloads = workloads[:1]
	dropped, err := file.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cell.Path, dropped, 0o600); err != nil {
		t.Fatal(err)
	}

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	_, err = PlanPublish(ctx, workspace, &request)
	if err == nil || !strings.Contains(err.Error(), "workload payments/Deployment host") ||
		!strings.Contains(err.Error(), "--allow-cell-removal (payments)") {
		t.Fatalf("a cell file removing a workload the base admits was not refused: %v", err)
	}
	if head := gitOutput(t, "", "--git-dir", remote, "show", "main:"+deliveryCellPath("environments", "production")); head != strings.TrimRight(string(data), "\n") {
		t.Fatalf("the refused cell file was carried to the delivery repository anyway:\n%s", head)
	}

	request.AllowCellRemovals = []string{"payments"}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CellFile != deliveryCellPath("environments", "production") {
		t.Fatalf("the allowed removal did not carry the cell file: %q", plan.CellFile)
	}
	if len(plan.CellRemovals) != 1 || !strings.Contains(plan.CellRemovals[0], "workload payments/Deployment host") {
		t.Fatalf("the plan does not report the removal it let through: %v", plan.CellRemovals)
	}
}

// TestAPromotionOwnsItsCellPathWhetherOrNotItCarriesIt holds the one place the
// two path sets are decided (publicationCellPaths) to the reason they differ:
// what a publication WRITES is staged, and a pathspec matching nothing fails
// the staging of everything with it, so the cell file joins that list only
// when there is one to carry; what a promotion of the module OWNS includes the
// cell path either way, or a rollback on the branch an earlier forward
// publication left would be refused as someone else's change.
func TestAPromotionOwnsItsCellPathWhetherOrNotItCarriesIt(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	if _, err := RenderCell(ctx, workspace, env); err != nil {
		t.Fatal(err)
	}
	tree := deliveryModulePath("", "shop")
	delivered := deliveryCellPath("", "staging")

	cell, owned, published, carried, err := publicationCellPaths(workspace, env, "", tree, "")
	if err != nil {
		t.Fatal(err)
	}
	if cell == nil || carried != delivered || !slices.Equal(published, []string{tree, delivered}) || !slices.Equal(owned, []string{tree, delivered}) {
		t.Fatalf("a forward publication writes owned=%v published=%v carried=%q", owned, published, carried)
	}

	_, owned, published, carried, err = publicationCellPaths(workspace, env, "", tree, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if carried != "" || !slices.Equal(published, []string{tree}) {
		t.Fatalf("a rollback carries a cell file: published=%v carried=%q", published, carried)
	}
	if !slices.Equal(owned, []string{tree, delivered}) {
		t.Fatalf("a rollback does not own its module's cell path: %v", owned)
	}

	// An environment with no host has no cell at all: no file is read, and the
	// promotion owns nothing but its module's tree.
	cell, owned, published, carried, err = publicationCellPaths(workspace, &environments.Environment{Name: "unhosted"}, "", tree, "")
	if err != nil {
		t.Fatal(err)
	}
	if cell != nil || carried != "" || !slices.Equal(owned, []string{tree}) || !slices.Equal(published, []string{tree}) {
		t.Fatalf("an unhosted environment contributes a cell path: owned=%v published=%v", owned, published)
	}
}

// TestOnlyAPromotionThatDerivedTheCellFileCarriesIt: the two publications that
// move no approved-build list say so instead of looking complete. A rollback
// restores a module tree from a revision while the workspace's cell file names
// the builds rendered now — the ones being rolled away from — so it carries
// none; a hosted checkout with no cell file rendered carries none either.
func TestOnlyAPromotionThatDerivedTheCellFileCarriesIt(t *testing.T) {
	hosted := &environments.Environment{Name: "production"}
	hosted.Host = &environments.EnvironmentHost{Coordinate: "example/prod/region-a"}
	carried := &publicationCell{
		source:    filepath.Join("w", "environments", "cells", "production", cellfile.FileName),
		delivered: deliveryCellPath("environments", "production"),
	}

	if notice := cellNotice(hosted, carried, "", "payments"); notice != "" {
		t.Fatalf("a forward publication carrying the file reports %q", notice)
	}
	rollback := cellNotice(hosted, carried, "0123456789abcdef0123456789abcdef01234567", "payments")
	for _, want := range []string{"this rollback restores payments's delivered tree and moves no cell file", "publish it forward"} {
		if !strings.Contains(rollback, want) {
			t.Fatalf("the rollback notice does not say %q: %s", want, rollback)
		}
	}
	unrendered := cellNotice(hosted, nil, "", "payments")
	for _, want := range []string{"holds no cell file", "render --all --env production"} {
		if !strings.Contains(unrendered, want) {
			t.Fatalf("the unrendered notice does not say %q: %s", want, unrendered)
		}
	}
	if notice := cellNotice(&environments.Environment{Name: "staging"}, nil, "", "payments"); notice != "" {
		t.Fatalf("an environment with no host has no cell to report: %q", notice)
	}
}
