package run

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"golang.org/x/sync/semaphore"
)

var errDeliveryChanged = errors.New("module inputs or lifecycle changed before delivery settled")

func (cluster *launcher) frontendInputDigest(ctx context.Context, module *resources.Module) (string, error) {
	frontend, err := solutionrun.DeclaredFrontend(ctx, module)
	if err != nil || frontend == nil {
		return "", err
	}
	return (solutionrun.LocalFrontend{Docker: cluster.flow.ServiceRuntimeContext(resources.ServiceUnique(module.Name, module.ServiceEntry)) == resources.RuntimeContextContainer}).InputDigest(ctx, module)
}

func (cluster *launcher) checkFrontendInputs(ctx context.Context, module *resources.Module, expected string) error {
	current, err := cluster.frontendInputDigest(ctx, module)
	if err != nil {
		return err
	}
	if current != expected {
		return errDeliveryChanged
	}
	return nil
}

// moduleLane orders the existing admission, readiness and withdrawal callers.
// Its revision is only a scheduling fence; NativeDeliverer owns generations.
// All fields except gate are protected by launcher.mu.
type moduleLane struct {
	gate                          *semaphore.Weighted
	revision                      uint64
	changed                       chan struct{}
	terminal, admitted, scheduled bool
	inputs                        string
	watch                         *services.Base
}

func (cluster *launcher) laneLocked(module string) *moduleLane {
	if cluster.lanes == nil {
		cluster.lanes = map[string]*moduleLane{}
	}
	if cluster.lanes[module] == nil {
		cluster.lanes[module] = &moduleLane{gate: semaphore.NewWeighted(1), changed: make(chan struct{})}
	}
	return cluster.lanes[module]
}

func (cluster *launcher) invalidateLocked(module string) {
	lane := cluster.laneLocked(module)
	lane.revision++
	close(lane.changed)
	lane.changed = make(chan struct{})
}

type moduleFence struct {
	module   string
	revision uint64
}

// Gates are acquired in one order, without holding state/composition locks.
// Teardown cancels and drains every caller before closing transport or staging.
func (cluster *launcher) lockModules(parent context.Context, modules []string) (context.Context, []moduleFence, func(), error) {
	modules = append([]string(nil), modules...)
	sort.Strings(modules)
	cluster.mu.Lock()
	if cluster.closed {
		cluster.mu.Unlock()
		return parent, nil, nil, errRunStopped
	}
	cluster.active.Add(1)
	lanes := make([]*moduleLane, len(modules))
	for i, name := range modules {
		lanes[i] = cluster.laneLocked(name)
	}
	cluster.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-cluster.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	held := 0
	release := func() {
		for i := held - 1; i >= 0; i-- {
			lanes[i].gate.Release(1)
		}
		cancel()
		cluster.active.Done()
	}
	for _, lane := range lanes {
		if err := lane.gate.Acquire(ctx, 1); err != nil {
			release()
			return ctx, nil, nil, err
		}
		held++
	}
	cluster.mu.Lock()
	fences := make([]moduleFence, len(modules))
	for i, name := range modules {
		fences[i] = moduleFence{name, lanes[i].revision}
	}
	cluster.mu.Unlock()
	return ctx, fences, release, nil
}

// This read is the admission linearization point. Events after it remain dirty;
// they cannot revoke a network request already admitted to flight.
func (cluster *launcher) checkFences(fences []moduleFence, withdrawal bool) error {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if cluster.closed {
		return errRunStopped
	}
	for _, fence := range fences {
		lane := cluster.laneLocked(fence.module)
		if lane.terminal && !withdrawal {
			return errWorkloadGone
		}
		if !withdrawal && lane.watch != nil && lane.watch.Watcher.Err() != nil {
			return fmt.Errorf("frontend observation incomplete for %s: %w", fence.module, lane.watch.Watcher.Err())
		}
		if lane.revision != fence.revision {
			return errDeliveryChanged
		}
	}
	return nil
}

func (cluster *launcher) waitFrontendRepair(ctx context.Context, fence moduleFence, cause error) error {
	cluster.mu.Lock()
	lane := cluster.laneLocked(fence.module)
	watching, changed, current := lane.watch != nil, lane.changed, lane.revision
	cluster.mu.Unlock()
	if !watching {
		return cause
	}
	if current != fence.revision {
		return errDeliveryChanged
	}
	select {
	case <-changed:
		return errDeliveryChanged
	case <-ctx.Done():
		return fmt.Errorf("frontend repair did not complete during the pending admission; restart the owning run after repair: %w (build: %v)", ctx.Err(), cause)
	}
}
