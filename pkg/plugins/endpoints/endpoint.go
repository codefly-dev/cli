package endpoints

import (
	"fmt"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/configurations"
)

type NilApiError struct {
	name string
}

func (err *NilApiError) Error() string {
	return fmt.Sprintf("endpoint <%s> api is nil", err.name)
}

type UnknownApiError struct {
	api *corev1.API
}

func (err *UnknownApiError) Error() string {
	return fmt.Sprintf("unknow api: <%v>", err.api)
}

func WhichApiFromEndpoint(endpoint *corev1.Endpoint) (string, error) {
	if endpoint.Api == nil {
		return "", &NilApiError{name: endpoint.Name}
	}
	return WhichApi(endpoint.Api)
}

func WhichApi(api *corev1.API) (string, error) {
	switch api.Value.(type) {
	case *corev1.API_Grpc:
		return configurations.Grpc, nil
	case *corev1.API_Rest:
		return configurations.Rest, nil
	case *corev1.API_Tcp:
		return configurations.Tcp, nil
	default:
		return "", &UnknownApiError{api}
	}
}

func StandardPort(api *corev1.API) (int, error) {
	switch api.Value.(type) {
	case *corev1.API_Grpc:
		return 9090, nil
	case *corev1.API_Rest:
		return 8080, nil
	case *corev1.API_Tcp:
		return 7070, nil
	default:
		return 0, &UnknownApiError{api}
	}
}

type NilEndpointError struct {
}

func (n NilEndpointError) Error() string {
	return "endpoint is nil"
}

func FromProtoEndpoint(r *corev1.Endpoint) (*configurations.Endpoint, error) {
	if r == nil {
		return nil, &NilEndpointError{}
	}
	return &configurations.Endpoint{
		Name:        r.Name,
		Description: r.Description,
		Api:         FromProtoApi(r.Api),
	}, nil
}

func FromProtoApi(api *corev1.API) string {
	switch api.Value.(type) {
	case *corev1.API_Grpc:
		return configurations.Grpc
	case *corev1.API_Rest:
		return configurations.Rest
	case *corev1.API_Tcp:
		return configurations.Tcp
	default:
		return configurations.Unknown
	}
}

func LightApi(api *corev1.API) *corev1.API {
	switch api.Value.(type) {
	case *corev1.API_Grpc:
		return &corev1.API{
			Value: &corev1.API_Grpc{},
		}
	case *corev1.API_Rest:
		return &corev1.API{
			Value: &corev1.API_Rest{
				Rest: &corev1.RestAPI{Routes: api.Value.(*corev1.API_Rest).Rest.Routes},
			},
		}
	case *corev1.API_Tcp:
		return &corev1.API{
			Value: &corev1.API_Tcp{},
		}
	default:
		return nil
	}
}

func Light(e *corev1.Endpoint) *corev1.Endpoint {
	return &corev1.Endpoint{
		Name:        e.Name,
		Description: e.Description,
		Api:         LightApi(e.Api),
	}
}
