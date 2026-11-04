package orchestration

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
)

// A bounded build phase finishes when its last build finishes, so which build
// is started last decides the phase's wall clock. The plan hands the phase over
// in topological order, which on a real render starts the longest build last:
// `deploy gitops render --all --env blue` built 25 images, the longest of them
// 1020s, and reached that build only after the other 24 had been dispatched.
// Replaying those durations (build_makespan_test.go) gives 25.7 minutes at four
// at once dispatched in plan order and 17.0 minutes dispatched longest first —
// which is the floor set by that single 1020s build, and a larger win than
// raising the bound to eight (20.5 minutes).
//
// That is Longest Processing Time first, and it needs an estimate of each
// build's duration before the build runs. The estimate comes from this machine:
// every buildx invocation that completes records how long it took, by service,
// and the next phase on the same workspace orders by what it finds.
//
// Why here and not in the image reuse cache, which is the other per-build
// record and already writes one file per identity by rename (image_cache.go).
// That cache is keyed by a digest over the build's *inputs*, and the keys it
// holds are the keys of builds that have already run: a build only runs when
// its key misses, so a duration stored under that key is readable exactly when
// the build it describes is about to be skipped, and never when it is about to
// run. Reading the whole directory and grouping its entries by service would
// get around that, at the cost of a scan proportional to every build the
// workspace has ever done and of only covering builds the cache found
// recordable. A duration is also neither an input of an image nor proof that
// one exists, which is the entire content of that cache's contract.
//
// Why not a static proxy with no history at all — context size, recipe kind,
// layer count. It would work on a cold machine, which this does not, but no
// measurement here supports one: the 25 durations are the only data, and none
// of them is paired with a context size or a recipe kind. Ranking 25 builds
// that span 6.8s to 1020s on an unvalidated correlation is a guess, and a wrong
// guess here is worse than no guess, because it displaces the plan order that
// is already known not to regress.
//
// A duration is a hint and nothing else. Nothing a render records, signs or
// writes to deployments/ may depend on it: the playbook replays all of its
// bookkeeping in the group's order whatever order the builds were started in
// (workConcurrently), so a machine with history and a machine without produce
// the same artifact from the same plan.
const (
	// buildDurationsDir is the workspace-relative directory the records live
	// in, beside the image reuse cache's own. `.codefly/` is CLI-owned scratch,
	// gitignored by core's composition and excluded from a build context
	// digest, so recording here cannot perturb the inputs of the next build.
	buildDurationsDir = ".codefly/build-durations"
	// buildDurationSchema is written into every record, so a change to what a
	// record means retires the ones written under the old definition rather
	// than reading them as if nothing had moved.
	buildDurationSchema = 1
)

// buildDurationRecord is how long one service's image build took, the last time
// one ran on this machine.
//
// The last duration and not an average over several: the ordering needs a rank
// and not a prediction, and the only measurement of what building a service
// costs now is the last build of it that actually ran. Builds the reuse cache
// stood in for never reach the recorder, so a configuration-only render — where
// every service is reused in seconds — leaves every record as it was instead of
// overwriting the durations of real builds with the cost of not building.
//
// It is a duration under whatever contention the phase it ran in had, not a
// solo one: a build sharing four cores with three others takes longer than the
// same build alone. That inflates every record of a phase together, which a
// rank is indifferent to, and it is the reason a record is never read as a
// prediction of how long the next build will take.
type buildDurationRecord struct {
	Schema   int     `json:"schema"`
	Service  string  `json:"service"`
	Seconds  float64 `json:"seconds"`
	Recorded string  `json:"recorded_at"`
}

// buildDurations is the workspace-local store of records. An empty root
// disables it: a flow with no workspace directory has nowhere to keep one, and
// dispatches in plan order.
type buildDurations struct {
	root string
}

// buildDurations is this workspace's record of what each service's image build
// costs. A world without a workspace returns a disabled store rather than nil,
// so every caller reads it the same way.
func (world *World) buildDurations() *buildDurations {
	if world == nil || world.Workspace == nil {
		return &buildDurations{}
	}
	dir := world.Workspace.Dir()
	if dir == "" {
		return &buildDurations{}
	}
	return &buildDurations{root: filepath.Join(dir, filepath.FromSlash(buildDurationsDir))}
}

func (durations *buildDurations) enabled() bool {
	return durations != nil && durations.root != ""
}

