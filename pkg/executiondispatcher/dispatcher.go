// Package executiondispatcher delivers durable Codefly execution receipts to
// independently installed exporter plugins. It is product-neutral: Warden is
// one possible exporter, never a branch in the dispatcher.
package executiondispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/executionjournal"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	defaultBatchLimit    = 64
	defaultExportTimeout = 5 * time.Second
	defaultPollInterval  = time.Second
)

var (
	// ErrInvalid is returned for malformed dispatcher configuration or plugin
	// acknowledgements.
	ErrInvalid = errors.New("invalid Codefly execution dispatcher input")
)

// Journal is the durable, exporter-isolated outbox contract.
type Journal interface {
	PendingFor(context.Context, string, uint64, int) ([]executionjournal.Entry, error)
	AcknowledgeFor(context.Context, string, string, string) (executionjournal.AckResult, error)
}

// ExporterClient is the generated plugin client surface used by delivery.
type ExporterClient interface {
	Export(
		context.Context,
		*executionv1.ExportExecutionRequest,
		...grpc.CallOption,
	) (*executionv1.ExportExecutionResponse, error)
}

// Exporter is one independently acknowledged plugin destination.
type Exporter struct {
	ID     string
	Client ExporterClient
}

// Config defines bounded delivery behavior.
type Config struct {
	Journal       Journal
	Exporters     []Exporter
	BatchLimit    int
	ExportTimeout time.Duration
	PollInterval  time.Duration
	OnError       func(error)
}

// ExporterReport describes one bounded delivery pass.
type ExporterReport struct {
	ExporterID string
	Attempted  int
	Accepted   int
}

// Report describes one pass across every configured plugin.
type Report struct {
	Exporters []ExporterReport
}

// Dispatcher drains one durable stream per exporter. Calls are serialized so
// a polling loop and a diagnostic/manual drain cannot reorder delivery.
type Dispatcher struct {
	mu            sync.Mutex
	journal       Journal
	exporters     []Exporter
	batchLimit    int
	exportTimeout time.Duration
	pollInterval  time.Duration
	onError       func(error)
}

// New validates and freezes a dispatcher. An empty exporter list is a
// supported no-op configuration; installing a plugin adds behavior without
// altering execution or receipt production.
func New(config Config) (*Dispatcher, error) {
	if config.Journal == nil {
		return nil, fmt.Errorf("%w: journal is required", ErrInvalid)
	}
	batchLimit := config.BatchLimit
	if batchLimit == 0 {
		batchLimit = defaultBatchLimit
	}
	if batchLimit < 1 || batchLimit > 1000 {
		return nil, fmt.Errorf("%w: batch limit must be between 1 and 1000", ErrInvalid)
	}
	exportTimeout := config.ExportTimeout
	if exportTimeout == 0 {
		exportTimeout = defaultExportTimeout
	}
	if exportTimeout < time.Millisecond || exportTimeout > time.Minute {
		return nil, fmt.Errorf("%w: export timeout must be between 1ms and 1m", ErrInvalid)
	}
	pollInterval := config.PollInterval
	if pollInterval == 0 {
		pollInterval = defaultPollInterval
	}
	if pollInterval < 10*time.Millisecond || pollInterval > time.Minute {
		return nil, fmt.Errorf("%w: poll interval must be between 10ms and 1m", ErrInvalid)
	}

	exporters := make([]Exporter, len(config.Exporters))
	seen := make(map[string]struct{}, len(config.Exporters))
	for index, exporter := range config.Exporters {
		if err := validateExporterID(exporter.ID); err != nil {
			return nil, err
		}
		if exporter.Client == nil {
			return nil, fmt.Errorf("%w: exporter %q has no client", ErrInvalid, exporter.ID)
		}
		if _, duplicate := seen[exporter.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate exporter ID %q", ErrInvalid, exporter.ID)
		}
		seen[exporter.ID] = struct{}{}
		exporters[index] = exporter
	}
	return &Dispatcher{
		journal: config.Journal, exporters: exporters, batchLimit: batchLimit,
		exportTimeout: exportTimeout, pollInterval: pollInterval, onError: config.OnError,
	}, nil
}

