package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/resources"
)

// ProtoNamesRemediation is what to do about protobuf names declared twice.
const ProtoNamesRemediation = "give one of the two contracts its own package and path: run `codefly fix proto-names <module>/<service>` on the service that owns it, then `codefly sync service` it and regenerate its client (`codefly generate contracts`, `codefly generate client`)"

// ProtoNameCollisionError refuses a service whose binary would register the
// same protobuf file or name twice: its own contract and a client library's,
// or two client libraries'. The scaffold gives every service proto/api.proto
// in package api, so this is what happens the first time one service consumes
// another's generated client, and the process dies at start, before any of
// its code runs, with nothing at build time saying why. It is checked from
// the sources so that run, test, lint and CI all say it, with the fix.
func ProtoNameCollisionError(ctx context.Context, workspace *resources.Workspace, service *resources.Service, unique string) error {
	collisions, err := resources.ServiceProtoNameCollisions(ctx, workspace, service)
	if err != nil {
		return fmt.Errorf("cannot check the protobuf names %s loads: %w", unique, err)
	}
	if len(collisions) == 0 {
		return nil
	}
	return fmt.Errorf("%s would load the same protobuf names twice and fail at start: %s. To fix it, %s",
		unique, resources.DescribeProtoNameCollisions(collisions), ProtoNamesRemediation)
}
