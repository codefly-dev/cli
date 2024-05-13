package manager

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// RunnerLoadOutput looks at:
// - endpoints

type RunnerLoadOutput struct {
	Endpoints []*basev0.Endpoint
	Err       string
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
	// Handle error
	if output.Err != "" {
		b.processed = Pause()
		return nil
	}
	// Compute a hash on the endpoints
	w.Debug("computing hash")
	hash, err := resources.EndpointHash(ctx, output.Endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot compute endpoints hash")
	}
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
		w.Debug("detected a change")
		b.processed = RequirePropagation()
		return nil
	}
	b.processed = IndependentUpdate()
	return nil
}

// RunnerInitOutput looks at:
// - network mappings
// - ConfigurationManager infos
type RunnerInitOutput struct {
	networkMappings []*basev0.NetworkMapping
	configurations  []*basev0.Configuration
	failing         bool
}

type RunnerInitManager struct {
	unique string

	configurationHash  string
	networkMappingHash string

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
	if output.failing {
		b.processed = Pause()
		return nil
	}
	networkMappingHash := resources.NetworkMappingHash(output.networkMappings...)
	configurationsHash := resources.ConfigurationsHash(output.configurations...)
	defer func() {
		b.configurationHash = configurationsHash
		b.networkMappingHash = networkMappingHash
	}()

	if b.processed == nil {
		w.Debug("first time")
		b.processed = OnInit()
		return nil
	}

	// Check for propagation
	if configurationsHash != b.configurationHash || networkMappingHash != b.networkMappingHash {
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

// RunnerTestOutput looks at:

type RunnerTestOutput struct {
	Err string
}

type RunnerTestManager struct {
	unique string

	processed *OutputProperty
}

var _ OutputProcessor = &RunnerTestManager{}

func NewRunnerTestManager(unique string) *RunnerTestManager {
	return &RunnerTestManager{unique: unique}
}

func (b *RunnerTestManager) Process(ctx context.Context) (*OutputProperty, error) {
	if b.processed == nil {
		return nil, wool.Get(ctx).In("RunnerTestManager.Process").Wrapf(nil, "cannot process")
	}
	return b.processed, nil
}

func (b *RunnerTestManager) Set(ctx context.Context, output *RunnerTestOutput) error {
	w := wool.Get(ctx).In("RunnerTestManager.Set", wool.NameField(b.unique))
	// Handle error
	if output.Err != "" {
		b.processed = Pause()
		return nil
	}
	defer func() {
	}()
	if b.processed == nil {
		w.Debug("first time")
		b.processed = OnInit()
		return nil
	}

	b.processed = IndependentUpdate()
	return nil
}
