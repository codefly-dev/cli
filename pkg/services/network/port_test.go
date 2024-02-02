package network_test

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/services/network"
	"github.com/codefly-dev/core/configurations/standards"

	"github.com/brianvoe/gofakeit/v6"

	"github.com/stretchr/testify/assert"
)

func TestHashInt(t *testing.T) {
	for i := 0; i < 1000; i++ {
		s := gofakeit.BS()
		v := network.HashInt(s, 10, 99)
		assert.GreaterOrEqual(t, v, 10)
		assert.LessOrEqual(t, v, 99)
	}
}

func getLastDigit(num int) int {
	return num % 10
}

func getApp(num int) int {
	return num / 1000
}

func TestPortGeneration(t *testing.T) {
	// first 3 digits: app
	var appPart *int
	for i := 0; i < 10; i++ {
		app := gofakeit.AppName()
		for j := 0; j < 10; j++ {
			for _, api := range []string{standards.TCP, standards.HTTP, standards.GRPC} {
				svc := gofakeit.Adjective()
				v := network.ToPort(app, svc, api)
				assert.GreaterOrEqual(t, v, 11000)
				assert.LessOrEqual(t, v, 49999)
				if appPart == nil {
					appPart = new(int)
					*appPart = getApp(v)
				} else {
					assert.Equal(t, *appPart, getApp(v))
				}
				assert.Equal(t, getLastDigit(v), network.APIInt(api))
			}
		}
		appPart = nil
	}
}

func TestBugNaming(t *testing.T) {
	one := network.ToPort("test-application", "test", standards.GRPC)
	two := network.ToPort("test-application", "go-test", standards.GRPC)
	assert.NotEqual(t, one, two)
}
