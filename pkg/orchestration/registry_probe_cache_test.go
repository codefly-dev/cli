package orchestration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCachedProbeReadsOnceAndForgetsAfterAPush(t *testing.T) {
	cache := newProbeCache()
	ctx := withProbes(context.Background(), cache)
	var reads atomic.Int32
	read := func() (string, error) { return "sha256:" + string(rune('a'+reads.Add(1))), nil }

	first, err := cachedProbe(ctx, "manifest", "registry/a:1", "", read)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := cachedProbe(ctx, "manifest", "registry/a:1", "", read)
	if first != second || reads.Load() != 1 {
		t.Fatalf("asked twice, read %d times: %q then %q", reads.Load(), first, second)
	}
	if _, _ = cachedProbe(ctx, "manifest", "registry/a:1", "other-builder", read); reads.Load() != 2 {
		t.Fatalf("another builder shares an answer: read %d times", reads.Load())
	}
	if _, _ = cachedProbe(ctx, "labels", "registry/a:1", "", func() (int, error) { return 7, nil }); reads.Load() != 2 {
		t.Fatal("a different question must not be answered from the manifest")
	}

	cache.pushing("registry/a:1")()
	third, _ := cachedProbe(ctx, "manifest", "registry/a:1", "", read)
	if third == first || reads.Load() != 3 {
		t.Fatalf("a push left the old answer in place: %q after %d reads", third, reads.Load())
	}
}

func TestCachedProbeNeverKeepsAFailure(t *testing.T) {
	ctx := withProbes(context.Background(), newProbeCache())
	var reads atomic.Int32
	failing := errors.New("not found")
	if _, err := cachedProbe(ctx, "manifest", "registry/b:1", "", func() (string, error) { reads.Add(1); return "", failing }); !errors.Is(err, failing) {
		t.Fatalf("the failure was not returned: %v", err)
	}
	got, err := cachedProbe(ctx, "manifest", "registry/b:1", "", func() (string, error) { reads.Add(1); return "sha256:b", nil })
	if err != nil || got != "sha256:b" || reads.Load() != 2 {
		t.Fatalf("a failure was remembered: %q, %v after %d reads", got, err, reads.Load())
	}
}

func TestCachedProbeSharesConcurrentReads(t *testing.T) {
	ctx := withProbes(context.Background(), newProbeCache())
	var reads atomic.Int32
	release := make(chan struct{})
	read := func() (string, error) { reads.Add(1); <-release; return "sha256:c", nil }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cachedProbe(ctx, "manifest", "registry/c:1", "", read); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if reads.Load() != 1 {
		t.Fatalf("eight concurrent identical probes read %d times", reads.Load())
	}
}

func TestAnAnswerReadAcrossAPushIsNotKept(t *testing.T) {
	cache := newProbeCache()
	ctx := withProbes(context.Background(), cache)
	var reads atomic.Int32
	got, _ := cachedProbe(ctx, "manifest", "registry/d:1", "", func() (string, error) {
		reads.Add(1)
		cache.pushing("registry/d:1")() // the push lands while the read is in flight
		return "sha256:before", nil
	})
	if got != "sha256:before" {
		t.Fatalf("the caller must still get its answer, got %q", got)
	}
	cachedProbe(ctx, "manifest", "registry/d:1", "", func() (string, error) { reads.Add(1); return "sha256:after", nil })
	if reads.Load() != 2 {
		t.Fatalf("an answer that predates a push was kept (%d reads)", reads.Load())
	}
}

// A push moves the tag from the moment it starts: an answer read before it is
// not served while it runs, and nothing read while it runs is kept, by it or
// after it.
func TestNoAnswerFromBeforeOrDuringAPushIsServedOnceItStarts(t *testing.T) {
	cache := newProbeCache()
	ctx := withProbes(context.Background(), cache)
	var reads atomic.Int32
	read := func() (string, error) { return "sha256:" + string(rune('a'+reads.Add(1))), nil }
	probe := func() string {
		got, err := cachedProbe(ctx, "manifest", "registry/f:1", "", read)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	before := probe()
	done := cache.pushing("registry/f:1")
	if during := probe(); during == before || reads.Load() != 2 {
		t.Fatalf("an answer from before the push was served while it ran: %q after %d reads", during, reads.Load())
	}
	if again := probe(); reads.Load() != 3 {
		t.Fatalf("an answer read while the push ran was kept: %q after %d reads", again, reads.Load())
	}
	done()
	done() // the end is idempotent: a second call must not end another push
	probe()
	if after := probe(); reads.Load() != 4 {
		t.Fatalf("after the push the answer must be read once and kept: %q after %d reads", after, reads.Load())
	}

	// Two overlapping pushes: the first one ending does not end the second.
	first, second := cache.pushing("registry/f:1"), cache.pushing("registry/f:1")
	first()
	probe()
	probe()
	if reads.Load() != 6 {
		t.Fatalf("an answer was kept while a second push was still running (%d reads)", reads.Load())
	}
	second()
}

// Two flows never share an answer: an image that vanished between two commands,
// or two flows in one, is seen as gone by the second.
func TestCachedProbeIsScopedToTheFlow(t *testing.T) {
	var reads atomic.Int32
	read := func() (string, error) { reads.Add(1); return "sha256:e", nil }
	for range 2 {
		ctx := withProbes(context.Background(), newProbeCache())
		_, _ = cachedProbe(ctx, "manifest", "registry/e:1", "", read)
	}
	_, _ = cachedProbe(context.Background(), "manifest", "registry/e:1", "", read)
	if reads.Load() != 3 {
		t.Fatalf("a probe outside any flow, or in another, was answered from a cache (%d reads)", reads.Load())
	}
}
