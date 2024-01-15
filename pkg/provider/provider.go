package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

type Provider struct {
	project      *configurations.Project
	projectInfos map[string][]*basev0.ProviderInformation
	serviceInfos map[string][]*basev0.ProviderInformation
	sharedInfos  map[string][]*basev0.ProviderInformation
}

func New(ctx context.Context, project *configurations.Project) (*Provider, error) {
	provider := &Provider{
		project:      project,
		projectInfos: make(map[string][]*basev0.ProviderInformation),
		serviceInfos: make(map[string][]*basev0.ProviderInformation),
		sharedInfos:  make(map[string][]*basev0.ProviderInformation),
	}
	infos, err := configurations.LoadProviderFromEnvFiles(ctx, provider.project, &configurations.Environment{Name: "local"})
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.Origin == configurations.ProjectProviderOrigin {
			provider.projectInfos[info.Name] = append(provider.projectInfos[info.Name], info)
			continue
		}
		provider.serviceInfos[info.Origin] = append(provider.serviceInfos[info.Origin], info)
	}
	return provider, nil
}

type InfoSource struct {
	*configurations.ServiceWithApplication
	Name string
}

// FromService satisfies this format:
// - Name
// - unique:Name
func FromService(service *configurations.Service, dep string) (*InfoSource, error) {
	if !strings.Contains(dep, ":") {
		return &InfoSource{Name: dep}, nil
	}
	tokens := strings.Split(dep, ":")
	if len(tokens) != 2 {
		return nil, fmt.Errorf("invalid provider dependency format: %s", dep)
	}
	name := tokens[1]
	parsed, err := configurations.ParseService(tokens[0])
	if err != nil {
		return nil, err
	}
	if parsed.Application == "" {
		parsed.Application = service.Application
	}
	return &InfoSource{ServiceWithApplication: parsed, Name: name}, nil
}

func (provider *Provider) GetProviderInformation(ctx context.Context, service *configurations.Service) ([]*basev0.ProviderInformation, error) {
	w := wool.Get(ctx).In("provider.GetProviderInformation")
	var res []*basev0.ProviderInformation
	for _, dep := range service.ProviderDependencies {
		source, err := FromService(service, dep)
		if err != nil {
			return nil, w.Wrap(err)
		}
		if source.ServiceWithApplication == nil {
			if infos, ok := provider.projectInfos[dep]; ok {
				res = append(res, infos...)
			}
			continue
		}
		unique := source.ServiceWithApplication.Unique()
		if infos, ok := provider.serviceInfos[unique]; ok {
			for _, info := range infos {
				if info.Name == source.Name {
					res = append(res, info)
				}

			}
		}
	}
	if infos, ok := provider.serviceInfos[service.Unique()]; ok {
		res = append(res, infos...)
	}
	return res, nil
}

func (provider *Provider) GetSharedProviderInformation(_ context.Context, service *configurations.Service) ([]*basev0.ProviderInformation, error) {
	return provider.sharedInfos[service.Unique()], nil
}

func (provider *Provider) Share(ctx context.Context, infos []*basev0.ProviderInformation) {
	w := wool.Get(ctx).In("provider.Share")
	for _, info := range infos {
		w.Debug("adding", wool.Field("info", info))
		provider.sharedInfos[info.Origin] = append(provider.sharedInfos[info.Origin], info)
	}
}
