package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/cliupdate"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/releaseupdate"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// Diagnostic codes of the pin-currency check. A pin that is coherent can still
// be old: every other check asks whether the composition fits together, and
// none asks whether what it pins is the newest thing its producer shipped.
const (
	codePinStale              = "pin_stale"
	codePinHeld               = "pin_held"
	codePinHoldInvalid        = "pin_hold_invalid"
	codePinCurrencyUnverified = "pin_currency_unverified"
	codeCheckoutOverride      = "checkout_override_active"
	codeCLIStale              = "cli_stale"
)

// pinHoldsKey is the committed list that says a pin is behind on purpose.
// checkStatusWarn is the status of a check that reports without failing.
const checkStatusWarn = "warn"

const pinHoldsKey = "pin-holds"

// currencyTimeout bounds every remote read of the check together. It is the
// whole reason the check can be on by default: nothing here may wait longer.
const currencyTimeout = 10 * time.Second

// strictCurrencyTimeout is the same bound for a render, which must not turn a
// cold first run (every history fetched once) into a spurious refusal.
const strictCurrencyTimeout = 25 * time.Second

// pinHold states that one pin is behind deliberately. It names the exact
// value held, so the hold ends the moment the pin moves; it must say why; and
// it must expire. A held pin is reported on every run, never skipped.
type pinHold struct {
	Name   string `yaml:"name"`
	Pin    string `yaml:"pin"`
	Reason string `yaml:"reason"`
	Until  string `yaml:"until"`
}

func loadPinHolds(workspaceDir string) ([]pinHold, error) {
	content, err := os.ReadFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName))
	if err != nil {
		return nil, err
	}
	var file struct {
		Holds []pinHold `yaml:"pin-holds"`
	}
	if err := yaml.Unmarshal(content, &file); err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", pinHoldsKey, resources.WorkspaceConfigurationName, err)
	}
	return file.Holds, nil
}

// readSourceHead is the one network read of the check, a variable so the
// package's tests never reach a real remote.
var readSourceHead = composition.ReadSourceHead

// currencyPin is one pin the check judges: a module or an imported workspace.
type currencyPin struct {
	label   string // "saas", "workspace platform-core"
	name    string // the name a hold uses
	source  string
	version string
	// declaredBy names the imported workspace that declares the pin, when it
	// is not the one being checked: that is where the pin moves.
	declaredBy string
}

// currencyVerdict is the judgement on one pin against its source.
type currencyVerdict struct {
	current bool
	detail  string
}

func (p *currencyPin) String() string {
	if p.declaredBy != "" {
		return fmt.Sprintf("%s (declared by workspace %s) pins %s at %s", p.label, p.declaredBy, p.source, shortPin(p.version))
	}
	return fmt.Sprintf("%s pins %s at %s", p.label, p.source, shortPin(p.version))
}

func shortPin(version string) string {
	if composition.IsCommitPin(version) {
		return "commit " + version[:8]
	}
	return "tag " + version
}

