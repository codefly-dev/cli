package orchestration

import (
	"sync"
	"testing"
)

// TestPrefetchesAreCreatedOnceUnderConcurrency pins the guards on goModules
// and gitSources. The plan phase runs its services concurrently, so without a
// sync.Once two goroutines each saw nil and each built a prefetch: the same
// modules were fetched twice and the losing assignment's temp directory
// leaked. Run with -race; before the guards this reported a data race.
func TestPrefetchesAreCreatedOnceUnderConcurrency(t *testing.T) {
	world := &World{}
	const goroutines = 8

	var wg sync.WaitGroup
	mods := make([]*goModulePrefetch, goroutines)
	gits := make([]*gitSourcePrefetch, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mods[i] = world.goModules()
			gits[i] = world.gitSources()
		}(i)
	}
	wg.Wait()

	for i := 1; i < goroutines; i++ {
		if mods[i] != mods[0] {
			t.Errorf("goModules returned %d distinct prefetches; every caller must share one", i)
			break
		}
	}
	for i := 1; i < goroutines; i++ {
		if gits[i] != gits[0] {
			t.Errorf("gitSources returned %d distinct prefetches; every caller must share one", i)
			break
		}
	}
	if mods[0] == nil || gits[0] == nil {
		t.Fatal("a prefetch was nil")
	}
}
