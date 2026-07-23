// Package executionruntime assembles Codefly's product-neutral governed
// execution producer. Exporter products remain independently installed service
// agents and never become branches in the Gateway.
package executionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/executionattestor"
	"github.com/codefly-dev/cli/pkg/executiondispatcher"
	"github.com/codefly-dev/cli/pkg/executionjournal"
	"github.com/codefly-dev/cli/pkg/executionrecorder"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	codefly "github.com/codefly-dev/sdk-go"
)

const (
	// ProducerID is the exact resource identifier required by the SDK Work
	// Context scope evidence:append:<producer>.
	ProducerID = "codefly.execution"

	producerComponent = "gateway"
	attestorFileName  = "gateway-ed25519-v1.json"
	journalFileName   = "receipts-v1.db"
)

var ErrInvalid = errors.New("invalid Codefly execution runtime configuration")

// Config defines one workspace-scoped execution producer.
type Config struct {
	WorkDir         string
	StateDir        string
	AuthorityJWKS   string
	AuthorityIssuer string
	Release         string
	ExporterSpecs   []string
	OnDispatchError func(error)
}

// Runtime owns the Gateway's recorder, dispatcher, journal, and exporter
// processes. Close it only after the Gateway has stopped its dispatcher loop.
type Runtime struct {
	Recorder   *executionrecorder.Recorder
	Dispatcher *executiondispatcher.Dispatcher
	Identity   executionattestor.PublicIdentity

	journal           *executionjournal.Journal
	exporterCacheKeys []string
}

// DefaultStateDir deterministically isolates durable execution state by
// canonical workspace path without exposing that path in the directory name.
func DefaultStateDir(workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve workspace: %v", ErrInvalid, err)
	}
	canonical := filepath.Clean(absolute)
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(
		resources.CodeflyHomeDir(),
		"execution",
		"gateways",
		hex.EncodeToString(digest[:]),
	), nil
}

// Open creates the workspace producer and loads only explicitly configured
// exporter plugins. JWKS retrieval remains lazy until the first governed
// operation, allowing startup while the authority is temporarily unavailable
// without weakening request-time admission.
func Open(ctx context.Context, config Config) (*Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.AuthorityJWKS) == "" {
		return nil, fmt.Errorf("%w: authority JWKS URL is required", ErrInvalid)
	}
	if strings.TrimSpace(config.AuthorityIssuer) == "" {
		return nil, fmt.Errorf("%w: authority issuer is required", ErrInvalid)
	}
	release := strings.TrimSpace(config.Release)
	if release == "" {
		return nil, fmt.Errorf("%w: producer release is required", ErrInvalid)
	}
	stateDir := strings.TrimSpace(config.StateDir)
	if stateDir == "" {
		var err error
		stateDir, err = DefaultStateDir(config.WorkDir)
		if err != nil {
			return nil, err
		}
	} else {
		absolute, err := filepath.Abs(stateDir)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve state directory: %v", ErrInvalid, err)
		}
		stateDir = filepath.Clean(absolute)
	}

	attestor, err := executionattestor.OpenFile(ctx, filepath.Join(stateDir, attestorFileName))
	if err != nil {
		return nil, fmt.Errorf("open execution attestor: %w", err)
	}
	journal, err := executionjournal.Open(
		ctx,
		filepath.Join(stateDir, journalFileName),
		attestor.Verify,
	)
	if err != nil {
		return nil, fmt.Errorf("open execution journal: %w", err)
	}
	closeJournal := true
	defer func() {
		if closeJournal {
			_ = journal.Close()
		}
	}()

	verifier, err := codefly.NewWorkContextJWKSVerifier(codefly.WorkContextJWKSVerifierOptions{
		URL: config.AuthorityJWKS,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Work Context JWKS verifier: %w", err)
	}
	authority, err := executionrecorder.NewWorkContextAuthority(
		executionrecorder.WorkContextAuthorityConfig{
			Verifier: verifier,
			Issuer:   config.AuthorityIssuer,
			Audience: executionrecorder.ExecutionWorkContextAudience,
		},
	)
	if err != nil {
		return nil, err
	}
	recorder, err := executionrecorder.New(executionrecorder.Config{
		Journal:   journal,
		Attestor:  attestor,
		Authority: authority,
		Producer: &executionv1.ExecutionProducerV1{
			Id:        ProducerID,
			Component: producerComponent,
			Release:   release,
		},
	})
	if err != nil {
		return nil, err
	}

	exporters, cacheKeys, err := loadExporters(ctx, config.ExporterSpecs)
	if err != nil {
		return nil, err
	}
	closeExporters := true
	defer func() {
		if closeExporters {
			clearExporters(cacheKeys)
		}
	}()
	dispatcher, err := executiondispatcher.New(executiondispatcher.Config{
		Journal:   journal,
		Exporters: exporters,
		OnError:   config.OnDispatchError,
	})
	if err != nil {
		return nil, err
	}

	closeJournal = false
	closeExporters = false
	return &Runtime{
		Recorder: recorder, Dispatcher: dispatcher, Identity: attestor.Identity(),
		journal: journal, exporterCacheKeys: cacheKeys,
	}, nil
}