// judgePin compares one pin to its source: a commit is current at the tip of
// the default branch, a tag when it is the newest release, a range when it
// reaches the newest release.
func judgePin(ctx context.Context, p *currencyPin, head *composition.SourceHead) currencyVerdict {
	version := strings.TrimSpace(p.version)
	branch := fmt.Sprintf("%s (%s)", head.Branch, head.Tip[:8])
	if commit, ok := composition.CommitPin(version); ok {
		if commit == head.Tip {
			return currencyVerdict{true, fmt.Sprintf("the tip of %s", head.Branch)}
		}
		lag, err := composition.CommitLag(ctx, p.source, head, commit)
		switch {
		case err != nil:
			return currencyVerdict{false, fmt.Sprintf("not the tip of %s (distance not measured: %v)", branch, err)}
		case !lag.OnBranch:
			return currencyVerdict{false, fmt.Sprintf("not in the history of %s: an unmerged branch, or a commit the history could not find", branch)}
		default:
			return currencyVerdict{false, fmt.Sprintf("%s behind %s", plural(lag.Behind, "commit"), branch)}
		}
	}
	// A pin that can never name anything, whatever its spelling: one predicate
	// answers it, and the hold bypass below reads the same answer rather than
	// deriving the question a second time.
	//
	// ABOVE the release comparison on purpose. A pin's shape has nothing to do
	// with whether its source has cut a release, and the comparison below answers
	// "no release to compare with" as CURRENT — so judged there, an unresolvable
	// pin on a tagless source reported green and the render still died. A commit
	// pin is exactly what a source that has not cut a tag yet is pinned by, so
	// that is not a rare case; it is the common one.
	if detail, unresolvable := composition.UnresolvablePin(version, head.Tags); unresolvable {
		return currencyVerdict{false, detail}
	}
	newest := composition.NewestRelease(head.Tags)
	if version == "" || version == "latest" {
		return currencyVerdict{true, "follows the newest release"}
	}
	if newest == "" {
		return currencyVerdict{true, "the source publishes no release to compare with"}
	}
	newestVersion, err := semver.NewVersion(newest)
	if err != nil {
		return currencyVerdict{true, "the source publishes no release to compare with"}
	}
	if pinned, err := semver.NewVersion(strings.TrimPrefix(version, "v")); err == nil {
		if pinned.Equal(newestVersion) {
			return currencyVerdict{true, "the newest release"}
		}
		if pinned.GreaterThan(newestVersion) {
			return currencyVerdict{false, fmt.Sprintf("newer than the newest stable release, %s (a pre-release, or a tag not yet published as a release)", newest)}
		}
		return currencyVerdict{false, fmt.Sprintf("%s behind the newest, %s", plural(composition.ReleasesNewerThan(head.Tags, version), "release"), newest)}
	}
	if constraint, err := semver.NewConstraint(version); err == nil {
		if constraint.Check(newestVersion) {
			return currencyVerdict{true, fmt.Sprintf("a range that reaches the newest release, %s", newest)}
		}
		return currencyVerdict{false, fmt.Sprintf("a range that cannot reach the newest release, %s", newest)}
	}
	return currencyVerdict{false, fmt.Sprintf("neither a tag, a range nor a commit, so it was not compared with %s", branch)}
}

