package routing

// istioRenderer emits the legacy networking.istio.io VirtualService envelope
// (the saas-starter shape) plus, optionally, the same STRICT-mTLS policy. It
// exists so a workspace already standardized on VirtualService keeps working;
// new solutions should prefer the gateway-api backend.
//
// Each endpoint becomes its own VirtualService: Istio evaluates the http rules
// within a VirtualService top to bottom and stops at the first match, so
// collapsing several endpoints into one object would leave every rule after
// the first unreachable. Separate objects (with their own hosts and, for gRPC,
// a package uri prefix) keep every endpoint routable.
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

func (r istioRenderer) Render(exposure *Exposure) (string, error) {
	var documents []string
	for _, endpoint := range exposure.Endpoints {
		hosts := endpoint.Hosts
		if len(hosts) == 0 {
			// validate() only permits hostless endpoints when they are a
			// package-scoped gRPC route; a VirtualService still requires a
			// hosts entry, so bind the package match to every host.
			hosts = []string{"*"}
		}
		document, err := marshalDocument(virtualService{
			APIVersion: "networking.istio.io/v1beta1",
			Kind:       "VirtualService",
			Metadata: objectMeta{
				Name:      exposure.Service + "-" + endpoint.Name,
				Namespace: exposure.Namespace,
				Labels:    managedLabels(exposure.Service),
			},
			Spec: virtualServiceSpec{
				Hosts:    hosts,
				Gateways: []string{exposure.Gateway.istioReference(exposure.Namespace)},
				HTTP: []httpRouteEntry{{
					Match: uriMatches(endpoint.Prefix),
					Route: []routeDestination{{Destination: destination{
						Host: exposure.backendHost(),
						Port: portNumber{Number: endpoint.Port},
					}}},
				}},
			},
		})
		if err != nil {
			return "", err
		}
		documents = append(documents, document)
	}

	if exposure.EnableMTLS {
		peer, err := renderPeerAuthentication(exposure)
		if err != nil {
			return "", err
		}
		documents = append(documents, peer)
	}
	return joinDocuments(documents), nil
}

// uriMatches scopes a gRPC route to its proto package
// (/<package>.<Service>/<Method>). HTTP endpoints carry no proto package, so
// they are host-scoped and match every path.
func uriMatches(prefix string) []uriMatch {
	if prefix == "" {
		return nil
	}
	return []uriMatch{{URI: prefixMatch{Prefix: "/" + prefix}}}
}
