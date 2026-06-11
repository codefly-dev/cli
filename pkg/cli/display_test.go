// display_test.go — coverage for the error-chain renderer.
//
// printErrorChain itself writes to stdout via the TUI; we exercise
// the pure helper `unwrapErrorLayers` it delegates to, which is the
// part that has logic worth testing (the rendering loop is one
// strings.Repeat + fmt.Println per line).

package cli

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestUnwrapErrorLayers_Nil(t *testing.T) {
	if got := unwrapErrorLayers(nil); got != nil {
		t.Errorf("nil err: want nil, got %v", got)
	}
}

func TestUnwrapErrorLayers_FlatError(t *testing.T) {
	err := errors.New("boom")
	got := unwrapErrorLayers(err)
	want := []string{"boom"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flat: got %v, want %v", got, want)
	}
}

func TestUnwrapErrorLayers_MultiLayerStripsInnerSuffix(t *testing.T) {
	// Three-layer wrap with fmt.Errorf's "%w" — each layer's
	// rendered message includes its inner via "outer: inner".
	// The helper should strip that so each layer's row carries
	// only the OUTER's own contribution.
	root := errors.New("connection refused")
	mid := fmt.Errorf("postgres ping failed: %w", root)
	top := fmt.Errorf("service neo4j start: %w", mid)

	got := unwrapErrorLayers(top)
	want := []string{
		"service neo4j start",
		"postgres ping failed",
		"connection refused",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapErrorLayers_PreservesLayerWithoutMatchingSuffix(t *testing.T) {
	// If an outer error's message DOESN'T end with ": "+inner
	// (e.g. it formatted the inner some other way), we should
	// keep the message verbatim — the strip is opportunistic.
	root := errors.New("root")
	custom := &customWrap{inner: root, msg: "totally different message"}
	got := unwrapErrorLayers(custom)
	if len(got) != 2 {
		t.Fatalf("want 2 layers, got %d: %v", len(got), got)
	}
	if got[0] != "totally different message" {
		t.Errorf("layer 0 should be preserved verbatim; got %q", got[0])
	}
	if got[1] != "root" {
		t.Errorf("layer 1 should be root; got %q", got[1])
	}
}

type customWrap struct {
	inner error
	msg   string
}

func (c *customWrap) Error() string { return c.msg }
func (c *customWrap) Unwrap() error { return c.inner }