// checkPinCurrency reports, for every pin the composition resolves, whether it
// is the newest its source has shipped, and for every module that does not run
// from its pin, where it runs from and how far that sits behind the source.
// strict makes a stale pin, or an unreadable source, a failure instead of a
// warning: the stance of a render, which must not ship an old pin unseen.
func checkPinCurrency(ctx context.Context, ws *resources.Workspace, module string, strict bool, report *workspaceReadinessReport) {
	timeout := currencyTimeout
	if strict {
		timeout = strictCurrencyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The CLI's own release check is slower than any source read; it runs beside them.
	cliDone := make(chan workspaceDiagnostic, 1)
	go func() { cliDone <- cliCurrency(ctx) }()

	pins, names, overrides := currencyPins(ctx, ws, module)
	holds, err := loadPinHolds(ws.Dir())
	if err != nil {
		report.add(codePinHoldInvalid, pinHoldsKey, "fail", err.Error(), fmt.Sprintf("fix %s in %s", pinHoldsKey, resources.WorkspaceConfigurationName))
	}

	// One round trip per source, all at once: the check's whole cost.
	sources := map[string]*composition.SourceHead{}
	failures := map[string]error{}
	var mu sync.Mutex
	var group sync.WaitGroup
	read := func(source string) {
		defer group.Done()
		head, err := readSourceHead(ctx, source)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures[source] = err
			return
		}
		sources[source] = head
	}
	seen := map[string]bool{}
	for _, source := range append(pinSources(pins), overrideSources(overrides)...) {
		if !seen[source] {
			seen[source] = true
			group.Add(1)
			go read(source)
		}
	}
	group.Wait()

	// An unreadable source is never green: the check could not do its job.
	unverified := checkStatusWarn
	if strict {
		unverified = "fail"
	}
	if len(failures) > 0 {
		unreadable := sortedKeys(failures)
		report.add(codePinCurrencyUnverified, "pin currency", unverified,
			fmt.Sprintf("the currency of the pins on %d of %d sources was NOT verified (%s): %v", len(unreadable), len(seen), strings.Join(unreadable, ", "), failures[unreadable[0]]),
			"reconnect (or sign in to GitHub) and run again; a render refuses to proceed on pins it could not compare")
	}

	// Judge each distinct pin once. Pins on one source run in turn (they share
	// one history clone); sources run side by side.
	verdicts := make([]currencyVerdict, len(pins))
	bySource := map[string][]int{}
	for i, p := range pins {
		if sources[p.source] != nil {
			bySource[p.source] = append(bySource[p.source], i)
		}
	}
	for source, indexes := range bySource {
		group.Add(1)
		go func() {
			defer group.Done()
			judged := map[string]currencyVerdict{}
			for _, i := range indexes {
				if _, done := judged[pins[i].version]; !done {
					judged[pins[i].version] = judgePin(ctx, &pins[i], sources[source])
				}
				verdicts[i] = judged[pins[i].version]
			}
		}()
	}
	group.Wait()

	stale, held, current := 0, 0, 0
	usable := validPinHolds(holds, names, time.Now().UTC(), report)
	used := make([]bool, len(holds))
	staleStatus := checkStatusWarn
	if strict {
		staleStatus = "fail"
	}
	for i, p := range pins {
		if sources[p.source] == nil {
			// Not compared, so not stale and not "holding nothing": already said above.
			if hold, index := matchingHold(holds, usable, &p); hold != nil {
				used[index] = true
			}
			continue
		}
		verdict := verdicts[i]
		name := "pin currency of " + p.label
		if verdict.current {
			current++
			report.add("", name, "ok", fmt.Sprintf("%s: %s", p, verdict.detail), "")
			continue
		}
		hold, index := matchingHold(holds, usable, &p)
		// A hold says "behind on purpose", and states a reason and an end date.
		// An unresolvable pin is not behind: it names nothing any remote can
		// resolve, so there is nothing to be behind of and nothing a hold can
		// excuse. The SAME predicate judgePin used answers it — re-deriving the
		// question here is how the uppercase spelling ended up protected in one
		// place and refused in the other.
		//
		// A matching hold is still marked used: it is not "holding nothing", it is
		// holding something that cannot resolve, and telling an operator both at
		// once sends them to the wrong file.
		if _, unresolvable := composition.UnresolvablePin(p.version, sources[p.source].Tags); unresolvable {
			if hold != nil {
				used[index] = true
			}
			stale++
			report.add(codePinStale, name, staleStatus, fmt.Sprintf("%s: %s", p, verdict.detail),
				fmt.Sprintf("write the pin of %s so it resolves — the lowercase 40-character object name, or a tag its source publishes; a hold cannot cover a pin that resolves nowhere", p.name))
			continue
		}
		if hold == nil {
			stale++
			report.add(codePinStale, name, staleStatus, fmt.Sprintf("%s: %s", p, verdict.detail),
				fmt.Sprintf("move the pin of %s to the tip or the newest release (an imported workspace moves its own), or state it in %s of %s with a reason and an end date",
					p.name, pinHoldsKey, resources.WorkspaceConfigurationName))
			continue
		}
		used[index] = true
		held++
		if report.heldUntil == "" || hold.Until < report.heldUntil {
			report.heldUntil = hold.Until
		}
		report.add(codePinHeld, name, checkStatusWarn, fmt.Sprintf("%s: %s -- HELD until %s: %s", p, verdict.detail, hold.Until, hold.Reason),
			fmt.Sprintf("drop the hold from %s once %s is moved", pinHoldsKey, p.name))
	}
	for i, hold := range holds {
		if usable[i] && !used[i] && slices.ContainsFunc(pins, func(p currencyPin) bool { return p.name == hold.Name }) {
			report.add(codePinHoldInvalid, "pin hold "+hold.Name, checkStatusWarn,
				fmt.Sprintf("the hold on %s at %s holds nothing: that pin is current or has moved", hold.Name, shortHold(hold.Pin)),
				fmt.Sprintf("delete it from %s of %s", pinHoldsKey, resources.WorkspaceConfigurationName))
		}
	}

	reportOverrides(ctx, overrides, sources, report)
	cli := <-cliDone
	report.add(cli.Code, cli.Name, cli.Status, cli.Message, cli.Remediation)
	// No duration here: a doctor report is deterministic for a given state.
	report.pinsCompared = true
	report.add("", "pin currency", "ok", fmt.Sprintf("%d pins compared with their sources: %d current, %d stale, %d held",
		current+stale+held, current, stale, held), "")
}

func shortHold(pin string) string {
	if composition.IsCommitPin(pin) {
		return pin[:8]
	}
	return pin
}

