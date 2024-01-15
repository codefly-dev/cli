package provider_test

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/provider"
	"github.com/codefly-dev/core/configurations"
	"github.com/stretchr/testify/assert"
)

func TestFromService(t *testing.T) {
	service := &configurations.Service{
		Name:        "svc",
		Application: "app",
	}
	tcs := []struct {
		in          string
		service     string
		application string
		name        string
	}{
		{in: "auth0", name: "auth0"},
		{in: "other_app/store:postgres", name: "postgres", service: "store", application: "other_app"},
		{in: "store:postgres", name: "postgres", service: "store", application: "app"},
	}

	for _, tc := range tcs {
		t.Run(tc.in, func(t *testing.T) {
			res, err := provider.FromService(service, tc.in)
			assert.NoError(t, err)
			assert.Equal(t, res.Name, tc.name)
			if tc.service != "" {
				assert.Equal(t, res.ServiceWithApplication.Name, tc.service)
			}
			if tc.application != "" {
				assert.Equal(t, res.ServiceWithApplication.Application, tc.application)
			}
		})
	}

}
