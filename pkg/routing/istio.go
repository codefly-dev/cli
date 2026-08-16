package routing

import "strings"

// istioRenderer emits the legacy networking.istio.io VirtualService envelope
// (the saas-starter shape) plus, optionally, the same STRICT-mTLS policy. It
// exists so a workspace already standardized on VirtualService keeps working;
// new solutions should prefer the gateway-api backend.
type istioRenderer struct{}

func (istioRenderer) Name() string { return "istio" }

type virtualService struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   objectMeta         `yaml:"metadata"`
	Spec       virtualServiceSpec `yaml:"spec"`
}

type virtualServiceSpec struct {
	Hosts    []string         `yaml:"hosts"`
	Gateways []string         `yaml:"gateways"`
	HTTP     []httpRouteEntry `yaml:"http"`
}

type httpRouteEntry struct {
	Match []uriMatch         `yaml:"match,omitempty"`
	Route []routeDestination `yaml:"route"`
}

type uriMatch struct {
	URI prefixMatch `yaml:"uri"`
}

type prefixMatch struct {
	Prefix string `yaml:"prefix"`
}

type routeDestination struct {
	Destination destination `yaml:"destination"`
}

type destination struct {
	Host string     `yaml:"host"`
	Port portNumber `yaml:"port"`
}

type portNumber struct {
	Number uint16 `yaml:"number"`
}

func (r istioRenderer) Render(exposure Exposure) (string, error) {
	hosts := exposure.Hosts
	if len(hosts) == 0 {
		hosts = []string{"*"}
	}

	routes := make([]httpRouteEntry, 0, len(exposure.Endpoints))
	for _, endpoint := range exposure.Endpoints {
		routes = append(routes, httpRouteEntry{
			Match: uriMatches(exposure.Prefix),
			Route: []routeDestination{{Destination: destination{
				Host: exposure.backendHost(),
				Port: portNumber{Number: endpoint.Port},
			}}},
		})
	}

	document, err := marshalDocument(virtualService{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       "VirtualService",
		Metadata: objectMeta{
			Name:      exposure.Service,
			Namespace: exposure.Namespace,
			Labels:    managedLabels(exposure.Service),
		},
		Spec: virtualServiceSpec{
			Hosts:    hosts,
			Gateways: []string{exposure.Gateway.istioReference(exposure.Namespace)},
			HTTP:     routes,
		},
	})
	if err != nil {
		return "", err
	}

	documents := []string{document}
	if exposure.EnableMTLS {
		peer, err := renderPeerAuthentication(exposure)
		if err != nil {
			return "", err
		}
		documents = append(documents, peer)
	}
	return joinDocuments(documents), nil
}

// uriMatches scopes a route to the path prefix contract. gRPC method paths
// (/<package>.<Service>/<Method>) and HTTP paths both fall under the prefix.
func uriMatches(prefix string) []uriMatch {
	if prefix == "" {
		return nil
	}
	return []uriMatch{{URI: prefixMatch{Prefix: "/" + strings.TrimPrefix(prefix, "/")}}}
}