// validPinHolds checks every hold on its own and says which can apply. A hold
// with no reason, no end date or no pin to name is a failure — it is the
// exception that hides things — and one past its end date applies to nothing
// and is said so, so a stale exception never turns into silence.
func validPinHolds(holds []pinHold, names []string, now time.Time, report *workspaceReadinessReport) []bool {
	usable := make([]bool, len(holds))
	for i, hold := range holds {
		name := "pin hold " + hold.Name
		named := slices.Contains(names, hold.Name)
		until, err := time.Parse("2006-01-02", hold.Until)
		switch {
		case !named:
			report.add(codePinHoldInvalid, name, "fail", fmt.Sprintf("the hold names %q, which is not a module or imported workspace of this composition", hold.Name),
				fmt.Sprintf("fix or delete it in %s of %s", pinHoldsKey, resources.WorkspaceConfigurationName))
		case strings.TrimSpace(hold.Pin) == "" || strings.TrimSpace(hold.Reason) == "":
			report.add(codePinHoldInvalid, name, "fail", fmt.Sprintf("the hold on %s needs `pin:` (the exact value held) and `reason:` (why it is behind)", hold.Name),
				"a hold says what it holds and why, so it ends when the pin moves")
		case err != nil:
			report.add(codePinHoldInvalid, name, "fail", fmt.Sprintf("the hold on %s needs `until:` as YYYY-MM-DD", hold.Name),
				"a hold ends: add the date it must be gone by")
		case now.After(until.Add(24 * time.Hour)):
			report.add(codePinHoldInvalid, name, checkStatusWarn, fmt.Sprintf("the hold on %s ended %s and no longer applies", hold.Name, hold.Until),
				"move the pin, or renew the hold with a new date and the reason it still stands")
		default:
			usable[i] = true
		}
	}
	return usable
}

// matchingHold finds the usable hold on p's exact pin.
func matchingHold(holds []pinHold, usable []bool, p *currencyPin) (*pinHold, int) {
	for i := range holds {
		if usable[i] && holds[i].Name == p.name && strings.TrimSpace(holds[i].Pin) == p.version {
			return &holds[i], i
		}
	}
	return nil, -1
}

// currencyPins lists the pins to judge — every pinned module and imported
// workspace of the composition — the names a hold may use, and the modules that
// run from a checkout instead of their pin. module narrows what is judged to
// one module, as a render does, plus the workspace that declares it; the names
// stay whole, so a hold on a module that is not being rendered is still valid.
func currencyPins(ctx context.Context, ws *resources.Workspace, module string) (pins []currencyPin, names []string, overrides []moduleOverride) {
	owner := ""
	if member, ok := ws.Member(module); ok && module != "" {
		owner = member.Workspace
	}
	for _, imported := range ws.Workspaces {
		if imported.Source == "" || imported.Version == "" {
			continue
		}
		names = append(names, imported.Name)
		if module == "" || imported.Name == owner {
			pins = append(pins, currencyPin{label: "workspace " + imported.Name, name: imported.Name, source: imported.Source, version: strings.TrimSpace(imported.Version)})
		}
	}
	for _, ref := range ws.Modules {
		if ref.Source == "" || ref.Version == "" {
			continue
		}
		names = append(names, ref.Name)
		if module != "" && ref.Name != module {
			continue
		}
		p := currencyPin{label: ref.Name, name: ref.Name, source: ref.Source, version: strings.TrimSpace(ref.Version)}
		if member, ok := ws.Member(ref.Name); ok && member.Workspace != ws.Name {
			p.declaredBy = member.Workspace
		}
		pins = append(pins, p)
	}
	return pins, names, moduleOverrides(ctx, ws, module)
}

func pinSources(pins []currencyPin) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, p.source)
	}
	return out
}

// moduleOverride is a pinned module that a checkout on this machine replaces.
type moduleOverride struct {
	name, source, version, kind, dir string
}

func overrideSources(overrides []moduleOverride) []string {
	out := make([]string, 0, len(overrides))
	for _, o := range overrides {
		out = append(out, o.source)
	}
	return out
}

