package orchestration

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"golang.org/x/sync/singleflight"
)

// probeCache remembers what one flow has already read from a registry.
//
// Every probe is a network round trip (registry_client.go), or a `docker
// buildx imagetools inspect` process on a caller-named builder, and the reuse
// decision makes several per recipe. One render decides the same recipe once per root that reaches it,
// and every recipe built FROM the same base asks the registry about that base
// again: the same question, asked of the same registry within one command.
//
// What is kept is what makes the answer the same the second time:
//
//   - only an answer, never a failure. "Not found" is the reason to build, and a
//     build is a push; a timeout or an auth refusal is not a fact about the image.
//   - the answer to one reference as the registry gave it. A tag is a claim that
//     moves when this process pushes it, so a push forgets every answer about
//     the reference it writes when it starts and again when it ends (pushing),
//     and an answer read before or while that push was happening is never kept.
//     A reference pinned by digest cannot move.
//   - one question in flight at a time: concurrent identical probes share the
//     single read instead of each starting a process.
//
// The cache belongs to the flow (World) and dies with it. Nothing it holds is
// written to disk, so a registry change between two commands, or two flows, is
// always seen by the second; only what a manifest digest's bytes say is kept
// across commands (digestLabels), because those bytes cannot change.

type probeKey struct {
	kind      string
	reference string
	builder   string
}

type probeCache struct {
	mu      sync.Mutex
	answers map[probeKey]any
	// written counts the starts and ends of each reference's pushes. A read
	// that began before either is stale, whatever it returns.
	written map[string]uint64
	// active counts the pushes of each reference in progress. Nothing read
	// while one is running is kept: the registry may hold either image.
	active map[string]int
	flight singleflight.Group
}

func newProbeCache() *probeCache {
	return &probeCache{answers: map[probeKey]any{}, written: map[string]uint64{}, active: map[string]int{}}
}

type probeCacheKey struct{}

// withProbes carries the flow's cache to the registry reads below it.
func withProbes(ctx context.Context, c *probeCache) context.Context {
	return context.WithValue(ctx, probeCacheKey{}, c)
}

func probesFrom(ctx context.Context) *probeCache {
	c, _ := ctx.Value(probeCacheKey{}).(*probeCache)
	return c
}

// registryProbes is the flow's cache, made on first use.
func (world *World) registryProbes() *probeCache {
	world.probesMu.Lock()
	defer world.probesMu.Unlock()
	if world.probes == nil {
		world.probes = newProbeCache()
	}
	return world.probes
}

// cachedProbe returns the answer to (kind, reference, builder), reading it with
// read the first time. The builder is part of the key because a registry
// reachable only from one builder answers differently on another.
func cachedProbe[T any](ctx context.Context, kind, reference, builder string, read func() (T, error)) (T, error) {
	c := probesFrom(ctx)
	if c == nil {
		return read()
	}
	key := probeKey{kind: kind, reference: reference, builder: builder}
	c.mu.Lock()
	if answer, ok := c.answers[key]; ok {
		c.mu.Unlock()
		return typedProbe[T](answer, kind, reference)
	}
	pushes := c.written[reference]
	c.mu.Unlock()

	value, err, _ := c.flight.Do(kind+"\x00"+reference+"\x00"+builder+"\x00"+strconv.FormatUint(pushes, 10), func() (any, error) {
		// Missing the answer above and joining a flight are two steps: a probe
		// that missed while another's read was in flight can arrive here after
		// that flight has finished and stored its answer. Ask again before reading,
		// or that probe reads the registry a second time for an answer it holds.
		c.mu.Lock()
		answer, ok := c.answers[key]
		c.mu.Unlock()
		if ok {
			return answer, nil
		}
		answer, err := read()
		if err != nil {
			return answer, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.written[reference] == pushes && c.active[reference] == 0 {
			c.answers[key] = answer
		}
		return answer, nil
	})
	if err != nil {
		var none T
		return none, err
	}
	return typedProbe[T](value, kind, reference)
}

// typedProbe reads a remembered answer back at the type this caller asks for.
// A mismatch is a programming error — two probes sharing a key with different
// answer types — and it is returned rather than panicked, so a render says
// which probe disagreed instead of dying in a goroutine.
func typedProbe[T any](answer any, kind, reference string) (T, error) {
	typed, ok := answer.(T)
	if !ok {
		var none T
		return none, fmt.Errorf("the %s probe of %s remembered a %T, which is not what this caller reads", kind, reference, answer)
	}
	return typed, nil
}

// pushing marks reference as being pushed by this flow until the returned
// function is called, which the pusher does whether the push succeeded or not.
// Both the start and the end drop every answer about reference: from the start
// the registry may already hold the new image, so an answer read before it
// begins is never served after, and one read while it runs is never kept.
func (c *probeCache) pushing(reference string) (done func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[reference]++
	c.forgetLocked(reference)
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.active[reference]--; c.active[reference] == 0 {
				delete(c.active, reference)
			}
			c.forgetLocked(reference)
		})
	}
}

// forgetLocked drops every answer about reference and makes every read of it
// already in flight stale. c.mu is held.
func (c *probeCache) forgetLocked(reference string) {
	c.written[reference]++
	for key := range c.answers {
		if key.reference == reference {
			delete(c.answers, key)
		}
	}
}
