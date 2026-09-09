package orchestration

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
	multierror "github.com/hashicorp/go-multierror"
)

// Budgets for one reverse-topological teardown layer. Because layers run one
// after the other, a whole Stop/Shutdown is bounded by its budget times the
// number of layers. Budgeting per layer rather than in aggregate matters: the
// dependencies that hold state (a database) are in the LAST layer, so a single
// aggregate deadline burned by slow consumers would leave nothing for exactly
// the resources whose clean stop matters most.
//
// Each budget must clear the ceiling the Runner already imposes on the
// operation, or a slow-but-healthy agent would be cut off mid-cleanup and leak
// what it was releasing: Runner.Stop bounds its RPC at 10s, and Runner.Destroy
// bounds its own RPC at 10s AFTER an implicit Stop that costs up to another
// 10s.
const (
	defaultStopPhaseBudget     = 15 * time.Second
	defaultShutdownPhaseBudget = 30 * time.Second
)

// teardownOutcome is what actually happened to one resource during a teardown.
type teardownOutcome string

const (
	// teardownStopped is a graceful drain: the agent acknowledged the call.
	teardownStopped teardownOutcome = "stopped"
	// teardownFailed is a drain the agent refused or errored out of. The
	// resource answered, so the flow knows its state.
	teardownFailed teardownOutcome = "failed"
	// teardownTimedOut is a drain abandoned when its layer's budget ran out.
	// The call is cancelled and the flow moves on to the next layer, so the
	// resource may still be running.
	teardownTimedOut teardownOutcome = "timed-out"
)

// teardownEntry is the per-resource receipt of one teardown operation.
type teardownEntry struct {
	Service  string
	Layer    int
	Duration time.Duration
	Outcome  teardownOutcome
	Err      error
}

// teardownReceipt records what a Stop or Shutdown did, resource by resource.
// Entries are ordered by layer and, within a layer, by the order the hub
// registered the resource — deterministic regardless of which goroutine
// finished first.
type teardownReceipt struct {
	Operation string
	Layers    int
	Duration  time.Duration
	Entries   []teardownEntry
}

// teardownUnit is one resource in a layer: the flow-unique service name and
// every manager the hub registered under it.
type teardownUnit struct {
	unique   string
	managers []IManager
}

// teardownLayers groups managers into reverse-topological layers. Layer 0 holds
// the consumers no other participating service depends on; each later layer
// becomes safe to stop only once every layer before it has finished. A manager
// with no edges among the participants — an excluded root's NoOpManager, or a
// dependency whose consumer never got created during a partial init — carries
// no consumer to wait on and therefore lands in layer 0.
func teardownLayers(managers []IManager, edges []architecture.ServiceDependency) [][]teardownUnit {
	var order []string
	units := map[string]*teardownUnit{}
	for _, manager := range managers {
		if manager == nil {
			continue
		}
		unique := manager.Unique()
		unit, known := units[unique]
		if !known {
			unit = &teardownUnit{unique: unique}
			units[unique] = unit
			order = append(order, unique)
		}
		unit.managers = append(unit.managers, manager)
	}

	// "X depends on Y" is the edge Y -> X, so a consumer is an edge's To side.
	requires := map[string][]string{}
	consumers := map[string]int{}
	seen := map[[2]string]bool{}
	for _, edge := range edges {
		dependency, consumer := edge.From.Unique, edge.To.Unique
		if dependency == consumer || units[dependency] == nil || units[consumer] == nil {
			continue
		}
		if seen[[2]string{dependency, consumer}] {
			continue
		}
		seen[[2]string{dependency, consumer}] = true
		requires[consumer] = append(requires[consumer], dependency)
		consumers[dependency]++
	}

	var layers [][]teardownUnit
	remaining := order
	for len(remaining) > 0 {
		var layer, held []string
		for _, unique := range remaining {
			if consumers[unique] == 0 {
				layer = append(layer, unique)
				continue
			}
			held = append(held, unique)
		}
		if len(layer) == 0 {
			// Only a cycle can starve every remaining resource. Tear the rest
			// down together rather than leaking it.
			layer, held = remaining, nil
		}
		batch := make([]teardownUnit, 0, len(layer))
		for _, unique := range layer {
			batch = append(batch, *units[unique])
			for _, dependency := range requires[unique] {
				consumers[dependency]--
			}
		}
		layers = append(layers, batch)
		remaining = held
	}
	return layers
}

func (flow *Flow) teardownLayers() [][]teardownUnit {
	var edges []architecture.ServiceDependency
	if flow.world != nil && flow.world.Dependencies != nil {
		edges = flow.world.Dependencies.Dependencies()
	}
	return teardownLayers(flow.hub.managers, edges)
}

func (flow *Flow) phaseBudget(fallback time.Duration) time.Duration {
	if flow.teardownPhaseBudget > 0 {
		return flow.teardownPhaseBudget
	}
	return fallback
}

// lastTeardownReceipt returns the receipt of the most recent Stop or Shutdown,
// or nil if this flow has not been torn down. The lock guards it against a
// second Stop/Shutdown running concurrently.
func (flow *Flow) lastTeardownReceipt() *teardownReceipt {
	if flow == nil {
		return nil
	}
	flow.teardownMu.Lock()
	defer flow.teardownMu.Unlock()
	return flow.lastTeardown
}

