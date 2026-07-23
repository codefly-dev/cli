// Package packageartifact exposes the typed Builder.Package operation.
package packageartifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	outputDirectory string
	artifactName    string
	targets         []string
	includeSBOM     bool
	format          string
	publisher       string
	subjectName     string
	subjectVersion  string
)

// ServiceCmd packages one source resource through its Builder plugin.
var ServiceCmd = &cobra.Command{
	Use:   "service [module/]service",
	Short: "Package one service's source resource with its plugin",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()
		machineReadable := strings.EqualFold(strings.TrimSpace(format), "json")
		if machineReadable {
			cli.SuppressOutput()
			cli.SetOutputSink(func(wool.Loglevel, string) {})
			defer func() {
				cli.SetOutputSink(nil)
				cli.RestoreOutput()
			}()
		}

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		parsedTargets, err := parsePackageTargets(targets)
		if err != nil {
			return err
		}
		absoluteOutput, err := filepath.Abs(outputDirectory)
		if err != nil {
			return fmt.Errorf("resolve package output: %w", err)
		}
		request := &builderv0.PackageRequest{
			Targets:         parsedTargets,
			OutputDirectory: absoluteOutput,
			ArtifactName:    artifactName,
			IncludeSbom:     includeSBOM,
		}
		if publisher != "" || subjectName != "" || subjectVersion != "" {
			request.Subject = &builderv0.PackageSubject{Publisher: publisher, Name: subjectName, Version: subjectVersion}
		}
		response, err := PackageService(ctx, workspace, module, service, request)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(format)) {
		case "json":
			payload, err := (protojson.MarshalOptions{Indent: "  ", UseProtoNames: true, EmitDefaultValues: true}).Marshal(response)
			if err != nil {
				return fmt.Errorf("encode package response: %w", err)
			}
			_, err = os.Stdout.Write(append(payload, '\n'))
			return err
		case "text":
			for _, artifact := range response.GetArtifacts() {
				cli.Info("%s %s/%s sha256:%s", artifact.GetPath(), artifact.GetTarget().GetOs(), artifact.GetTarget().GetArchitecture(), artifact.GetSha256())
			}
			return nil
		default:
			return fmt.Errorf("unsupported package format %q (use text or json)", format)
		}
	},
}

// PackageService invokes the loaded resource's typed Builder.Package RPC.
func PackageService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, request *builderv0.PackageRequest) (*builderv0.PackageResponse, error) {
	w := wool.Get(ctx).In("packageService", wool.NameField(service.Name))
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, w.Wrapf(err, "load service")
	}
	if advertised, supported := orchestration.ValidationOperationSupport(instance.Info, orchestration.ValidationSourcePackage); advertised && !supported {
		return nil, w.NewError("portable source packaging is explicitly unsupported by %s", service.Agent.Identifier())
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, w.Wrapf(err, "load builder agent")
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, w.Wrapf(err, "builder load")
	}
	response, err := instance.Builder.Package(ctx, request)
	if err != nil {
		return nil, w.Wrapf(err, "Builder.Package RPC")
	}
	if response == nil || response.GetState() == nil {
		return nil, w.NewError("Builder.Package returned no status")
	}
	if response.GetState().GetState() != builderv0.PackageStatus_SUCCESS {
		return nil, w.NewError("portable source packaging failed: %s", response.GetState().GetMessage())
	}
	return response, nil
}

func parsePackageTargets(values []string) ([]*builderv0.PackageTarget, error) {
	identities := make(map[string]*builderv0.PackageTarget, len(values))
	for _, value := range values {
		parts := strings.Split(strings.TrimSpace(value), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid package target %q (use os/architecture)", value)
		}
		identity := parts[0] + "/" + parts[1]
		identities[identity] = &builderv0.PackageTarget{Os: parts[0], Architecture: parts[1]}
	}
	keys := make([]string, 0, len(identities))
	for identity := range identities {
		keys = append(keys, identity)
	}
	sort.Strings(keys)
	result := make([]*builderv0.PackageTarget, 0, len(keys))
	for _, identity := range keys {
		result = append(result, identities[identity])
	}
	return result, nil
}

func init() {
	ServiceCmd.Flags().StringVar(&outputDirectory, "output-dir", ".codefly/packages", "Directory for portable package artifacts")
	ServiceCmd.Flags().StringVar(&artifactName, "name", "", "Portable artifact base name (defaults to resource name)")
	ServiceCmd.Flags().StringSliceVar(&targets, "target", nil, "Target os/architecture (repeatable; empty uses the host)")
	ServiceCmd.Flags().BoolVar(&includeSBOM, "sbom", true, "Emit release-bound CycloneDX evidence")
	ServiceCmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	ServiceCmd.Flags().StringVar(&publisher, "publisher", "", "Release subject publisher")
	ServiceCmd.Flags().StringVar(&subjectName, "subject-name", "", "Release subject name")
	ServiceCmd.Flags().StringVar(&subjectVersion, "subject-version", "", "Release subject version")
}
