package manager

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	resources "github.com/codefly-dev/core/resources"
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

func NewBuilderLoadManager(unique string) *BuilderLoadManager {
	return &BuilderLoadManager{unique: unique}
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
	hash, err := resources.EndpointHash(ctx, output.Endpoints...)
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

// BuilderInitOutput looks at:
// - endpoints
type BuilderInitOutput struct {
	Endpoints []*basev0.Endpoint
}

type BuilderInitManager struct {
	unique        string
	endpointsHash string

	processed *OutputProperty
}

func NewBuilderInitManager(unique string) *BuilderInitManager {
	return &BuilderInitManager{unique: unique}
}

var _ OutputProcessor = &BuilderInitManager{}

func (b *BuilderInitManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("BuilderInitManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *BuilderInitManager) Set(ctx context.Context, output *BuilderInitOutput) error {
	w := wool.Get(ctx).In("BuilderInitManager.Set")
	// Compute a hash on the endpoints
	hash, err := resources.EndpointHash(ctx, output.Endpoints...)
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

var _ OutputProcessor = &BuilderInitManager{}

// BuilderBuildOutput looks at:
type BuilderBuildOutput struct {
}

type BuilderBuildManager struct {
	unique string

	processed *OutputProperty
}

func NewBuilderBuildManager(unique string) *BuilderBuildManager {
	return &BuilderBuildManager{unique: unique}
}

var _ OutputProcessor = &BuilderBuildManager{}

func (b *BuilderBuildManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("BuilderBuildManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *BuilderBuildManager) Set(ctx context.Context, output *BuilderBuildOutput) error {
	if b.processed == nil {
		b.processed = OnInit()
		return nil
	}

	b.processed = IndependentUpdate()
	return nil
}

var _ OutputProcessor = &BuilderBuildManager{}

// BuilderSyncOutput looks at:
type BuilderSyncOutput struct {
}

type BuilderSyncManager struct {
	unique string

	processed *OutputProperty
}

func NewBuilderSyncManager(unique string) *BuilderSyncManager {
	return &BuilderSyncManager{unique: unique}
}

var _ OutputProcessor = &BuilderSyncManager{}

func (b *BuilderSyncManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("BuilderSyncManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *BuilderSyncManager) Set(ctx context.Context, output *BuilderSyncOutput) error {
	//		w := wool.Get(ctx).In("BuilderSyncManager.Set")
	if b.processed == nil {
		b.processed = OnInit()
		return nil
	}

	return nil
}

var _ OutputProcessor = &BuilderLoadManager{}
