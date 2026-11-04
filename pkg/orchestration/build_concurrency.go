package orchestration

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// A whole-cell render spends nearly all of its wall clock in one place: the
// snapshot's build phase, running `docker buildx` once per service. Those builds
// are independent of each other — every input one reads is its own, every
// artifact one writes is named by its own service, and the agent process behind
// each is that service's alone (services.Load keys its instances by service,
// not by agent) — so the only reason they ran one at a time was that the
// snapshot policy emitted one action at a time.
//
// They are bounded rather than unbounded because each build is itself parallel:
// a Go image build compiles with every core the engine will give it, so past a
// handful of concurrent builds the host stops going faster and starts paging.
// The bound is the host's CPUs halved and clamped.
//
// Where to clamp was measured on a real 25-service render whose build phase
// took 56.9 minutes with nothing overlapping. Replaying those durations through
// the playbook (build_makespan_test.go) gives 37.1 minutes at 2 concurrent
// builds, 29.5 at 3, 25.7 at 4, 22.4 at 6 and 20.5 at 8 — and that render's
// longest single build, 17 minutes on its own, is a floor no bound can go
// under. So the curve has flattened well before the host runs out of memory,
// and the returns past 4 are small enough not to be worth taking by default on
// a machine the CLI knows nothing about. An operator who knows theirs raises
// it.
//
// One thing that bound does NOT fix, said here so the next person does not
// read the numbers as the end of the story: real concurrent builds contend.
// Eight real Go-compile image builds measured 94s one at a time and 35s four at
// a time, so 2.69x where the schedule alone predicted 3.9x.
//
// The numbers above are also the makespan of a phase dispatched in the plan's
// topological order, which starts that 17-minute build last. A phase dispatched
// longest-recorded-build first reaches 17.0 minutes at four at once — the floor
// — which is more than raising the bound to eight buys. That is where the
// render's time went and build_dispatch.go is how it is taken back; the bound
// here only decides how many builds may be in flight, not which ones start.
const (
	// buildConcurrencyEnv pins the number of image builds a build phase runs at
	// once. It is read per build phase, so it needs no plumbing through every
	// command that builds: the bound is a property of the machine, not of the
	// render that was asked for. 0 or 1 restores one-at-a-time building.
	buildConcurrencyEnv = "CODEFLY_BUILD_CONCURRENCY"
	// buildConcurrencyCeiling caps the derived default. See above: a render's
	// makespan stops improving well before this on the workloads measured, and
	// each concurrent build holds a build context in the engine.
	buildConcurrencyCeiling = 4
	// buildOutputCapBytes bounds one captured build transcript. A buildx
	// transcript is tens of kilobytes, so this only ever trips on pathological
	// output, and when it does the tail is kept because that is where a failure
	// says why.
	buildOutputCapBytes = 8 << 20
)

// buildConcurrency is how many image builds one build phase may run at once.
// An unparsable or negative override is refused rather than guessed at, so a
// typo in the variable does not silently serialize a render the operator
// thought they had parallelized.
func buildConcurrency() (int, error) {
	if raw, ok := os.LookupEnv(buildConcurrencyEnv); ok {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("%s=%q is not a number of concurrent builds", buildConcurrencyEnv, raw)
		}
		if value < 0 {
			return 0, fmt.Errorf("%s=%d is negative", buildConcurrencyEnv, value)
		}
		if value == 0 {
			return 1, nil
		}
		return value, nil
	}
	return defaultBuildConcurrency(runtime.GOMAXPROCS(0)), nil
}

// defaultBuildConcurrency derives the bound from the cores the process may use:
// half of them, because every build is itself parallel, clamped to at least one
// and at most buildConcurrencyCeiling.
func defaultBuildConcurrency(cpus int) int {
	bound := cpus / 2
	if bound < 1 {
		return 1
	}
	if bound > buildConcurrencyCeiling {
		return buildConcurrencyCeiling
	}
	return bound
}

// buildTranscript captures one service's build output while other services are
// building at the same time.
//
// Interleaving several buildx progress streams into one terminal produces
// output that names no service and reads as neither, so a build that shares the
// phase with others writes here instead and the transcript is replayed, framed
// by the service it belongs to, if the build fails. A build that succeeded is
// described by the completion line the builder already logs — its duration and
// its image — and its transcript is dropped: keeping it would bury the one line
// per service that the operator is reading for.
//
// Write is called from the goroutine running one build's docker command (and,
// because exec wires the same writer to stdout and stderr, from the two copier
// goroutines exec starts for it), so it locks.
type buildTranscript struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	dropped int
}

func (t *buildTranscript) Write(payload []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buffer.Write(payload)
	if excess := t.buffer.Len() - buildOutputCapBytes; excess > 0 {
		t.buffer.Next(excess)
		t.dropped += excess
	}
	return len(payload), nil
}

// Report renders the transcript framed by the service and image it belongs to,
// or "" when nothing was captured.
func (t *buildTranscript) Report(service, image string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buffer.Len() == 0 {
		return ""
	}
	var report bytes.Buffer
	fmt.Fprintf(&report, "---- %s build output (%s) ----\n", service, image)
	if t.dropped > 0 {
		fmt.Fprintf(&report, "[%d earlier bytes dropped; the end of the output is kept]\n", t.dropped)
	}
	report.Write(t.buffer.Bytes())
	fmt.Fprintf(&report, "---- end of %s build output ----\n", service)
	return report.String()
}
