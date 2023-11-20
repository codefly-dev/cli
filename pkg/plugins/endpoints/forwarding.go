package endpoints

import (
	"fmt"

	"github.com/codefly-dev/core/configurations"
)

/*
Forwarding is the such an important concept that it deserves to be part of the plugin toolkit
*/

type Forwarding interface {
	Forward(r *configurations.RestRoute) (*configurations.RestRoute, error)
}

type ServiceForwarding struct {
	base *configurations.ServiceReference
	from *configurations.Service
}

var _ Forwarding = (*ServiceForwarding)(nil)

func (s ServiceForwarding) Forward(r *configurations.RestRoute) (*configurations.RestRoute, error) {
	return &configurations.RestRoute{
		Path:        fmt.Sprintf("/%s/%s%s", s.from.Application, s.from.Name, r.Path),
		Methods:     r.Methods,
		Application: s.base.Application,
		Service:     s.base.Name,
	}, nil
}
