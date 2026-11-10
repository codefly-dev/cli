package orchestration

import (
	"context"
	"strconv"
	"sync"

	"golang.org/x/sync/singleflight"
)

// probeCache remembers what one flow has already read from a registry.
//
// Every probe is a `docker buildx imagetools inspect` process and a network
// round trip, a couple of seconds each, and the reuse decision makes several
// per recipe. One render decides the same recipe once per root that reaches it,
// and every recipe built FROM the same base asks the registry about that base
// again: the same question, asked of the same registry within one command.
//
// What is kept is what makes the answer the same the second time:
//
//   - only an answer, never a failure. "Not found" is the reason to build, and a
//     build is a push; a timeout or an auth refusal is not a fact about the image.
//   - the answer to one reference as the registry gave it. A tag is a claim that
//     moves when this process pushes it, so a push forgets every answer about
//     the reference it wrote (forget), and an answer read while
//     that push was happening is not kept. A reference pinned by digest cannot move.
//   - one question in flight at a time: concurrent identical probes share the
//     single read instead of each starting a process.
//
// The cache belongs to the flow (World) and dies with it. Nothing is written to
// disk, so a registry change between two commands, or two flows, is always seen
// by the second.

type probeKey struct {
	kind      string
	reference string
	builder   string
}

type probeCache struct {
	mu      sync.Mutex
	answers map[probeKey]any
	// written counts the pushes of each reference. A read that began before a
	// push of its reference finished is stale, whatever it returns.
	written map[string]uint64
	flight  singleflight.Group
}

func newProbeCache() *probeCache {
	return &probeCache{answers: map[probeKey]any{}, written: map[string]uint64{}}
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
		return answer.(T), nil
	}
	pushes := c.written[reference]
	c.mu.Unlock()

	value, err, _ := c.flight.Do(kind+"\x00"+reference+"\x00"+builder+"\x00"+strconv.FormatUint(pushes, 10), func() (any, error) {
		answer, err := read()
		if err != nil {
			return answer, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.written[reference] == pushes {
			c.answers[key] = answer
		}
		return answer, nil
	})
	if err != nil {
		var none T
		return none, err
	}
	return value.(T), nil
}

// forget drops every answer about reference: this flow has just pushed it, so
// what the registry said before is no longer what it holds.
func (c *probeCache) forget(reference string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written[reference]++
	for key := range c.answers {
		if key.reference == reference {
			delete(c.answers, key)
		}
	}
}
