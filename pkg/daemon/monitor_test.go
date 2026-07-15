package daemon

import (
	"context"
	"testing"
	"time"
)

func TestProcessClassificationUsesExactExecutableNames(t *testing.T) {
	tests := []struct {
		name    string
		command string
		related bool
		extract string
	}{
		{
			name:    "agent executable",
			command: "/tmp/codefly/agents/go-grpc --port 1234",
			related: true,
			extract: "go-grpc",
		},
		{
			name:    "compiler source path is not an agent",
			command: "go build ./plugins/go-grpc/cmd/go-grpc",
			related: false,
			extract: "go",
		},
		{
			name:    "unrelated substring is not an agent",
			command: "/usr/bin/python test-go-grpc-worker.py",
			related: false,
			extract: "python",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodeflyRelatedProcess(tt.command); got != tt.related {
				t.Fatalf("isCodeflyRelatedProcess(%q) = %v, want %v", tt.command, got, tt.related)
			}
			if got := extractProcessName(tt.command); got != tt.extract {
				t.Fatalf("extractProcessName(%q) = %q, want %q", tt.command, got, tt.extract)
			}
		})
	}
}

func TestRunMonitorLoopStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunMonitorLoop(ctx, MonitorConfig{CheckInterval: time.Hour})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunMonitorLoop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunMonitorLoop ignored cancellation")
	}
}
