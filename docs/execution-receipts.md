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
`Authenticator` by alias, pinned by a compile-time assertion, and
`TestNoSecondWorkContextImplementation` refuses a module that requires or
imports the SDK's earlier copy (which signed a hand-written JSON payload and
is what core's `ErrNotACoreToken` exists to name).

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
