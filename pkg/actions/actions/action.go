package actions

import (
	"context"
	"encoding/json"
	"fmt"
)

type Action interface {
	Run(ctx context.Context) (any, error)
}

type FactoryFunc func(content []byte) (Action, error)

// Factory registry map
var factoryRegistry = make(map[string]FactoryFunc)

func RegisterFactory(typeName string, factory FactoryFunc) {
	factoryRegistry[typeName] = factory
}

func CreateAction(content []byte) (Action, error) {
	var base Config
	err := json.Unmarshal(content, &base)
	if err != nil {
		return nil, err
	}
	typeName := base.Kind

	if factory, ok := factoryRegistry[typeName]; ok {
		return factory(content)
	}
	return nil, fmt.Errorf("unknown type: %v", typeName)
}

// Wrap function with a constraint that *T satisfies Action
func Wrap[T Action]() FactoryFunc {
	return func(content []byte) (Action, error) {
		ptr, err := Load[T](content) // Note the pointer type here
		if err != nil {
			return nil, err
		}
		return *ptr, nil
	}
}

// Load function, *T must satisfy Action
func Load[T Action](content []byte) (*T, error) {
	var action T
	err := json.Unmarshal(content, &action)
	if err != nil {
		return nil, err
	}
	return &action, nil // Returning a pointer
}
