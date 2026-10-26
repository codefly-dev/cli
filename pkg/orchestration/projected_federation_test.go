package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestProjectedFederationResolvesWithoutAnAuthoredFile(t *testing.T) {
	for _, authored := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated only", true: "preserves authored secret"}[authored], func(t *testing.T) {
			loader := staticWorkspaceLoader{}
			if authored {
				secret := workspaceConfiguration("federation", "REGISTRATION_SECRET", "fixture-secret")
				secret.Infos[0].ConfigurationValues[0].Secret = true
				loader.confs = []*basev0.Configuration{secret}
			}
			world := loadedWorkspaceWorld(t, loader)
			calls := 0
			world.RenderOwnedGroups = func(context.Context, *resources.Service) ([]*basev0.Configuration, error) {
				calls++
				return []*basev0.Configuration{workspaceConfiguration("federation", environments.FederationHostCoordinateKey, "owner/local/host")}, nil
			}
			service := &resources.Service{WorkspaceConfigurationDependencies: []string{"federation"}}
			confs, err := world.workspaceConfigurationsFor(t.Context(), service, nil, resources.NewNativeNetworkAccess())
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			values := map[string]*basev0.ConfigurationValue{}
			for _, conf := range confs {
				for _, info := range conf.Infos {
					for _, value := range info.ConfigurationValues {
						values[value.Key] = value
					}
				}
			}
			require.Equal(t, "owner/local/host", values[environments.FederationHostCoordinateKey].Value)
			if authored {
				require.Equal(t, "fixture-secret", values["REGISTRATION_SECRET"].Value)
				require.True(t, values["REGISTRATION_SECRET"].Secret)
			}
		})
	}
}

func TestProjectedFederationDoesNotRelaxRequiredConfigurations(t *testing.T) {
	for _, scenario := range []string{"no hook", "no projection", "other group", "unknown loader", "ambiguous", "projection failure"} {
		t.Run(scenario, func(t *testing.T) {
			world := loadedWorkspaceWorld(t, staticWorkspaceLoader{})
			world.RenderOwnedGroups = func(context.Context, *resources.Service) ([]*basev0.Configuration, error) {
				if scenario == "projection failure" {
					return nil, errors.New("host projection failed")
				}
				if scenario == "no projection" {
					return nil, nil
				}
				return []*basev0.Configuration{workspaceConfiguration("federation", environments.FederationHostCoordinateKey, "owner/local/host")}, nil
			}
			group := "federation"
			if scenario == "no hook" {
				world.RenderOwnedGroups = nil
			}
			if scenario == "other group" {
				group = "required-secret"
			}
			if scenario == "unknown loader" {
				world.providedWorkspaceConfigurationInfos = nil
			}
			if scenario == "ambiguous" {
				world.ambiguousWorkspaceConfigurations = func() map[string]error { return map[string]error{"federation": errors.New("conflicting groups")} }
			}
			_, err := world.workspaceConfigurationsFor(t.Context(), &resources.Service{WorkspaceConfigurationDependencies: []string{group}}, nil, resources.NewNativeNetworkAccess())
			require.Error(t, err)
		})
	}
}

func TestNativeFederationProjectionThroughConfigurationResolver(t *testing.T) {
	host := &environments.EnvironmentHost{Delivery: "authority/local/grpc", Coordinate: "owner/local/host", Domain: "owner"}
	for _, scenario := range []string{"host", "other service", "excluded", "authored invalid reference"} {
		t.Run(scenario, func(t *testing.T) {
			loader := staticWorkspaceLoader{}
			if scenario == "authored invalid reference" {
				loader.confs = []*basev0.Configuration{workspaceConfiguration("federation", "registration", "${endpoint:missing/service/grpc}")}
			}
			world := loadedWorkspaceWorld(t, loader)
			world.RenderOwnedGroups = func(_ context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
				group, err := environments.NativeHostFederation(host, service)
				if group == nil || err != nil {
					return nil, err
				}
				return []*basev0.Configuration{group}, nil
			}
			service := &resources.Service{Name: "local", WorkspaceConfigurationDependencies: []string{"federation"}}
			service.WithModule("authority")
			if scenario == "other service" {
				service.Name = "edge"
			}
			if scenario == "excluded" {
				world.excludedWorkspaceConfigurations = map[string]bool{"federation": true}
			}
			confs, err := world.workspaceConfigurationsFor(t.Context(), service, nil, resources.NewNativeNetworkAccess())
			if scenario == "other service" || scenario == "authored invalid reference" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if scenario == "excluded" {
				require.Empty(t, confs)
			} else {
				require.Len(t, confs, 1)
				require.Len(t, confs[0].Infos[0].ConfigurationValues, 3)
			}
		})
	}
}
