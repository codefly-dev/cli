# The development loop

How a change to a service's code or configuration reaches a running environment:
which verb does what, which step is expensive and which is kept, how to force a
rebuild, and — the part that costs the most time when it is not written down —
how to tell a **stale** build from a **wrong** one.

This is the loop for someone changing an application in a workspace. For the
loop that changes the CLI itself, see [development.md](development.md).

---

## The cycle

```
edit ──▶ codefly run service          local, builds runtime recipes when needed
      └▶ codefly deploy gitops render builds + pushes images      minutes (kept when nothing changed)
         codefly deploy secrets       seeds the environment store one store read per key
         codefly deploy gitops plan   the publication diff        seconds
         codefly deploy gitops publish signed promotion PR         seconds
         codefly deploy gitops observe Argo CD reconciled it       minutes
```

| Step | Verb | What it does | Cost |
|---|---|---|---|
| Run it locally | `codefly run service <name>` | Starts the service and its dependency graph on this machine, injecting connection strings as env vars. Container services with agent-emitted recipes build and load their runtime image before Init; unchanged inputs reuse the loaded image. Stock-image services retain their own image selection. | Seconds when images are already built; the first recipe build can take minutes. |
| Render | `codefly deploy gitops render <module> --env <env>` | Resolves the module, builds and pushes every service image, writes the promotable tree under `deployments/modules/<module>/` and records `.codefly-render.json`. | Minutes — the image builds. Images whose inputs did not change are kept, so a configuration-only edit costs the render and no build. |
| Seed secrets | `codefly deploy secrets --env <env>` | Reads the ExternalSecrets the render projected and resolves each remote property to a source — kept, derived, propagated, generated or required — then writes the store. Values are never printed. | One backend read per remote key, eight at a time: seconds for a small environment, longer for a large one. Run it after a render that added a service or a secret key. |
| Plan | `codefly deploy gitops plan <module> --env <env>` | Shows the exact publication diff — what publishing would change in the GitOps repository — and writes nothing. | Seconds. |
| Publish | `codefly deploy gitops publish <module> --env <env>` | Creates a signed promotion commit and opens or updates its pull request. The cluster changes when that lands and Argo CD syncs it, never from this command. | Seconds. |
| Observe | `codefly deploy gitops observe <module> --env <env>` | Verifies Argo CD reconciled the reviewed revision and stores the evidence. | As long as the rollout takes. See [deployment-completion.md](deployment-completion.md). |
| Escape hatch | `codefly deploy dev <module>/<service> --env <env>` | Builds one service from local code and re-pins only that service's digest in an already-rendered tree. | One image build. The environment then runs code no release describes until the next full render. |

`render`, `snapshot`, `plan` and `publish` evaluate the workspace readiness
verdict before doing any expensive work and refuse with `codefly doctor
workspace`'s diagnostics, so a missing configuration fails in seconds instead of
after the images are built. Every flag of every verb is in
[commands.md](commands.md) and [cli-reference.md](cli-reference.md).

---

## Local container runtime images

After selecting the container backend, the CLI loads and initializes the agent's
builder and asks it for its recipe when the manifest names no runtime image.
A single-image recipe is built using the
same verified buildx execution path as deploy, with `--load` for the local
Docker engine's architecture and no registry push. Native and Nix runtimes do
not enter this path. An agent that emits no recipe, or declares artifact builds
unsupported, retains its existing runtime image selection.