// moduleOverrides asks the resolver — the same precedence a run uses — which
// pinned modules land on a checkout rather than on their pin. A path the CLI
// itself materialized for the pin (a resolution receipt) is the pin, not an
// override.
func moduleOverrides(ctx context.Context, ws *resources.Workspace, module string) []moduleOverride {
	overlay, err := resources.LoadLocalOverlay(ctx, ws.Dir())
	if err != nil {
		return nil
	}
	overlayDir := ws.Dir()
	if dir := composition.NearestOverlayDir(ws.Dir()); dir != "" {
		overlayDir = dir
	}
	receipts, err := composition.LoadResolutionReceipts(overlayDir)
	if err != nil {
		return nil
	}
	var out []moduleOverride
	for _, ref := range ws.Modules {
		if ref.Source == "" || (module != "" && ref.Name != module) {
			continue
		}
		directive := overlayDirective(overlay, ref.Name)
		if directive != nil && directive.Path != "" && directive.Path == receipts[ref.Name].ResolvedPath() {
			continue
		}
		resolution, err := ws.ResolveModule(ctx, ref)
		if err != nil || resolution.Dir == "" {
			continue
		}
		out = append(out, moduleOverride{ref.Name, ref.Source, ref.Version, string(resolution.Kind), resolution.Dir})
	}
	return out
}

// reportOverrides says, for every module running from a checkout, which
// checkout and commit that is and how it stands against its source: the answer
// to "is the code I am editing the code that runs?".
func reportOverrides(ctx context.Context, overrides []moduleOverride, sources map[string]*composition.SourceHead, report *workspaceReadinessReport) {
	for _, o := range overrides {
		name := "checkout of " + o.name
		head := sources[o.source]
		if head == nil {
			report.add(codeCheckoutOverride, name, checkStatusWarn, fmt.Sprintf("module %q runs from the %s override %s, not from its pin %s", o.name, o.kind, o.dir, o.version), "")
			continue
		}
		commit, lag, err := composition.CheckoutLag(ctx, o.source, head, o.dir)
		message := fmt.Sprintf("module %q runs from the %s override %s, not from its pin %s", o.name, o.kind, o.dir, o.version)
		status := checkStatusWarn
		switch {
		case err != nil:
			message += fmt.Sprintf("; its commit could not be read (%v)", err)
		case !lag.OnBranch:
			message += fmt.Sprintf("; the checkout is at %s, which is not in the history of %s (%s): local commits or an unmerged branch", commit[:8], head.Branch, head.Tip[:8])
		case lag.Behind == 0:
			status = "ok"
			message += fmt.Sprintf("; the checkout is at %s, the tip of %s", commit[:8], head.Branch)
		default:
			message += fmt.Sprintf("; the checkout is at %s, %s behind %s (%s)", commit[:8], plural(lag.Behind, "commit"), head.Branch, head.Tip[:8])
		}
		report.add(codeCheckoutOverride, name, status, message,
			fmt.Sprintf("delete resolve.%s from %s to run the pin, or keep the override knowingly", o.name, resources.LocalOverlayConfigurationName))
	}
}

// cliCurrency says which codefly is running and how old it is, reusing the
// self-update check (signed release metadata, cached against rate limits).
func cliCurrency(ctx context.Context) workspaceDiagnostic {
	build := cliupdate.CurrentBuildInfo()
	diagnostic := func(code, status, message, fix string) workspaceDiagnostic {
		return workspaceDiagnostic{Code: code, Name: "codefly CLI", Status: status, Message: message, Remediation: fix}
	}
	if !build.Released() {
		return diagnostic("", "ok", fmt.Sprintf("a development build (%s), not compared with a release", build.Version), "")
	}
	service, err := cliupdate.NewService()
	if err != nil {
		return diagnostic(codeCLIStale, checkStatusWarn, fmt.Sprintf("codefly %s: whether it is the newest release was NOT verified: %v", build.Version, err), "")
	}
	result, err := service.Check(ctx, releaseupdate.ChannelStable, false)
	if err != nil {
		return diagnostic(codeCLIStale, checkStatusWarn, fmt.Sprintf("codefly %s: whether it is the newest release was NOT verified: %v", build.Version, err), "")
	}
	age := ""
	if built, parseErr := time.Parse(time.RFC3339, build.BuildDate); parseErr == nil {
		age = fmt.Sprintf(", built %s (%d days ago)", built.Format("2006-01-02"), int(time.Since(built).Hours()/24))
	}
	if result.Available {
		return diagnostic(codeCLIStale, checkStatusWarn, fmt.Sprintf("codefly %s%s is behind the newest release, %s", build.Version, age, result.Latest),
			"run `codefly self update`; an old CLI renders and checks differently from the one the team is on")
	}
	return diagnostic("", "ok", fmt.Sprintf("codefly %s%s is the newest release", build.Version, age), "")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
