package engine

import (
	"context"

	workcontext "github.com/codefly-dev/sdk-go/workcontext"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// governed reports whether the request presents a Work Context capability.
// Presenting one is the caller stating that this execution is governed, which
// is the only thing that turns an ordinary request into a governed one.
func governed(ctx context.Context) bool {
	values, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	return len(values.Get(workcontext.HeaderName)) > 0
}

// refuseGoverned is the answer to a governed request that would start an
// agent: this process holds none of the authorization-revision, replay, grant
// and seal sources a verification needs, so it admits no unverified
// capability.
func refuseGoverned() error {
	return status.Error(
		codes.Unimplemented,
		"governed execution requires a verified Work Context: this process does not hold the authorization-revision, replay, grant and seal sources needed to verify one, and will not admit an unverified capability",
	)
}
