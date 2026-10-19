package environments

// WorkContextConfiguration is the workspace configuration group a service
// verifies inbound Work Contexts against: where the key set is served and the
// issuer it must be signed under.
//
// Like FederationConfiguration, the group stays AUTHORED and is never
// render-owned: a composition with a host declares it, pointing at the host's
// key set. A native run derives it too, for the composition that has no host
// to point at, and it is handed last so it settles over what an authored group
// leaves empty rather than displacing one.
//
// Without that, a service whose identity comes from the host cannot boot on
// its own, because nothing local serves the key set its verifier loads.
const WorkContextConfiguration = "work-context"

// The two keys a verifier is configured from. A key set without a pinned
// issuer would admit a capability any holder of a key in that set signed under
// any issuer name, so neither is optional.
const (
	WorkContextKeySetURLKey = "authority-jwks-url"
	WorkContextIssuerKey    = "issuer"
)
