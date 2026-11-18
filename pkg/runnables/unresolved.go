package runnables

import (
	"strings"

	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
)

// unresolvedAddress stands in for an endpoint reference when a prepared
// binding is read before a run resolved it. It names no host.
const unresolvedAddress = "http://unresolved.invalid"

// DecodeUnresolvedPrepared reads one runnable-bindings value as its file holds
// it. A local environment's file carries the call endpoint as an
// ${endpoint:…} reference, which the run that delivers the group resolves; a
// reader of the file itself — the grants a composition asks for — reads the
// binding's operation, contract and policy, and must not read its address.
func DecodeUnresolvedPrepared(value string) (*runnablev0.PreparedBinding, error) {
	for _, reference := range resources.EndpointReferences(value) {
		value = strings.ReplaceAll(value, "${endpoint:"+reference+"}", unresolvedAddress)
	}
	return corerunnable.DecodePrepared([]byte(value))
}