// DispatchOnce performs at most one batch per exporter. Failure of one plugin
// never blocks another plugin, and a plugin's first failed receipt stops only
// that plugin's pass to preserve ordering.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (Report, error) {
	if d == nil {
		return Report{}, fmt.Errorf("%w: dispatcher is nil", ErrInvalid)
	}
	if ctx == nil {
		return Report{}, fmt.Errorf("%w: context is required", ErrInvalid)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	report := Report{Exporters: make([]ExporterReport, 0, len(d.exporters))}
	var dispatchErrors []error
	for _, exporter := range d.exporters {
		exporterReport, err := d.dispatchExporter(ctx, exporter)
		report.Exporters = append(report.Exporters, exporterReport)
		if err != nil {
			dispatchErrors = append(dispatchErrors, fmt.Errorf("exporter %q: %w", exporter.ID, err))
		}
	}
	return report, errors.Join(dispatchErrors...)
}

func (d *Dispatcher) dispatchExporter(
	ctx context.Context,
	exporter Exporter,
) (ExporterReport, error) {
	report := ExporterReport{ExporterID: exporter.ID}
	entries, err := d.journal.PendingFor(ctx, exporter.ID, 0, d.batchLimit)
	if err != nil {
		return report, fmt.Errorf("read pending receipts: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		attestation := entry.Attestation
		if attestation == nil || attestation.GetReceipt() == nil {
			return report, fmt.Errorf("%w: journal sequence %d has no attestation", ErrInvalid, entry.Sequence)
		}
		receipt := attestation.GetReceipt()
		if receipt.GetReceiptId() == "" || receipt.GetPayloadSha256() == "" {
			return report, fmt.Errorf("%w: journal sequence %d has incomplete receipt identity", ErrInvalid, entry.Sequence)
		}
		report.Attempted++
		callContext, cancel := context.WithTimeout(ctx, d.exportTimeout)
		response, exportErr := exporter.Client.Export(
			callContext,
			&executionv1.ExportExecutionRequest{
				Attestation: proto.Clone(attestation).(*executionv1.ExecutionAttestationV1),
			},
		)
		cancel()
		if exportErr != nil {
			return report, fmt.Errorf("export receipt %q: %w", receipt.GetReceiptId(), exportErr)
		}
		if response == nil || response.GetReceiptId() != receipt.GetReceiptId() {
			return report, fmt.Errorf(
				"%w: exporter acknowledged receipt %q as %q",
				ErrInvalid,
				receipt.GetReceiptId(),
				response.GetReceiptId(),
			)
		}
		if _, err := d.journal.AcknowledgeFor(
			ctx,
			exporter.ID,
			receipt.GetReceiptId(),
			receipt.GetPayloadSha256(),
		); err != nil {
			return report, fmt.Errorf("persist acknowledgement for receipt %q: %w", receipt.GetReceiptId(), err)
		}
		report.Accepted++
	}
	return report, nil
}

// Run polls until the parent context is cancelled. Export failures are
// durable/retryable and are reported through OnError; they do not stop the
// Gateway or receipt production.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil {
		return fmt.Errorf("%w: dispatcher is nil", ErrInvalid)
	}
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrInvalid)
	}
	for {
		if _, err := d.DispatchOnce(ctx); err != nil && d.onError != nil && ctx.Err() == nil {
			d.onError(err)
		}
		timer := time.NewTimer(d.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func validateExporterID(exporterID string) error {
	if exporterID == "" || strings.TrimSpace(exporterID) != exporterID || len(exporterID) > 512 {
		return fmt.Errorf("%w: exporter ID must be canonical and at most 512 bytes", ErrInvalid)
	}
	for _, character := range exporterID {
		if character == '\x00' || character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: exporter ID contains a control character", ErrInvalid)
		}
	}
	return nil
}
