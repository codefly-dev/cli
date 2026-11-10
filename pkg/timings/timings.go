// Package timings says where a command's wall clock went.
//
// A span is a labelled stretch of work: `defer timings.Start("label")()`. When
// CODEFLY_TIMINGS is set the process prints, on exit, every label with the
// number of times it ran, the sum of its durations and its longest single run,
// ordered by sum. Unset, Start does nothing and costs one atomic load.
//
// Sums are of span durations, not of the process's wall clock: spans on
// concurrent goroutines overlap, and a span nested in another is counted in
// both. Read a row as "this much was spent inside this label", and compare
// rows of the same kind (the leaf probes with each other, the phases with each
// other).
package timings

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// Variable is the environment variable that turns the report on.
const Variable = "CODEFLY_TIMINGS"

var (
	enabled = os.Getenv(Variable) != ""
	mu      sync.Mutex
	totals  = map[string]*total{}
	began   = time.Now()
)

type total struct {
	count   int
	sum     time.Duration
	longest time.Duration
}

// Start opens a span and returns the function that closes it.
func Start(label string) func() {
	if !enabled {
		return func() {}
	}
	start := time.Now()
	return func() {
		elapsed := time.Since(start)
		mu.Lock()
		defer mu.Unlock()
		t := totals[label]
		if t == nil {
			t = &total{}
			totals[label] = t
		}
		t.count++
		t.sum += elapsed
		t.longest = max(t.longest, elapsed)
	}
}

// Report writes the table when timings are on.
func Report(out io.Writer) {
	if !enabled {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	labels := make([]string, 0, len(totals))
	for label := range totals {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool { return totals[labels[i]].sum > totals[labels[j]].sum })
	_, _ = fmt.Fprintf(out, "\ntimings (wall clock %s; sums overlap across goroutines and nested spans)\n", time.Since(began).Round(time.Second))
	_, _ = fmt.Fprintf(out, "%-44s %7s %12s %10s %10s\n", "label", "count", "sum", "mean", "longest")
	for _, label := range labels {
		t := totals[label]
		_, _ = fmt.Fprintf(out, "%-44s %7d %12s %10s %10s\n", label, t.count,
			t.sum.Round(time.Millisecond), (t.sum / time.Duration(t.count)).Round(time.Millisecond), t.longest.Round(time.Millisecond))
	}
}