// teardownTask is one manager's in-flight teardown call. The lock guards the
// result against the reader that gives up on the task when its layer's budget
// expires.
type teardownTask struct {
	unique  string
	manager IManager

	mu       sync.Mutex
	finished bool
	forced   bool
	err      error
	duration time.Duration
}

// runTeardown walks the flow's reverse-topological layers, running every
// resource in a layer concurrently and waiting for that layer to settle before
// touching the layer it depends on. Launch order is not completion order, so
// the barrier — not the iteration direction — is what keeps a database alive
// until the API draining into it has finished.
//
// The barrier is exactly as strong as the agent's Stop contract: it guarantees
// the consumer's Stop RPC RETURNED before the dependency's began, so an agent
// that acks Stop before its process has actually exited still leaves a window.
// Closing that window here — polling the consumer's endpoints until they stop
// answering — would be worse than the gap: boundNativePorts reports a port held
// by ANY process, so a zombie from an earlier run squatting the same
// deterministically hashed port (the case Flow.begun exists to handle) would
// stall every teardown for a full layer budget. The durable fix belongs in the
// agent's Stop semantics, not in this loop.
func (flow *Flow) runTeardown(name string, budget time.Duration, invoke func(IManager, context.Context) (*OutputProperty, error)) error {
	// Don't call on a possibly Done context
	stoppedContext, done := flow.newTeardownContext()
	defer done()
	// Abandoned calls unwind as soon as their layer context is cancelled; wait
	// so no teardown goroutine outlives the call that started it.
	var inflight sync.WaitGroup
	defer inflight.Wait()

	w := wool.Get(stoppedContext).In("flow." + name)

	// Clear any stale pause state — if a paused action is still sitting in the
	// PauseManager, the spinner keeps spinning even as teardown runs. Force-
	// clear so the UI reflects reality.
	if flow.playbook != nil && flow.playbook.pause != nil {
		flow.playbook.pause.Clear()
	}

	layers := flow.teardownLayers()
	budget = flow.phaseBudget(budget)
	started := time.Now()
	receipt := &teardownReceipt{Operation: name, Layers: len(layers)}

	var res error
	for index, layer := range layers {
		var tasks []*teardownTask
		for _, unit := range layer {
			for _, manager := range unit.managers {
				tasks = append(tasks, &teardownTask{unique: unit.unique, manager: manager})
			}
		}
		w.Debug("tearing down layer", wool.Field("layer", index), wool.Field("resources", layerUniques(layer)))

		layerContext, cancelLayer := context.WithTimeout(stoppedContext, budget)
		layerDeadline, _ := layerContext.Deadline()
		layerStarted := time.Now()
		var running sync.WaitGroup
		for _, task := range tasks {
			running.Add(1)
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				defer running.Done()
				callStarted := time.Now()
				_, err := invoke(task.manager, layerContext)
				finishedAt := time.Now()
				task.mu.Lock()
				task.finished, task.err, task.duration = true, err, finishedAt.Sub(callStarted)
				// A call that only failed once the layer's deadline had passed
				// was cut short, not answered. Compare against the deadline
				// rather than layerContext.Err(): gRPC enforces the same
				// deadline on its own timer and can return first.
				task.forced = err != nil && !finishedAt.Before(layerDeadline)
				task.mu.Unlock()
			}()
		}
		settled := make(chan struct{})
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			running.Wait()
			close(settled)
		}()
		select {
		case <-settled:
		case <-layerContext.Done():
		}
		cancelLayer()

		for _, task := range tasks {
			entry := teardownEntry{Service: task.unique, Layer: index}
			task.mu.Lock()
			switch {
			case !task.finished || task.forced:
				entry.Outcome = teardownTimedOut
				entry.Duration = time.Since(layerStarted)
				entry.Err = fmt.Errorf("%s of %s did not drain within %s and was forced", name, task.unique, budget)
				if task.err != nil {
					entry.Err = fmt.Errorf("%w: %w", entry.Err, task.err)
				}
			case task.err != nil:
				entry.Outcome = teardownFailed
				entry.Duration = task.duration
				entry.Err = task.err
			default:
				entry.Outcome = teardownStopped
				entry.Duration = task.duration
			}
			task.mu.Unlock()
			if entry.Outcome == teardownTimedOut {
				// Callers such as the control plane's stopFlow discard the
				// returned error, and a forced resource may still be running.
				// Say so where the user actually looks.
				flow.output().Error("%s of %s was forced after %s: it may still be running",
					name, task.unique, budget)
			}
			if entry.Err != nil {
				w.Debug("got error", wool.ErrField(entry.Err))
				res = multierror.Append(res, entry.Err)
			}
			receipt.Entries = append(receipt.Entries, entry)
		}
		w.Debug("layer torn down", wool.Field("layer", index), wool.Field("took", time.Since(layerStarted).String()))
	}

	receipt.Duration = time.Since(started)
	flow.teardownMu.Lock()
	flow.lastTeardown = receipt
	flow.teardownMu.Unlock()
	return res
}

func layerUniques(layer []teardownUnit) []string {
	out := make([]string, 0, len(layer))
	for _, unit := range layer {
		out = append(out, unit.unique)
	}
	return out
}
