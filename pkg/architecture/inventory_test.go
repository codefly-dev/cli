package architecture_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/stretchr/testify/assert"
)

func TestInventory(t *testing.T) {
	ctx := context.Background()
	workspace := &configurations.Workspace{}
	project, err := workspace.LoadProjectFromDir(ctx, "testdata/codefly-platform")
	assert.NoError(t, err)
	assert.NotNil(t, project)

	// applications:
	// management:
	// - organization [application endpoint]
	// web:
	// - frontend -> gateway [public http]
	// - gateway -> organization [public rest]
	// billing
	// - accounts [public rest]
	//

	assert.Equal(t, 3, len(project.Applications))
	gs, err := architecture.LoadPublicApplicationGraph(ctx, project)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(gs))

}
