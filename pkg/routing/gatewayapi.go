package routing

import (
	"regexp"
	"strings"
)

// gatewayAPIRenderer emits Kubernetes Gateway API routes (the current standard,
// implemented by Istio when gatewayClassName is istio) plus, optionally, an
// Istio STRICT-mTLS policy for the mesh leg of the seam.
type gatewayAPIRenderer struct{}

func (gatewayAPIRenderer) Name() string { return "gateway-api" }

const gatewayAPIVersion = "gateway.networking.k8s.io/v1"

type parentRef struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

type backendRef struct {
	Name string `yaml:"name"`
	Port uint16 `yaml:"port"`
}

type grpcRoute struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   objectMeta    `yaml:"metadata"`
	Spec       grpcRouteSpec `yaml:"spec"`
}

type grpcRouteSpec struct {
	ParentRefs []parentRef     `yaml:"parentRefs"`
	Hostnames  []string        `yaml:"hostnames,omitempty"`
	Rules      []grpcRouteRule `yaml:"rules"`
}

type grpcRouteRule struct {
	Matches     []grpcRouteMatch `yaml:"matches,omitempty"`
	BackendRefs []backendRef     `yaml:"backendRefs"`
}

type grpcRouteMatch struct {
	Method grpcMethodMatch `yaml:"method"`
}

type grpcMethodMatch struct {
	Type    string `yaml:"type"`
	Service string `yaml:"service"`
}

type httpRoute struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   objectMeta    `yaml:"metadata"`
	Spec       httpRouteSpec `yaml:"spec"`
}

type httpRouteSpec struct {
	ParentRefs []parentRef     `yaml:"parentRefs"`
	Hostnames  []string        `yaml:"hostnames,omitempty"`
	Rules      []httpRouteRule `yaml:"rules"`
}

type httpRouteRule struct {
	Matches     []httpRouteMatch `yaml:"matches,omitempty"`
	BackendRefs []backendRef     `yaml:"backendRefs"`
}

type httpRouteMatch struct {
	Path httpPathMatch `yaml:"path"`
}

type httpPathMatch struct {
	Type  string `yaml:"type"`
	Value string `yaml:"value"`
}

func (r gatewayAPIRenderer) Render(exposure Exposure) (string, error) {
	parent := parentRef{Name: exposure.Gateway.Name}
	if exposure.Gateway.Namespace != "" && exposure.Gateway.Namespace != exposure.Namespace {
		parent.Namespace = exposure.Gateway.Namespace
	}

	var documents []string
	for _, endpoint := range exposure.Endpoints {
		backend := backendRef{Name: exposure.Service, Port: endpoint.Port}
		metadata := objectMeta{
			Name:      exposure.Service + "-" + endpoint.Name,
			Namespace: exposure.Namespace,
			Labels:    managedLabels(exposure.Service),
		}
		var (
			document string
			err      error
		)
		if endpoint.GRPC() {
			document, err = marshalDocument(grpcRoute{
				APIVersion: gatewayAPIVersion,
				Kind:       "GRPCRoute",
				Metadata:   metadata,
				Spec: grpcRouteSpec{
					ParentRefs: []parentRef{parent},
					Hostnames:  exposure.Hosts,
					Rules:      []grpcRouteRule{{Matches: grpcMatches(exposure.Prefix), BackendRefs: []backendRef{backend}}},
				},
			})
		} else {
			document, err = marshalDocument(httpRoute{
				APIVersion: gatewayAPIVersion,
				Kind:       "HTTPRoute",
				Metadata:   metadata,
				Spec: httpRouteSpec{
					ParentRefs: []parentRef{parent},
					Hostnames:  exposure.Hosts,
					Rules:      []httpRouteRule{{Matches: httpMatches(exposure.Prefix), BackendRefs: []backendRef{backend}}},
				},
			})
		}
		if err != nil {
			return "", err
		}
		documents = append(documents, document)
	}

	if exposure.EnableMTLS {
		document, err := renderPeerAuthentication(exposure)
		if err != nil {
			return "", err
		}
		documents = append(documents, document)
	}

	return joinDocuments(documents), nil
}

// grpcMatches scopes a GRPCRoute to a proto package: the gRPC service portion
// of the path (/<package>.<Service>/<Method>) must fall under the package. An
// empty prefix means no scoping.
func grpcMatches(prefix string) []grpcRouteMatch {
	if prefix == "" {
		return nil
	}
	return []grpcRouteMatch{{Method: grpcMethodMatch{
		Type:    "RegularExpression",
		Service: regexp.QuoteMeta(prefix) + `\..*`,
	}}}
}

func httpMatches(prefix string) []httpRouteMatch {
	if prefix == "" {
		return nil
	}
	return []httpRouteMatch{{Path: httpPathMatch{
		Type:  "PathPrefix",
		Value: "/" + strings.TrimPrefix(prefix, "/"),
	}}}
}
