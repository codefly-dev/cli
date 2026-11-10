package cmd

import (
	"context"
	"errors"

	"github.com/codefly-dev/cli/pkg/composition"
)

// No test in this package reads a real remote: the fixtures name sources that
// do not exist, and a doctor run over them must not depend on the network.
func init() {
	readSourceHead = func(context.Context, string) (*composition.SourceHead, error) {
		return nil, errors.New("offline in tests")
	}
}
