package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/executionruntime"
	"github.com/spf13/cobra"
)

type gatewayExecutionOptions struct {
	enabled         bool
	authorityJWKS   string
	authorityIssuer string
	stateDir        string
	exporters       []string
}

func (options gatewayExecutionOptions) childArgs() ([]string, error) {
	if !options.enabled {
		if options.hasConfiguration() {
			return nil, fmt.Errorf("execution authority, state, and exporter flags require --governed-execution")
		}
		return nil, nil
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	args := []string{
		"--governed-execution",
		"--execution-authority-jwks", options.authorityJWKS,
		"--execution-authority-issuer", options.authorityIssuer,
	}
	if strings.TrimSpace(options.stateDir) != "" {
		args = append(args, "--execution-state-dir", options.stateDir)
	}
	for _, exporter := range options.exporters {
		args = append(args, "--execution-exporter", exporter)
	}
	return args, nil
}

func (options gatewayExecutionOptions) open(
	ctx context.Context,
	workDir string,
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
	release, err := cli.GetCurrentVersion()
	if err != nil {
		return nil, fmt.Errorf("resolve Codefly CLI release for execution receipts: %w", err)
	}
	runtime, err := executionruntime.Open(ctx, executionruntime.Config{
		WorkDir:         workDir,
		StateDir:        options.stateDir,
		AuthorityJWKS:   options.authorityJWKS,
		AuthorityIssuer: options.authorityIssuer,
		Release:         release,
		ExporterSpecs:   append([]string(nil), options.exporters...),
		OnDispatchError: func(dispatchErr error) {
			fmt.Fprintf(os.Stderr, "[gateway] execution export pending: %v\n", dispatchErr)
		},
	})
	if err != nil {
		return nil, err
	}
	identity := runtime.Identity
	fmt.Printf(
		"[gateway] governed execution signer=%s key=%s algorithm=%s public_key=%s\n",
		identity.SignerID,
		identity.KeyID,
		identity.Algorithm,
		base64.RawURLEncoding.EncodeToString(identity.PublicKey),
	)
	return runtime, nil
}

func (options gatewayExecutionOptions) validate() error {
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

func (options gatewayExecutionOptions) hasConfiguration() bool {
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
		"Enable signed Work Context admission and durable receipts for supported ApplyEdit/Test effects",
	)
	command.Flags().StringVar(
		&gatewayExecution.authorityJWKS,
		"execution-authority-jwks",
		"",
		"HTTPS JWKS URL for Work Context verification",
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
