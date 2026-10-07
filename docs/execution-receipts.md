# Execution receipts: how the gateway verifies a Work Context

The gateway's governed execution (`codefly daemon --governed-execution`)
brackets an effect — an edit applied, a test run — with signed receipts, and
admits the effect only under a **Work Context**: the capability a Codefly
authority (Accounts) mints for a task, carried to the gateway as gRPC
metadata. This page says what verifies that capability, what it needs, and
what is therefore unavailable in this release.

## One implementation: core's

The Work Context is a core proto (`codefly.base.v0.WorkContextV1`), and
`github.com/codefly-dev/core/workcontext` is its **only** mint and its only
verify — the deterministic proto marshal, Ed25519 over those bytes. The CLI
does not carry a second one. `pkg/executionrecorder` resolves to core's
`Authenticator` by alias, pinned by a compile-time assertion.

The SDK's leaf, `github.com/codefly-dev/sdk-go/workcontext`, is the **client
side** of the capability and the owner of its **carriers** — the HTTP header
and the gRPC metadata the capability travels in, with the sealed installation
beside it as a pre-check (`x-codefly-work-context`,
`x-codefly-installation-id`, `x-codefly-installation-revision`, and the
operation id). It mints nothing and verifies nothing: what it exports as a
verification entry point is core's, by alias, and it deliberately does not
re-export core's verify-only `Authenticator` (a client holds no issuer
sources). The gateway reads the carrier through
`sdk-go/workcontext/grpctransport` and verifies through core — one carrier,
one verifier, each in the repository that owns it. The leaf has no tag by
design; it is pinned by commit, and only from the commit where its own
earlier second implementation (a hand-written JSON payload signed beside
core's proto encoding, what core's `ErrNotACoreToken` exists to name) was
deleted. `TestNoSecondWorkContextImplementation` holds both: the pin at or
after that commit, and nothing in this module naming the deleted verifier's
surface.

`Authenticator` is the entrypoint for a party that verifies **without
minting**. It assembles core's `Verifier` and calls `Verify`, so there is one
check path — issuer, signature, audience, validity window, chain attenuation,
authorization revision, the seal (installation and its revision, the
principal's epoch, the approved build and its incarnation, the operation
binding), and single-use replay — and a capability carrying a grant hop is
refused (`ErrNeedsIssuer`) rather than accepted unchecked, because the
approval it names is the issuer's record and nobody else's to confirm.

Core's conformance kit certifies the behaviour: `TestWorkContextAuthorityIsCoresAuthenticator`
runs `conformance.RunAuthenticator` against this package's entrypoint, built
field by field from the kit's settings, so every fixture core mints is
accepted or refused here with the same named reason.

## What the authenticator needs, and what the gateway holds

Core refuses to assemble a verifier without every source, and the CLI does
not soften that:

| Need | Where it comes from |
| --- | --- |
| The issuer's **verification keys** | `--execution-authority-jwks`: a JWK Set of OKP/Ed25519 signing keys the issuer publishes, fetched over HTTPS, cached, and refreshed when a capability names a key the set does not hold (`pkg/executionrecorder/jwks.go`). Key distribution only — nothing here decides what a token says. |
| The issuer's **authorization revision**, per tenant | `executionruntime.Config.Revisions` (`workcontext.RevisionSource`) — the issuer's live number; a capability minted at another revision is revoked. |
| The issuer's **seals**: installation, principal epoch, approved build, operation binding | `executionruntime.Config.Seals` (`workcontext.SealSource`) — live state; a capability sealed to a superseded installation, epoch, build incarnation or binding revision is revoked. |
| A **replay store** for single-use capabilities | A process-local memory store, by default. |

The recorder adds one requirement of its own, at the use site: the actor's
effective scopes must grant `evidence`/`append` **naming this producer**
explicitly. Under core's containment rule a scope naming no resource is a
wildcard; the recorder does not take a wildcard for the evidence it appends.

The carrier is whole or absent, never half. The SDK deleted its "extract if
present" entry point on purpose — a capability-bearing path requires the
capability, and a call carrying half the carriers is refused like one
carrying none — and the gateway follows it: with governed execution
configured, a governed effect carrying no Work Context is **refused**
(`Unauthenticated`) instead of running ungoverned, which was the
optional-carrier escape hatch on this side. Paths that are not governed
effects (a dry run, a file write outside the recorder) still take a call
with no carrier, and refuse a presented carrier that is not whole.

## What is unavailable, and why it refuses rather than degrades

The revision and seal sources are the issuer's live records. **This release
carries no client for them** — nothing in this repository can ask Accounts
for a tenant's authorization revision or a principal's seal — and core's
authenticator has no mode without them. So `codefly daemon
--governed-execution` refuses by name, before a child process or any durable
state:

```
--governed-execution needs the Work Context issuer's live authorization-revision
and seal sources, and this release has no client for them; governed execution
is unavailable until one exists
```

That is deliberate. The alternative — keeping the earlier verifier, which
checked a signature and a window and nothing live — would admit a capability
whose installation was superseded, whose principal's epoch advanced, or whose
build was replaced, which is exactly what the seal exists to refuse. A gateway
that cannot verify is not a gateway with a weaker verifier.

`executionruntime.Open` takes the two sources in its configuration and
refuses `nil` for either, before any state directory exists
(`TestOpenRejectsIncompleteAuthorityBeforeState`), so the day a client
exists it is wired there and the command flag that names it is the only
command change.

## Tests that hold this

- `TestWorkContextAuthorityIsCoresAuthenticator` — core's conformance kit in
  authenticator mode, against this package's entrypoint.
- `TestWorkContextAuthorityRefusesStaleIssuerState` — through `Verify`: a
  superseded authorization revision, an advanced principal epoch, an advanced
  installation revision and a replaced build incarnation each refuse a
  capability that verified a moment before, as `ErrRevoked`.
- `TestWorkContextAuthorityRequiresTheProducerBoundEvidenceScope` — the
  wildcard, another producer, another action and another resource kind are
  refused; the exact scope is admitted.
- `TestJWKSKeysFollowTheIssuersRotation` — keys are fetched over TLS once per
  cache lifetime, refreshed when a capability names an unpublished key (not
  more than once a minute), and refused by name when the issuer has not
  published that key.
- `TestNoSecondWorkContextImplementation` — the import gate.
- `TestGatewayExecutionIsUnavailableWithoutTheIssuersLiveSources` — the
  daemon's refusal, with no durable state created.

## What is not verified here

Where `Execution.ImageDigest` comes from on the minting side — the running
build, attested from the pod rather than read back from the approved record —
is the issuer's and the host's; core's own README names it as the check the
kit cannot make. Nothing in this repository mints a Work Context.
