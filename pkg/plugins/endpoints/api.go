package endpoints

import (
	"context"
	"fmt"
	"os"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	openapiloads "github.com/go-openapi/loads"
	openapispec "github.com/go-openapi/spec"
)

func WithApi(endpoint *configurations.Endpoint, source ApiSource) (*corev1.Endpoint, error) {
	logger := shared.NewLogger("services.DefaultApi")
	logger.TODO("visibility")
	api, err := source.Proto()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create grpc api: %v")
	}
	return &corev1.Endpoint{
		Name:        endpoint.Name,
		Description: endpoint.Description,
		Api:         api,
	}, nil
}

type ApiSource interface {
	Proto() (*corev1.API, error)
}

type GrpcApi struct {
	filename string
	content  []byte
}

func NewGrpcApi(endpoint *configurations.Endpoint, filename string) (*corev1.Endpoint, error) {
	// Read the file content
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %v", filename, err)
	}
	return WithApi(endpoint, &GrpcApi{filename: filename, content: content})
}

func (grpc *GrpcApi) Proto() (*corev1.API, error) {
	// Add a GrpcAPI message with the file content
	grpcAPI := &corev1.GrpcAPI{
		Proto: grpc.content,
	}
	// Add an API message with the GrpcAPI message
	api := &corev1.API{
		Value: &corev1.API_Grpc{
			Grpc: grpcAPI,
		},
	}
	return api, nil
}

type RestApi struct {
	//openapi []byte
}

func NewRestApi(endpoint *configurations.Endpoint) (*corev1.Endpoint, error) {
	return WithApi(endpoint, &RestApi{})
}

func (http *RestApi) Proto() (*corev1.API, error) {
	restAPI := &corev1.RestAPI{}
	// Add an API message with the GrpcAPI message
	api := &corev1.API{
		Value: &corev1.API_Rest{
			Rest: restAPI,
		},
	}
	return api, nil
}

func NewRestApiFromOpenAPI(ctx context.Context, endpoint *configurations.Endpoint, filename string) (*corev1.Endpoint, error) {
	logger := shared.NewLogger("services.Default")
	logger.TODO("visibility")
	rest := &corev1.API_Rest{Rest: &corev1.RestAPI{}}
	e := &corev1.Endpoint{
		Name:        endpoint.Name,
		Description: endpoint.Description,
		Api: &corev1.API{
			Value: rest,
		},
	}
	if !shared.FileExists(filename) {
		return e, nil
	}
	// Read the file content
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, logger.Wrapf(err, "failed to read file")
	}
	swagger, err := parseOpenApi(content)
	if err != nil {
		return nil, logger.Wrapf(err, "failed to parse openapi spec")
	}

	rest.Rest.Openapi = content
	for p, item := range swagger.Paths.Paths {
		rest.Rest.Routes = append(rest.Rest.Routes, &corev1.RestRoute{
			Methods: getHTTPMethodsFromPathItem(item),
			Path:    p,
		})
	}
	return e, nil
}

type Tcp struct{}

func NewTcp() (*Tcp, error) {
	return &Tcp{}, nil
}

func (*Tcp) Proto() (*corev1.API, error) {
	// Add a GrpcAPI message with the file content
	tcp := &corev1.TcpAPI{}
	// Add an API message with the GrpcAPI message
	api := &corev1.API{
		Value: &corev1.API_Tcp{
			Tcp: tcp,
		},
	}
	return api, nil
}

/* Helpers */
func parseOpenApi(spec []byte) (*openapispec.Swagger, error) {
	analyzed, err := openapiloads.Analyzed(spec, "2.0")
	if err != nil {
		return nil, err
	}
	return analyzed.Spec(), nil
}

func getHTTPMethodsFromPathItem(pathItem openapispec.PathItem) []corev1.HttpMethod {
	var methods []corev1.HttpMethod

	if pathItem.Get != nil {
		methods = append(methods, corev1.HttpMethod_GET)
	}
	if pathItem.Post != nil {
		methods = append(methods, corev1.HttpMethod_POST)
	}
	if pathItem.Put != nil {
		methods = append(methods, corev1.HttpMethod_PUT)
	}
	if pathItem.Delete != nil {
		methods = append(methods, corev1.HttpMethod_DELETE)
	}
	if pathItem.Options != nil {
		methods = append(methods, corev1.HttpMethod_OPTIONS)
	}
	if pathItem.Head != nil {
		methods = append(methods, corev1.HttpMethod_HEAD)
	}
	if pathItem.Patch != nil {
		methods = append(methods, corev1.HttpMethod_PATCH)
	}
	return methods
}