// path is one service's record. A service unique is a relative path of
// validated components (`module/service`), so it nests under the root as it
// does in the workspace — legible to whoever goes looking, and incapable of
// colliding two services onto one name the way flattening the separator would.
// Anything that is not such a path has no record, which costs the dispatch
// order a hint and nothing else.
func (durations *buildDurations) path(service string) (string, bool) {
	if service == "" || !filepath.IsLocal(service) {
		return "", false
	}
	return filepath.Join(durations.root, filepath.FromSlash(service)+".json"), true
}

// record remembers how long this service's build took.
//
// One file per service, written by rename, for the reason the reuse cache gives
// for its own layout: the builds of a phase finish concurrently, so a shared
// index would have several of them writing it at once.
func (durations *buildDurations) record(ctx context.Context, service string, took time.Duration) error {
	if !durations.enabled() || took <= 0 {
		return nil
	}
	path, ok := durations.path(service)
	if !ok {
		return nil
	}
	payload, err := json.MarshalIndent(buildDurationRecord{
		Schema:   buildDurationSchema,
		Service:  service,
		Seconds:  took.Seconds(),
		Recorded: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, path, append(payload, '\n'), 0o644)
}

// seconds is what this service's build last took, and ok=false when nothing
// usable is recorded for it.
//
// A record is not an interior value: it was written by an earlier run, possibly
// by an older CLI, so the fields the ordering reads are checked rather than
// trusted. Everything that does not check out is absent, not zero — a service
// with no usable record keeps its place in the plan, where a zero would send it
// to the back of the phase.
func (durations *buildDurations) seconds(service string) (float64, bool) {
	if !durations.enabled() {
		return 0, false
	}
	path, ok := durations.path(service)
	if !ok {
		return 0, false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var record buildDurationRecord
	if err = json.Unmarshal(payload, &record); err != nil {
		return 0, false
	}
	if record.Schema != buildDurationSchema || record.Service != service || record.Seconds <= 0 {
		return 0, false
	}
	return record.Seconds, true
}

// known is what this store has recorded for the named services, leaving out
// the ones it has nothing usable for.
func (durations *buildDurations) known(services []string) map[string]float64 {
	known := make(map[string]float64, len(services))
	for _, service := range services {
		if seconds, ok := durations.seconds(service); ok {
			known[service] = seconds
		}
	}
	return known
}

// dispatchOrder is the order the playbook starts a concurrent group's actions
// in: the indices of actions, longest recorded build first.
//
// It reorders starts and nothing else. The caller keeps every result at its
// action's own index, so what is recorded, attributed, signalled and queued is
// a function of the group it was given and not of this permutation.
//
// A service with no recorded duration is taken to be typical rather than free:
// it is weighted at the median of the services that do have one, so it sorts
// into the middle of the phase instead of behind every six-second build the
// machine happens to remember. With no records at all — a machine rendering for
// the first time — there is no median, every weight is equal, and the stable
// sort returns the plan's own order untouched. That is the cold-machine floor:
// no history dispatches exactly as topological order does today.
func (playbook *Playbook) dispatchOrder(ctx context.Context, actions []Action) []int {
	services := make([]string, 0, len(actions))
	for _, action := range actions {
		services = append(services, action.Service)
	}
	known := playbook.world.buildDurations().known(services)
	order := dispatchOrder(actions, known)
	if len(known) > 0 {
		// Said out loud so an operator reading a render can see which build
		// went first, and so a phase that took its serial time is diagnosable
		// as "nothing was timed yet" rather than left to be guessed at.
		wool.Get(ctx).In("work.dispatch").Info("starting the longest recorded builds first",
			wool.Field("first", actions[order[0]].Service),
			wool.Field("timed", len(known)), wool.Field("of", len(actions)))
	}
	return order
}

func dispatchOrder(actions []Action, known map[string]float64) []int {
	order := make([]int, len(actions))
	for index := range order {
		order[index] = index
	}
	if len(known) == 0 {
		return order
	}
	typical := medianDuration(known)
	weight := func(index int) float64 {
		if seconds, ok := known[actions[index].Service]; ok {
			return seconds
		}
		return typical
	}
	slices.SortStableFunc(order, func(left, right int) int {
		return cmp.Compare(weight(right), weight(left))
	})
	return order
}

// medianDuration is the middle of the recorded durations, which stands in for a
// service that has none. The lower of the two middle values on an even count:
// which one is taken changes nothing this is used for, and taking a value that
// was actually measured keeps it a duration rather than an average of two.
func medianDuration(known map[string]float64) float64 {
	seconds := make([]float64, 0, len(known))
	for _, value := range known {
		seconds = append(seconds, value)
	}
	if len(seconds) == 0 {
		return 0
	}
	slices.Sort(seconds)
	return seconds[(len(seconds)-1)/2]
}
