package executiondispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/executionjournal"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestDispatcherIsolatesExporterFailureAndPreservesOrder(t *testing.T) {
	journal := newMemoryJournal("receipt-1", "receipt-2")
	var wardenReceipts []string
	warden := exporterClientFunc(func(
		_ context.Context,
		request *executionv1.ExportExecutionRequest,
		_ ...grpc.CallOption,
	) (*executionv1.ExportExecutionResponse, error) {
		receiptID := request.GetAttestation().GetReceipt().GetReceiptId()
		wardenReceipts = append(wardenReceipts, receiptID)
		return &executionv1.ExportExecutionResponse{ReceiptId: receiptID}, nil
	})
	auditAvailable := false
	var auditReceipts []string
	audit := exporterClientFunc(func(
		_ context.Context,
		request *executionv1.ExportExecutionRequest,
		_ ...grpc.CallOption,
	) (*executionv1.ExportExecutionResponse, error) {
		if !auditAvailable {
			return nil, errors.New("offline")
		}
		receiptID := request.GetAttestation().GetReceipt().GetReceiptId()
		auditReceipts = append(auditReceipts, receiptID)
		return &executionv1.ExportExecutionResponse{ReceiptId: receiptID}, nil
	})
	dispatcher, err := New(Config{
		Journal: journal,
		Exporters: []Exporter{
			{ID: "warden.execution", Client: warden},
			{ID: "audit.archive", Client: audit},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := dispatcher.DispatchOnce(t.Context())
	if err == nil {
		t.Fatal("offline exporter did not surface an error")
	}
	if len(report.Exporters) != 2 ||
		report.Exporters[0].Accepted != 2 ||
		report.Exporters[1].Attempted != 1 ||
		report.Exporters[1].Accepted != 0 {
		t.Fatalf("first report=%+v", report)
	}
	if got := journal.pendingIDs("warden.execution"); len(got) != 0 {
		t.Fatalf("warden pending=%v", got)
	}
	if got := journal.pendingIDs("audit.archive"); !equalStrings(got, []string{"receipt-1", "receipt-2"}) {
		t.Fatalf("audit pending=%v", got)
	}

	auditAvailable = true
	report, err = dispatcher.DispatchOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Exporters[0].Attempted != 0 || report.Exporters[1].Accepted != 2 {
		t.Fatalf("second report=%+v", report)
	}
	if !equalStrings(wardenReceipts, []string{"receipt-1", "receipt-2"}) ||
		!equalStrings(auditReceipts, []string{"receipt-1", "receipt-2"}) {
		t.Fatalf("warden=%v audit=%v", wardenReceipts, auditReceipts)
	}
}

func TestDispatcherRejectsMismatchedAcknowledgementWithoutAdvancing(t *testing.T) {
	journal := newMemoryJournal("receipt-1")
	dispatcher, err := New(Config{
		Journal: journal,
		Exporters: []Exporter{{
			ID: "warden.execution",
			Client: exporterClientFunc(func(
				context.Context,
				*executionv1.ExportExecutionRequest,
				...grpc.CallOption,
			) (*executionv1.ExportExecutionResponse, error) {
				return &executionv1.ExportExecutionResponse{ReceiptId: "another-receipt"}, nil
			}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.DispatchOnce(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched acknowledgement error=%v", err)
	}
	if got := journal.pendingIDs("warden.execution"); !equalStrings(got, []string{"receipt-1"}) {
		t.Fatalf("pending after mismatched acknowledgement=%v", got)
	}
}

func TestDispatcherBoundsHungExporterAndRunStopsWithContext(t *testing.T) {
	journal := newMemoryJournal("receipt-1")
	dispatcher, err := New(Config{
		Journal: journal,
		Exporters: []Exporter{{
			ID: "hung.exporter",
			Client: exporterClientFunc(func(
				ctx context.Context,
				_ *executionv1.ExportExecutionRequest,
				_ ...grpc.CallOption,
			) (*executionv1.ExportExecutionResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}),
		}},
		ExportTimeout: 20 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := dispatcher.DispatchOnce(t.Context()); err == nil {
		t.Fatal("hung exporter returned no error")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("export timeout was not bounded: %s", elapsed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop after cancellation")
	}
}

type exporterClientFunc func(
	context.Context,
	*executionv1.ExportExecutionRequest,
	...grpc.CallOption,
) (*executionv1.ExportExecutionResponse, error)

func (function exporterClientFunc) Export(
	ctx context.Context,
	request *executionv1.ExportExecutionRequest,
	options ...grpc.CallOption,
) (*executionv1.ExportExecutionResponse, error) {
	return function(ctx, request, options...)
}

type memoryJournal struct {
	mu      sync.Mutex
	entries []executionjournal.Entry
	acks    map[string]map[string]struct{}
}

func newMemoryJournal(receiptIDs ...string) *memoryJournal {
	entries := make([]executionjournal.Entry, 0, len(receiptIDs))
	for index, receiptID := range receiptIDs {
		entries = append(entries, executionjournal.Entry{
			Sequence: uint64(index + 1),
			Attestation: &executionv1.ExecutionAttestationV1{
				Receipt: &executionv1.ExecutionReceiptV1{
					ReceiptId: receiptID, PayloadSha256: "digest-" + receiptID,
				},
			},
		})
	}
	return &memoryJournal{entries: entries, acks: make(map[string]map[string]struct{})}
}

func (journal *memoryJournal) PendingFor(
	_ context.Context,
	exporterID string,
	_ uint64,
	limit int,
) ([]executionjournal.Entry, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	var pending []executionjournal.Entry
	for _, entry := range journal.entries {
		receiptID := entry.Attestation.GetReceipt().GetReceiptId()
		if _, acknowledged := journal.acks[exporterID][receiptID]; acknowledged {
			continue
		}
		pending = append(pending, executionjournal.Entry{
			Sequence:    entry.Sequence,
			Attestation: proto.Clone(entry.Attestation).(*executionv1.ExecutionAttestationV1),
		})
		if len(pending) == limit {
			break
		}
	}
	return pending, nil
}

func (journal *memoryJournal) AcknowledgeFor(
	_ context.Context,
	exporterID string,
	receiptID string,
	payloadSHA256 string,
) (executionjournal.AckResult, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for _, entry := range journal.entries {
		receipt := entry.Attestation.GetReceipt()
		if receipt.GetReceiptId() != receiptID {
			continue
		}
		if receipt.GetPayloadSha256() != payloadSHA256 {
			return executionjournal.AckResult{}, errors.New("digest mismatch")
		}
		if journal.acks[exporterID] == nil {
			journal.acks[exporterID] = make(map[string]struct{})
		}
		_, duplicate := journal.acks[exporterID][receiptID]
		journal.acks[exporterID][receiptID] = struct{}{}
		return executionjournal.AckResult{Duplicate: duplicate}, nil
	}
	return executionjournal.AckResult{}, errors.New("unknown receipt")
}

func (journal *memoryJournal) pendingIDs(exporterID string) []string {
	entries, _ := journal.PendingFor(context.Background(), exporterID, 0, 1000)
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.Attestation.GetReceipt().GetReceiptId())
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