The running agent must advertise `runtime-init-image/v1` before the CLI builds
and supplies a recipe image through `InitRequest.runtime_image` (field 10).
Bindings alone do not prove adoption. An older recipe-producing agent without
the capability is refused before image execution. This handoff follows
[handbook proposal #260](https://github.com/obin-ai/handbook/pull/260) and
[Core #748](https://github.com/codefly-dev/core/issues/748).

The image tag is `codefly-local/runtime:sha256-<64 hex digits>`. The hash uses
the existing image-input identity: verified recipe files, Docker's effective
build context, target and build arguments, selected platform, Go module
manifests and resolved base-image digests. The destination image is normalized
in the build invocation before hashing, avoiding a circular hash; the original
agent plan digest, including its metadata, remains an input. Temporary paths
and builder names are normalized by the existing cache.
There is no timestamp, checkout path or `latest` in the result.

The next run emits and verifies the recipe again, then reuses the cached image
only if its inputs match and Docker still holds the recorded image ID. A source,
recipe, argument or base-image change changes the tag. Inputs that cannot be
identified, or a context that changes during the build, fail the local handoff.
Network downloads inside a Dockerfile's `RUN` remain subject to the same
limitation as the build cache described below.

Generated recipes and their image references stay under
`.codefly/runtime-build/<module>/<service>/`; reuse records stay under
`.codefly/build-cache/`. Nothing is written into `spec.runtime-image` or the
authored service tree. The agent retains responsibility for manifest precedence
and reference validation, including refusing an invalid explicit manifest value
even when the request supplies a valid image. Remote runtime initialization does
not receive a local image.

## What is expensive, and what is kept

The image build is the only step measured in minutes. A build reuses the image
this workspace already built from the same inputs, keyed on a digest over the
bytes that go into the image — every file of the build context, the verified
recipe tree, the exact buildx invocation, the Go module manifests of each
declared module root, and the resolved manifest digest of each base image. A
source file that changes by one byte changes the key.

So:

- **A configuration-only change is cheap.** Editing a value under
  `configurations/`, a workspace or module manifest, or anything a service reads
  at runtime, re-renders the configuration and leaves every image digest where it
  was.
- **A source change is a build.** Its bytes are in the context, so the key moves.
- **Reuse is verified, not assumed.** A pushed image is reused only if the
  registry still serves the recorded manifest digest, a loaded one only if the
  daemon still holds the recorded image ID. Anything the key cannot account for —
  an unresolvable base image, an unwalkable context — builds on the deployment
  path. A local runtime handoff refuses those inputs because it cannot derive
  an identifiable tag for them.

The full rules, the exclusions, and what no digest over inputs can bind are in
[commands.md → Keeping an image whose inputs did not change](commands.md#keeping-an-image-whose-inputs-did-not-change).

When a build is kept rather than run, it says so at info level:

```
no image input changed; keeping the built image  image=… digest=sha256:… identity=sha256:…
```

## Forcing a rebuild

```sh
codefly deploy gitops render payments --env staging --rebuild
codefly deploy dev payments/api --env staging --rebuild
```

`--rebuild` builds every image even when no input changed. It is on `codefly
build service`, `codefly build module`, `codefly ci build`, `codefly ci run`,
`codefly deploy service`, `codefly deploy module`, `codefly deploy dev`,
`codefly deploy gitops render` and `codefly deploy gitops snapshot`.

It bypasses **one** cache: this workspace's record of images it already built,
which lives as one small JSON file per input set under
`<workspace>/.codefly/build-cache/` and can be removed wholesale with `rm -rf`.
A registry layer cache (`--cache-from`/`--cache-to`), a BuildKit layer cache, and
any cache inside a build agent's recipe emission or inside the image build itself
are untouched by it and still have to be cleared their own way.

---

## Telling a stale build from a wrong one

**A stale build is an image that does not contain the change you made.** A wrong
build is an image that contains it and still does not behave as you expect. They
look identical from the outside, and the two obvious checks do not separate them:

- **The image digest does not tell you.** It is a digest of the image, not of
  your source. A stale image has a perfectly valid digest that matches
  everything recorded about it; it is simply the digest of the *previous*
  image, which reads as "nothing changed" rather than as "wrong".
- **Scanning the artifact does not tell you either.** A compiled binary does not
  contain your source text, a string you added may be optimized away, and a
  string you expect to be gone may still be in the binary from a path that is no
  longer reachable. A byte-scan of the artifact answers neither question.

The mechanism that produces a stale build is a **cache keyed on a declaration
about the inputs rather than on the inputs**. Key an image on a dependency
manifest or a lock file, edit only source in a language whose manifest did not
move, and the key does not move: the cache serves the previous binary, the
digest matches its record, the rollout is green, the pods are healthy — and the
behaviour is the old behaviour. Nothing downstream is inconsistent, which is why
this can absorb days. The image-input cache is keyed on the context bytes for
exactly this reason, but the failure class is worth recognizing wherever it
appears, and one residue remains even here: **bytes a build step fetches from
the network itself**. A `RUN` that installs from a mutable package index reads
bytes no input names, so reuse means "the image built from these inputs", not
"the image a build today would produce".

### The question that separates them

*Did the deployed artifact change at all?*

1. **Note the digest before and after.** Read the pinned image out of the
   rendered unit:

   ```sh
   grep -r 'image:' deployments/modules/payments/services/api/
   ```

   `codefly deploy dev` prints `Pinned <image>` for the service it re-pinned, and
   records `image` and `digest` in the `dev` entry of
   `deployments/modules/payments/.codefly-render.json`.

2. **The digest did not move after a source edit:** investigate the source
   checkout and build inputs, then retry with `--rebuild`. The edit may have no
   effect on the compiled artifact, or a cache or recipe may have missed it.
   An unchanged digest after a configuration-only change is expected when no
   image input changed.

3. **The digest moved:** some part of the image changed. This does not prove the
   application binary contains the edit: a base-image update or image metadata
   can change the digest while the application stays stale. Verify the selected
   source revision and exercise behavior specific to the edit in the deployed
   service. Where the build emits revision information, compare it with the
   intended checkout as additional evidence.

4. **`--rebuild` produced the same digest:** the output is unchanged, but that
   alone does not prove the inputs were unchanged or the binary is current.
   Check that you edited the source the build actually reads — `codefly deploy
   dev` builds `--path` if given, else the machine-local service override, which
   may select another checkout. Inspect the recipe's source inputs and any
   downstream cache before concluding the build is reproducible.

`--rebuild` bypasses the CLI's image reuse record. It leaves BuildKit layers and
agent recipe caches in place, so it is a diagnostic step, not proof that the
intended application code reached the image.

### Leaving the escape hatch

`codefly deploy dev` leaves the environment running code no release describes.
`codefly doctor workspace` reports each one as a `gitops_dev_deployment_active`
warning. A full `codefly deploy gitops render <module> --env <env>` re-derives
every image from the workspace, drops the `dev` entries, and says which dev
deployments it cleared.

---

## Related

- [commands.md](commands.md) — every verb and flag, including the image-input
  cache and the registry build cache.
- [orchestration.md](orchestration.md) — the engine underneath `run`, `build` and
  `deploy`.
- [deployment-completion.md](deployment-completion.md) — rendered, applied,
  bootstrapped, healthy.
- [development.md](development.md) — building and testing the CLI itself.