// Close releases exporter processes and the durable journal lock.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	clearExporters(r.exporterCacheKeys)
	r.exporterCacheKeys = nil
	if r.journal == nil {
		return nil
	}
	err := r.journal.Close()
	r.journal = nil
	return err
}

func loadExporters(
	ctx context.Context,
	specs []string,
) ([]executiondispatcher.Exporter, []string, error) {
	type parsedExporter struct {
		id       string
		agent    *resources.Agent
		cacheKey string
	}
	parsed := make([]parsedExporter, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, raw := range specs {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			return nil, nil, fmt.Errorf("%w: exporter agent specification is empty", ErrInvalid)
		}
		agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, spec)
		if err != nil {
			return nil, nil, fmt.Errorf("parse execution exporter %q: %w", spec, err)
		}
		id := agent.Publisher + "/" + agent.Name
		if _, duplicate := seen[id]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate semantic exporter %q", ErrInvalid, id)
		}
		seen[id] = struct{}{}
		parsed = append(parsed, parsedExporter{
			id: id, agent: agent, cacheKey: "execution-exporter::" + agent.Unique(),
		})
	}

	exporters := make([]executiondispatcher.Exporter, 0, len(parsed))
	cacheKeys := make([]string, 0, len(parsed))
	for _, candidate := range parsed {
		agent, err := coreservices.LoadAgent(ctx, candidate.agent, candidate.cacheKey)
		if err != nil {
			clearExporters(cacheKeys)
			return nil, nil, fmt.Errorf("load execution exporter %q: %w", candidate.id, err)
		}
		cacheKeys = append(cacheKeys, candidate.cacheKey)
		info, err := agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
		if err != nil {
			clearExporters(cacheKeys)
			return nil, nil, fmt.Errorf("inspect execution exporter %q: %w", candidate.id, err)
		}
		if !advertisesExecutionExporter(info) {
			clearExporters(cacheKeys)
			return nil, nil, fmt.Errorf(
				"%w: agent %q does not advertise EXECUTION_EXPORTER",
				ErrInvalid,
				candidate.id,
			)
		}
		if agent.ExecutionExporter == nil {
			clearExporters(cacheKeys)
			return nil, nil, fmt.Errorf(
				"%w: agent %q has no execution exporter client",
				ErrInvalid,
				candidate.id,
			)
		}
		exporters = append(exporters, executiondispatcher.Exporter{
			ID: candidate.id, Client: agent.ExecutionExporter,
		})
	}
	return exporters, cacheKeys, nil
}

func advertisesExecutionExporter(info *agentv0.AgentInformation) bool {
	if info == nil {
		return false
	}
	for _, capability := range info.GetCapabilities() {
		if capability.GetType() == agentv0.Capability_EXECUTION_EXPORTER {
			return true
		}
	}
	return false
}

func clearExporters(cacheKeys []string) {
	for _, cacheKey := range cacheKeys {
		coreservices.ClearAgent(cacheKey)
	}
}
