package mcp

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSynchronizedBufferConcurrentSnapshot(t *testing.T) {
	var buffer synchronizedBuffer
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 1_000; i++ {
				_, _ = fmt.Fprintf(&buffer, "%d:%d\n", worker, i)
				_ = buffer.String()
			}
		}(worker)
	}
	wg.Wait()
	if got := buffer.String(); !strings.Contains(got, "0:0\n") {
		t.Fatalf("buffer lost written output")
	}
}
