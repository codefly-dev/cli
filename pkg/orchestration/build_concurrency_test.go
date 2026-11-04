package orchestration

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultBuildConcurrencyClampsToTheHost(t *testing.T) {
	// Half the cores, because every image build is itself parallel, and never
	// below one or above the ceiling.
	for _, test := range []struct{ cpus, want int }{
		{cpus: 0, want: 1},
		{cpus: 1, want: 1},
		{cpus: 2, want: 1},
		{cpus: 4, want: 2},
		{cpus: 8, want: 4},
		{cpus: 16, want: 4},
		{cpus: 128, want: 4},
	} {
		t.Run(fmt.Sprintf("cpus=%d", test.cpus), func(t *testing.T) {
			require.Equal(t, test.want, defaultBuildConcurrency(test.cpus))
		})
	}
}

func TestBuildConcurrencyOverride(t *testing.T) {
	t.Run("a pin is honoured", func(t *testing.T) {
		t.Setenv(buildConcurrencyEnv, "7")
		bound, err := buildConcurrency()
		require.NoError(t, err)
		require.Equal(t, 7, bound)
	})

	t.Run("zero restores one build at a time", func(t *testing.T) {
		t.Setenv(buildConcurrencyEnv, "0")
		bound, err := buildConcurrency()
		require.NoError(t, err)
		require.Equal(t, 1, bound)
	})

	// A typo must not quietly serialize a render the operator believed they had
	// parallelized, nor quietly parallelize one they had pinned.
	for _, raw := range []string{"", "lots", "4.5", "-1"} {
		t.Run("refuses "+raw, func(t *testing.T) {
			t.Setenv(buildConcurrencyEnv, raw)
			_, err := buildConcurrency()
			require.Error(t, err)
			require.Contains(t, err.Error(), buildConcurrencyEnv)
		})
	}
}

func TestBuildTranscriptReportsUnderItsService(t *testing.T) {
	transcript := &buildTranscript{}
	_, err := transcript.Write([]byte("#1 [internal] load build definition\n"))
	require.NoError(t, err)
	_, err = transcript.Write([]byte("ERROR: failed to solve\n"))
	require.NoError(t, err)

	report := transcript.Report("robin/chat", "registry/robin/chat:0.1.0")
	require.Contains(t, report, "robin/chat build output")
	require.Contains(t, report, "registry/robin/chat:0.1.0")
	require.Contains(t, report, "ERROR: failed to solve")
	require.Contains(t, report, "end of robin/chat build output")
}

func TestBuildTranscriptOfASilentBuildIsEmpty(t *testing.T) {
	silent := &buildTranscript{}
	require.Empty(t, silent.Report("a/b", "image"))
}

func TestBuildTranscriptKeepsTheEndOfRunawayOutput(t *testing.T) {
	transcript := &buildTranscript{}
	// Well past the cap, so the early output is dropped: a failure says why at
	// the end, which is the part that must survive.
	chunk := strings.Repeat("x", 1<<20)
	for range 10 {
		_, err := transcript.Write([]byte(chunk))
		require.NoError(t, err)
	}
	_, err := transcript.Write([]byte("ERROR: the real cause\n"))
	require.NoError(t, err)

	report := transcript.Report("a/b", "image")
	require.Contains(t, report, "ERROR: the real cause")
	require.Contains(t, report, "bytes dropped")
	require.Less(t, len(report), 9<<20)
}

// exec wires one writer to both stdout and stderr, so a single build already
// writes to its transcript from two copier goroutines.
func TestBuildTranscriptTakesConcurrentWrites(t *testing.T) {
	transcript := &buildTranscript{}
	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for range 64 {
				_, _ = transcript.Write([]byte("line of build output\n"))
			}
		}()
	}
	waiting.Wait()
	require.Equal(t, 8*64, strings.Count(transcript.Report("a/b", "image"), "line of build output"))
}
