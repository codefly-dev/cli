package generators_test

import (
	"os"
	"path"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/languages"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/assert"
)

func TestGenerateGRPC(t *testing.T) {
	// Load some endpoints
	ctx, done := common.NewContext()
	defer done()
	dir, err := shared.SolvePath("testdata/api.proto")
	assert.NoError(t, err)
	grpc, err := configurations.NewGrpcAPI(ctx, &configurations.Endpoint{Application: "app", Service: "svc", Name: "api"}, dir)
	assert.NoError(t, err)
	destination := t.TempDir()
	defer os.RemoveAll(destination)
	err = generators.GenerateGRPC(ctx, languages.GO, destination, "app/svc", grpc)
	assert.NoError(t, err)
	// Make sure we have the files
	files := []string{"app_svc_api.pb.go", "app_svc_api_grpc.pb.go"}
	for _, f := range files {
		p := path.Join(destination, f)
		assert.NoError(t, err)
		assert.FileExists(t, p)
	}
}
