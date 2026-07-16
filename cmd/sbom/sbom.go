// Package sbom implements the typed Builder.SBOM CLI surface.
package sbom

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var (
	includeDev bool
	output     string
	outputDir  string
)

// ServiceCmd generates one service SBOM through its owning plugin.
var ServiceCmd = &cobra.Command{
	Use:   "service [name]",
	Short: "Generate a CycloneDX SBOM for one service",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()
		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		response, err := generate(ctx, workspace, module, service, includeDev)
		if err != nil {
			return err
		}
		payload, err := coresbom.MarshalCycloneDXJSON(response.GetBom())
		if err != nil {
			return fmt.Errorf("encode CycloneDX: %w", err)
		}
		payload = append(payload, '\n')
		if output == "" || output == "-" {
			_, err = os.Stdout.Write(payload)
			return err
		}
		if err := writeAtomic(output, payload); err != nil {
			return fmt.Errorf("write %s: %w", output, err)
		}
		return nil
	},
}

// WorkspaceCmd generates one independent SBOM per service. Unsupported or
// failed services make the command fail so the output directory can be used as
// honest release evidence.
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Generate CycloneDX SBOMs for every service",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()
		workspace, err := resources.FindWorkspaceUp(ctx)
		if err != nil {
			return fmt.Errorf("find workspace: %w", err)
		}
		if workspace == nil {
			return fmt.Errorf("no workspace found")
		}
		modules, err := workspace.LoadModules(ctx)
		if err != nil {
			return fmt.Errorf("load modules: %w", err)
		}
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
		for _, module := range modules {
			serviceList, err := module.LoadServices(ctx)
			if err != nil {
				return fmt.Errorf("load services for %s: %w", module.Name, err)
			}
			for _, service := range serviceList {
				service.WithModule(module.Name)
				response, err := generate(ctx, workspace, module, service, includeDev)
				if err != nil {
					return fmt.Errorf("%s/%s: %w", module.Name, service.Name, err)
				}
				payload, err := coresbom.MarshalCycloneDXJSON(response.GetBom())
				if err != nil {
					return fmt.Errorf("encode %s/%s CycloneDX: %w", module.Name, service.Name, err)
				}
				name := safeName(module.Name) + "--" + safeName(service.Name) + ".cdx.json"
				if err := writeAtomic(filepath.Join(outputDir, name), append(payload, '\n')); err != nil {
					return err
				}
			}
		}
		return nil
	},
}

func generate(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, withDev bool) (*builderv0.SBOMResponse, error) {
	w := wool.Get(ctx).In("sbom", wool.NameField(service.Name))
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, w.Wrapf(err, "load service")
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, w.Wrapf(err, "load builder agent")
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, w.Wrapf(err, "builder load")
	}
	return instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{IncludeDevDependencies: withDev})
}

func safeName(value string) string {
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "..", "-")
	return value
}

func writeAtomic(destination string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".codefly-sbom-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(path, destination)
}

func init() {
	ServiceCmd.Flags().BoolVar(&includeDev, "include-dev", false, "Include development/test dependencies")
	ServiceCmd.Flags().StringVarP(&output, "output", "o", "-", "Output file, or - for stdout")
	WorkspaceCmd.Flags().BoolVar(&includeDev, "include-dev", false, "Include development/test dependencies")
	WorkspaceCmd.Flags().StringVar(&outputDir, "output-dir", ".codefly/sbom", "Directory for per-service CycloneDX files")
}
