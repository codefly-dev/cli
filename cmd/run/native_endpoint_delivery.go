package run

import (
	"context"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

func (cluster *nativeCluster) endpointDeliveryDirectory() string {
	return filepath.Join(cluster.stateDir, "host", "endpoint-delivery")
}

// projectEndpoints updates the existing host's input after signed delivery has
// settled the generation. Readers observe either complete version; a registry
// refresh racing replacement refuses a hash mismatch instead of mixing them.
// Called under the cluster delivery lock.
func (cluster *nativeCluster) projectEndpoints(ctx context.Context, document *solutionhost.SolutionHostBinding) error {
	name, err := solutionhost.EndpointDeliveryFilename(document)
	if err != nil {
		return err
	}
	path := filepath.Join(cluster.endpointDeliveryDirectory(), name)
	if document.Removed {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := cluster.flow.EndpointDelivery(ctx, document, resources.NewNativeNetworkAccess())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".endpoint-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
