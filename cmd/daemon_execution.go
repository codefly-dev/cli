package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/executionruntime"
	"github.com/spf13/cobra"
)

type gatewayExecutionOptions struct {
	enabled         bool
	authorityIssuer string
	stateDir        string
	exporters       []string
}

func (options *gatewayExecutionOptions) childArgs() ([]string, error) {
	if !options.enabled {
		if options.hasConfiguration() {
			return nil, fmt.Errorf("execution authority, state, and exporter flags require --governed-execution")
		}
		return nil, nil
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	// The child's flags (--governed-execution, the authority JWKS and issuer,
	// the state directory, the exporters) are built here once governed
	// execution can be configured; until then the refusal is the answer, and
	// nothing after it pretends otherwise.
	return nil, governedExecutionUnavailable()
}

func (options *gatewayExecutionOptions) open(
	_ context.Context,
	_ string,
) (*executionruntime.Runtime, error) {
	if !options.enabled {
		if options.hasConfiguration() {
			return nil, fmt.Errorf("execution authority, state, and exporter flags require --governed-execution")
		}
		return nil, nil
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	// The runtime is opened here (executionruntime.Open with the work
	// directory, the state directory, the authority JWKS and issuer, this
	// release and the exporter specs) once the issuer's live sources have a
	// client; until then the refusal is the answer.
	return nil, governedExecutionUnavailable()
}

func (options *gatewayExecutionOptions) validate() error {
	if strings.TrimSpace(options.authorityJWKS) == "" {
		return fmt.Errorf("--execution-authority-jwks is required with --governed-execution")
	}
	if strings.TrimSpace(options.authorityIssuer) == "" {
		return fmt.Errorf("--execution-authority-issuer is required with --governed-execution")
	}
	for _, exporter := range options.exporters {
		if strings.TrimSpace(exporter) == "" {
			return fmt.Errorf("--execution-exporter cannot be empty")
		}
	}
	return nil
}

// governedExecutionUnavailable is why --governed-execution is refused by
// name in this release. Core's Work Context authenticator verifies every
// capability against the issuer's LIVE state — its authorization revision
// and its seals (installation, principal epoch, approved build, operation
// binding) — and has no mode without those sources. This release carries no
// client for them, so governed execution cannot be configured from the
// command line until one exists: it refuses rather than start a gateway
// that would verify nothing.
func governedExecutionUnavailable() error {
	return fmt.Errorf("--governed-execution needs the Work Context issuer's live authorization-revision and seal sources, and this release has no client for them; governed execution is unavailable until one exists")
}

func (options *gatewayExecutionOptions) hasConfiguration() bool {
	return strings.TrimSpace(options.authorityJWKS) != "" ||
		strings.TrimSpace(options.authorityIssuer) != "" ||
		strings.TrimSpace(options.stateDir) != "" ||
		len(options.exporters) != 0
}

func addGatewayExecutionFlags(command *cobra.Command) {
	command.Flags().BoolVar(
		&gatewayExecution.enabled,
		"governed-execution",
		false,
		"Arm durable execution receipt state and its recovery. A governed request is still REFUSED: this process holds none of the sources needed to verify a Work Context",
	)
	command.Flags().StringVar(
		&gatewayExecution.authorityIssuer,
		"execution-authority-issuer",
		"",
		"Exact Work Context issuer",
	)
	command.Flags().StringVar(
		&gatewayExecution.stateDir,
		"execution-state-dir",
		"",
		"Owner-only execution key and receipt state directory (defaults per workspace)",
	)
	command.Flags().StringArrayVar(
		&gatewayExecution.exporters,
		"execution-exporter",
		nil,
		"Installed execution-exporter agent specification (repeatable)",
	)
}
