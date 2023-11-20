package create

import (
	"github.com/codefly-dev/cli/pkg/plugins/services"
	"github.com/codefly-dev/core/configurations"
)

type ClientBuilder struct{}

func (c ClientBuilder) Get(dependency *configurations.Service, endpoint *configurations.Endpoint) services.ClientDecider {
	return &withClientPrompt{dependency: dependency, endpoint: endpoint}
}

type withClientPrompt struct {
	endpoint   *configurations.Endpoint
	dependency *configurations.Service
	client     *services.Client
}

func (w *withClientPrompt) Includes() (*services.Client, error) {
	return w.client, nil
}

func (w *withClientPrompt) Fetch() error {
	//logger := shared.NewLogger("withClientPrompt.Fetch")
	//var include bool
	//confirm := survey.Confirm{Message: fmt.Sprintf("Include client for API %s of %s?",
	//	w.endpoint.Name, w.dependency.Name),
	//	Default: defaultInclude(w.endpoint.Api)}
	//err := survey.AskOne(&confirm, &include)
	//if err != nil {
	//	return logger.Wrapf(err, "cannot ask for client")
	//}
	//if include {
	//	w.client = &services.Client{Endpoint: &configurations.EndpointEntry{
	//		Name:        w.endpoint.Name,
	//		Description: w.endpoint.Description,
	//		Public:      w.endpoint.Public,
	//		Api:         w.endpoint.Api,
	//	}}
	//}
	return nil
}

var _ services.WithClientDecider = (*ClientBuilder)(nil)

func NewClientBuilder() *ClientBuilder {
	return &ClientBuilder{}
}
