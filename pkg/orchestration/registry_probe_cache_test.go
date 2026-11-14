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

	cache.forget("registry/a:1")
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
		cache.forget("registry/d:1") // the push lands while the read is in flight
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
