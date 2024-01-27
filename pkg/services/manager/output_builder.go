package manager

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

// BuilderLoadOutput looks at:
// - endpoints
type BuilderLoadOutput struct {
	Endpoints []*basev0.Endpoint
}

type BuilderLoadManager struct {
	unique        string
	endpointsHash string

	processed *OutputProperty
}

var _ OutputProcessor = &BuilderLoadManager{}

func (b *BuilderLoadManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("BuilderLoadManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *BuilderLoadManager) Set(ctx context.Context, output *BuilderLoadOutput) error {
	w := wool.Get(ctx).In("BuilderLoadManager.Set")
	// Compute a hash on the endpoints
	hash, err := configurations.EndpointHash(ctx, output.Endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot compute endpoints hash")
	}
	defer func() {
		b.endpointsHash = hash
	}()
	if b.processed == nil {
		b.processed = OnInit()
		return nil
	}

	// If the hash is different, we need to propagate
	if hash != b.endpointsHash {
		b.processed = RequirePropagation()
		return nil
	}
	b.processed = IndependentUpdate()
	return nil
}

var _ OutputProcessor = &BuilderLoadManager{}
