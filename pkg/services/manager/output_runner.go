package manager

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/wool"
)

// RunnerLoadOutput looks at:
// - endpoints

type RunnerLoadOutput struct {
	Endpoints []*basev0.Endpoint
}

type RunnerLoadManager struct {
	unique        string
	endpointsHash string

	processed *OutputProperty
}

var _ OutputProcessor = &RunnerLoadManager{}

func NewRunnerLoadManager(unique string) *RunnerLoadManager {
	return &RunnerLoadManager{unique: unique}
}

func (b *RunnerLoadManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("RunnerLoadManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *RunnerLoadManager) Set(ctx context.Context, output *RunnerLoadOutput) error {
	w := wool.Get(ctx).In("RunnerLoadManager.Set", wool.NameField(b.unique))
	// Compute a hash on the endpoints
	hash, err := configurations.EndpointHash(ctx, output.Endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot compute endpoints hash")
	}
	defer func() {
		b.endpointsHash = hash
	}()
	defer func() {
		b.endpointsHash = hash
	}()
	if b.processed == nil {
		w.Debug("first time")
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

// RunnerInitOutput looks at:
// - network mappings
// - Provider infos
type RunnerInitOutput struct {
	networkMappings []*basev0.NetworkMapping
	providerInfos   []*basev0.ProviderInformation
}

type RunnerInitManager struct {
	unique             string
	networkMappingHash string
	providerInfoHash   string

	processed *OutputProperty
}

var _ OutputProcessor = &RunnerInitManager{}

func NewRunnerInitManager(unique string) *RunnerInitManager {
	return &RunnerInitManager{unique: unique}
}

func (b *RunnerInitManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("RunnerInitManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *RunnerInitManager) Set(ctx context.Context, output *RunnerInitOutput) error {
	w := wool.Get(ctx).In("RunnerInitManager.Set", wool.NameField(b.unique))

	providerInfoHash, err := configurations.ProviderInformationHash(output.providerInfos...)
	if err != nil {
		return w.Wrapf(err, "cannot compute Provider info hash")
	}

	networkMappingHash, err := configurations.NetworkMappingHash(output.networkMappings...)
	if err != nil {
		return w.Wrapf(err, "cannot compute network mapping hash")
	}
	defer func() {
		b.providerInfoHash = providerInfoHash
		b.networkMappingHash = networkMappingHash
	}()

	if b.processed == nil {
		w.Debug("first time")
		b.processed = OnInit()
		return nil
	}

	// If the hash is different, we need to propagate
	if providerInfoHash != b.providerInfoHash || networkMappingHash != b.networkMappingHash {
		b.processed = RequirePropagation()
		return nil
	}
	b.processed = IndependentUpdate()
	return nil
}

// RunnerStartOutput looks at:
type RunnerStartOutput struct {
}

type RunnerStartManager struct {
	unique string

	processed *OutputProperty
}

var _ OutputProcessor = &RunnerStartManager{}

func NewRunnerStartManager(unique string) *RunnerStartManager {
	return &RunnerStartManager{unique: unique}
}

func (b *RunnerStartManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("RunnerStartManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *RunnerStartManager) Set(ctx context.Context, output *RunnerStartOutput) error {
	w := wool.Get(ctx).In("RunnerStartManager.Set", wool.NameField(b.unique))
	if b.processed == nil {
		w.Debug("first time")
		b.processed = OnInit()
		return nil
	}
	b.processed = IndependentUpdate()
	return nil
}
