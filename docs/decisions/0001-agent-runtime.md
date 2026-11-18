# 0001: agent runtime for worker sandboxes

- Status: proposed
- Issue: [#9](https://github.com/autobutler-org/factory/issues/9)
- Affects: [#11](https://github.com/autobutler-org/factory/issues/11),
  [#12](https://github.com/autobutler-org/factory/issues/12),
  [#15](https://github.com/autobutler-org/factory/issues/15),
  [#17](https://github.com/autobutler-org/factory/issues/17)

## Decision

For v1, factory runs each worker sandbox as a **plain Kubernetes Job** with one pod. The factory server creates the
Jobs, execs into them through the Kubernetes exec API, and deletes them. It can run the pods under gVisor through a
`RuntimeClass` when the cluster has one. Agent Substrate is **not adopted for v1**. We revisit it once v1 runs (see
[Revisiting Substrate](#revisiting-substrate)).

Why, in short:

1. **No exec path.** Substrate v1.0.0 has no exec, attach, or shell into a running actor. Developer exec (#12) is a
   decision the owner has already made, so we would have to build it ourselves inside the sandbox. Kubernetes gives it
   to us.
2. **Suspend buys nothing for this workload.** A coding session is busy for hours. Substrate never suspends a busy
   actor on its own; suspend only happens when a caller asks. The density Substrate is built for comes from idle
   actors, and our workers are not idle.
3. **Cluster cost.** Substrate needs three Kubernetes feature gates that are off by default, a Postgres of its own,
   an object store, privileged worker pods, and its own control plane, router, and egress gateway. That is a second
   platform to run before factory runs one item.
4. **Time box.** #9 says that if a proof of concept is not running inside the time box, the default is Kubernetes
   Jobs behind the same worker protocol. No proof of concept was run (see [What is unproven](#what-is-unproven)).

What we give up, and why it is acceptable for v1: Substrate's egress gateway can keep the GitHub token and the model
API key out of the sandbox entirely. With Jobs, the sandbox holds a short-lived, per-repository GitHub token and the
model API key. #11 already scrubs the environment of the gate and the agent session, and the tokens expire within an
hour. This is the main reason to revisit Substrate later.

## How factory runs workers (the Jobs design)

### Mapping: one sandbox per work item attempt

A sandbox is a Job with `backoffLimit: 0` and `restartPolicy: Never`. Its one container runs `factory worker`, which
registers with the factory server, claims **one** item, drives it to a result, and exits. The Job then finishes and
Kubernetes deletes it an hour later (`ttlSecondsAfterFinished`), so its logs stay readable for that hour.

One sandbox per attempt, rather than a long-lived worker per slot, because:

- every item starts from a clean filesystem, so nothing from one item (clones, credentials, agent state) leaks into
  the next;
- exec (#12) has one obvious target: the sandbox of the worker that holds the item;
- a crashed or stolen item needs no cleanup inside a shared worker. The sandbox ends, and the item goes back to
  `ready` through the lease (#6).

The launcher does not choose which item a sandbox runs. It only keeps the right number of sandboxes alive. The worker
claims through the protocol, as #6 and #11 describe.

### Who creates sandboxes, and when

The factory server, on a timer (every 10 seconds is a reasonable start), calls `sandbox.Reconcile`:

```go
ready := store.CountReady(ctx)                       // work_items in ready
live := liveSandboxes(rt.List(ctx))                  // Pending or Running
busy, idle := splitByClaim(live, registry)           // join on FACTORY_SANDBOX
plan := sandbox.Reconcile(sandbox.Counts{Ready: ready, Busy: busy, Idle: idle, Max: cfg.MaxWorkers})
for range plan.Start { rt.Ensure(ctx, "worker-"+randomSuffix()) }
for _, s := range pickIdle(idle, plan.Stop) { rt.Destroy(ctx, s.Name) }
```

`Reconcile` starts enough sandboxes that every ready item has an idle one to claim it, never goes above the maximum,
and stops only idle sandboxes (ones whose worker holds no claim, including ones still starting). A busy sandbox is
never stopped by reconcile; it exits when its item is done. Each worker is told its sandbox name in
`FACTORY_SANDBOX` and sends it when it registers (#6), which is how the server joins a sandbox to a worker and an item.

Destroying a sandbox deletes its Job, which sends the worker `SIGTERM`. #11 already says the worker releases its item
on `SIGTERM` without counting an attempt.

### Exec

`factory work exec <id>` reaches the server. The server authorizes the caller (#15), writes the start event, looks up
the sandbox of the worker holding the item, and calls `Runtime.Exec`, which streams stdin, stdout, stderr, and an
optional TTY through the Kubernetes exec API (WebSocket, falling back to SPDY, as `kubectl exec` does). When the
command ends the server writes the end event with the duration. Developers never hold cluster credentials; the
server's ServiceAccount does, scoped to the worker namespace.

`Exec` takes the caller's streams (`io.Reader` and `io.Writer`) rather than returning a stream object. The HTTP
handler in #12 will hold a WebSocket to the CLI and pass its two halves in; this keeps the runtime free of any
transport of its own.

### Credentials

- **Worker to factory:** a projected ServiceAccount token with audience `factory`, mounted at
  `/var/run/factory/token` and refreshed by the kubelet. The pod does not mount the default Kubernetes API token
  (`automountServiceAccountToken: false`). The server verifies the token against the cluster's OIDC issuer (or with a
  TokenReview) and binds the worker to the pod name in the token's `kubernetes.io` claim.
- **GitHub:** the server mints an installation token per repository listed on the item (#10, #11) and hands it to the
  worker over the protocol. Nothing is configured in the pod.
- **Model API key:** a Kubernetes Secret in the worker namespace, injected into the worker container. The worker
  passes it to the agent session only, never to the gate (#11's environment scrubbing).

### The worker image

Not added here. It needs git, the agent CLIs, and the factory binary, and #11 builds it. The Job only needs its
digest-pinned reference.

## Answers to the questions in #9

Substrate sources are at tag `v1.0.0` (commit `b34fbee`) of
[agent-substrate/substrate](https://github.com/agent-substrate/substrate/tree/v1.0.0). Paths below are relative to
that tree. "Confirmed" means read in docs or code; "unconfirmed" means not stated there or not tried on a cluster.

| # | Question | Answer | Status | Source |
| --- | --- | --- | --- | --- |
| 1 | Fit | One actor per work item fits the API. Suspend does not help a busy session: nothing suspends a running actor unless a caller asks. It could help only while waiting on CI or review, which our worker protocol already handles by releasing the sandbox. | Confirmed | `docs/concepts/architecture.md:497-499`; `api/ateapi/v1/ateapi.proto:2704-2723`; `internal/ateworker/suspend` has no importer |
| 2 | Lifecycle API | Yes. gRPC `Control` service: `CreateActor`, `ResumeActor`, `SuspendActor`, `DeleteActor`, `GetActor`, `ListActors`. Importable as `go.ate.dev/substrate/api/ateapi/v1`; importing it pulls only grpc and protobuf. States: `RESUMING`, `RUNNING`, `SUSPENDING`, `SUSPENDED`, `CRASHED`, `DELETING`, `REVERTING`. A new actor starts `SUSPENDED` and runs on the first `ResumeActor`. | Confirmed | `api/ateapi/v1/ateapi.proto:22,25-63,879-889`; `docs/concepts/architecture.md:435-443`; `docs/guides/authentication.md:178`; dependency check below |
| 3 | Workload | An OCI image runs under gVisor; the Claude Code demo runs `claude --print` in one. Nested containers are out: `SYS_ADMIN` cannot be granted. SSH to GitHub is blocked (only HTTP(S) and passthrough TLS leave). Whether a full `make check` and push work, and how file watchers and build tools behave under gVisor, was not tried. | Partly confirmed | `demos/claude-code-multiplex/README.md`; `docs/reference/api-guide.md:375`; `docs/guides/egress-traffic.md:45-48` |
| 4 | Credentials | Yes for header-based credentials over HTTPS: the egress gateway terminates TLS and replaces a placeholder header (`Authorization` for git and the GitHub API, `x-api-key` for the model API) with a value from a Kubernetes Secret. The actor must trust the gateway CA. The agent CLI then needs only a placeholder key. Not tried with git or Claude Code; the Claude Code demo puts the real key in the actor's environment instead. | Docs confirmed, use unconfirmed | `docs/guides/egress-credential-injection.md:24,96-99,262`; `docs/guides/egress-trust-bundle.md:91-127`; `demos/claude-code-multiplex/agent-luna-template.yaml.tmpl:21,41-42` |
| 5 | Identity | Yes. The gateway can inject an actor JWT (`sub` = `actor/<atespace>/<name>`, audience we choose, 5 to 60 minutes) into requests to the factory API; we verify it against the issuer's JWKS. No revocation; tokens are cached by actor name. | Confirmed | `docs/guides/actor-jwts.md:62,162,209`; `docs/guides/egress-credential-injection.md:145` |
| 6 | Developer access | No. No exec, attach, or shell RPC, and no such `kubectl ate` verb (it has get, create, update, delete, resume, suspend, logs, top). Logs come from worker pod logs. The sandbox demo runs commands through its own unauthenticated HTTP endpoint, which is the only pattern shown. | Confirmed | `api/ateapi/v1/ateapi.proto:25-260`; `cmd/kubectl-ate/internal/cmd/root.go:24-31`; `cmd/kubectl-ate/internal/cmd/actor.go:391`; `demos/sandbox/README.md:18` |
| 7 | Storage | Files persist across suspend in `durableDir` volumes and the root filesystem overlay at `VOLUMES` or `ROOTFS` fidelity; each snapshot is uploaded to the object store. Per-actor CSI volumes exist but are a preview feature. Snapshot cost for a large repository was not measured. | Partly confirmed | `docs/concepts/architecture.md:537-545`; `docs/reference/sandbox-classes.md`; `docs/guides/csi-volumes.md:13` |
| 8 | Observability | Partly. Substrate exports OTel traces, metrics, and logs with parent-based sampling, so our gRPC calls can carry our trace into it. The actor's own telemetry reaches our collector only through an egress rule. Joining it to a work item's trace is our job. | Confirmed (docs) | `docs/guides/observability.md:447,524-528` |
| 9 | Operating cost | Feature gates `ClusterTrustBundle`, `ClusterTrustBundleProjection`, `PodCertificateRequest` and the `certificates.k8s.io/v1beta1` API (off by default as of Kubernetes 1.36); its own Postgres; GCS or S3 for snapshots; worker pods running as root with `SYS_ADMIN`, `NET_ADMIN`, unconfined seccomp and AppArmor; `/dev/kvm` only for microVM. No minimum Kubernetes version is documented; the code builds against Kubernetes API v0.37. | Confirmed, except the version floor | `hack/create-kind-cluster.sh:105-111`; `docs/guides/postgres.md`; `docs/concepts/architecture.md:337`; `docs/reference/api-guide.md:55-85`; `docs/reference/sandbox-classes.md:20`; `go.mod:72-75` |
| 10 | Fallback | Kubernetes Jobs, described above and implemented in `internal/sandbox`. | Built here | this document |

Notes on the answers:

- **No automatic idle suspend in v1.0.0.** The quickstart says the counter actor is checkpointed "when the actor
  becomes idle" (`docs/get-started/quickstart.md:93`), but the architecture doc says suspend is "Triggered by an
  explicit `SuspendActor` call" (`docs/concepts/architecture.md:497-499`). The worker-side requester
  (`internal/ateworker/suspend/suspend.go`, which calls `RequestActorSuspend`) is imported by nothing in v1.0.0 or on
  `main` as of 2026-11-18. The Claude Code demo's dashboard suspends and resumes agents itself
  (`demos/claude-code-multiplex/ui/server.go:285`).
- **Golden snapshots would freeze our worker.** By default an actor resumes from a memory snapshot of the template's
  first boot. A worker that had registered or opened connections at boot would be cloned into every actor. Setting
  `goldenSnapshotConfig.mode: DISABLED` makes each actor cold-boot (`api/ateapi/v1/ateapi.proto:1307-1323`;
  `docs/reference/api-guide.md:668,691`).
- **Actors get no per-actor environment.** Containers, env, and limits live on the immutable `ActorTemplate`. An actor
  can read its own name, atespace, and UID from a projected file (`api/ateapi/v1/ateapi.proto:1606-1631`), which would
  be enough for the worker to identify itself.
- **The factory server would need network access** to the Substrate API: its namespace labeled
  `ate.dev/api-access=true` (`docs/guides/authentication.md:167-171`), and a principal granted atespace `editor`
  (`docs/guides/authorization.md`).

### Dependency weight of the Substrate client

The module `go.ate.dev/substrate` lists about 250 requirements, including the AWS and GCS SDKs and Envoy's control
plane. Importing only `go.ate.dev/substrate/api/ateapi/v1@v1.0.0` into an empty module and running `go mod tidy`
gave a `go.mod` with one direct and six indirect requirements (grpc, protobuf, genproto rpc, `x/net`, `x/sys`,
`x/text`) and a 40-line `go.sum`. Module graph pruning keeps the rest out. If we adopt Substrate later, importing the
generated package is fine; there is no need to generate our own client. The `internal/ateclient` helper the demos use
is not importable (it is under `internal/`).

## Revisiting Substrate

Substrate becomes worth it when keeping credentials out of the sandbox matters more than the cost of running it, or
when we want many idle agents waiting on review. The mapping, if we adopt it:

- One atespace for factory. One `ActorTemplate` per worker image version, with `goldenSnapshotConfig.mode: DISABLED`
  and `preferredFidelity: SNAPSHOT_FIDELITY_ROOTFS`, a `durableDir` volume for clones, and the egress trust bundle
  projected.
- One actor per work item attempt, named after the attempt. The server calls `CreateActor`, then
  `CreateActorEgressPolicy` (factory API over `http` with an `actorJwt` `Authorization` header; GitHub over `https`
  with `Authorization` from a per-item Secret; the model API over `https` with `x-api-key` from a Secret), then
  `ResumeActor`. It never suspends a busy actor, and calls `DeleteActor` when the attempt ends.
- Exec would need a small exec server in the worker image, reached through `atenet-router`, with the factory server
  as the only allowed caller. The router carries HTTP only (`docs/concepts/architecture.md:367-375`), so an
  interactive TTY would need a request/response protocol of our own.
- A Substrate `Runtime` implementation slots in beside `Jobs`; `Reconcile` does not change.

## Cluster prerequisites (for the infrastructure engineer)

### Needed for v1 (Kubernetes Jobs)

- [ ] A namespace for worker sandboxes (proposed `factory-workers`), with the Pod Security `restricted` (or at least
      `baseline`) level enforced.
- [ ] A ResourceQuota on that namespace that matches the configured maximum number of workers, as a backstop.
- [ ] A ServiceAccount `factory-worker` in that namespace with **no** RoleBindings.
- [ ] A Role and RoleBinding in the worker namespace for the factory server's ServiceAccount: `batch/jobs`: create,
      get, list, delete; `pods`: get, list; `pods/log`: get; `pods/exec`: create, get.
- [ ] The factory server can verify projected ServiceAccount tokens: either the cluster's service account issuer
      discovery is reachable from the server (`/.well-known/openid-configuration` and its JWKS), or the server's
      ServiceAccount may `create` `authentication.k8s.io/tokenreviews`.
- [ ] A Secret in the worker namespace holding the model API key (name to be set in #17).
- [ ] NetworkPolicy for worker pods: egress only to cluster DNS, the factory server's Service, `github.com` and
      `api.github.com` on 443, and the model API on 443; no ingress. This needs a CNI that enforces NetworkPolicy, and
      hostname egress rules need a CNI that supports FQDN policy (for example Cilium); otherwise allow 443 out and
      deny cluster-internal ranges.
- [ ] Node ephemeral storage sized for clones: the largest repositories times the maximum number of workers per
      node, plus build output. #11 sets the pod's `ephemeral-storage` request and limit.
- [ ] Image pulls from `ghcr.io/autobutler-org` are allowed.
- [ ] Optional, recommended: a gVisor `RuntimeClass` (name it `gvisor`) on a node pool with `runsc` installed, so
      agent code runs under a user-space kernel. Factory sets `runtimeClassName` from configuration.
- [ ] Kubernetes 1.33 or newer. The Job fields used (`status.ready`, `ttlSecondsAfterFinished`, the `FailureTarget`
      and `SuccessCriteriaMet` conditions) are GA there. The client is client-go v0.37.2.

### Needed only if Substrate is adopted later

- [ ] Feature gates `ClusterTrustBundle`, `ClusterTrustBundleProjection`, and `PodCertificateRequest` on the API
      server, and `certificates.k8s.io/v1beta1` enabled. Check that our control plane (managed or not) allows this.
- [ ] A PostgreSQL database for Substrate with owner and read/write roles (`docs/guides/postgres.md`), separate from
      factory's.
- [ ] A GCS or S3 bucket for snapshots, and credentials for Substrate's object-store plugin.
- [ ] A node pool for Substrate workers whose namespace allows pods that run as root with `SYS_ADMIN`, `NET_ADMIN`,
      and `NET_RAW` and unconfined seccomp and AppArmor (Pod Security `privileged`), plus the `atelet` DaemonSet with
      a hostPath at `/var/lib/ate` (optionally its own disk for `/var/lib/ate/snapshots`).
- [ ] `/dev/kvm` with nested virtualization on those nodes only if we want the microVM class; gVisor needs a standard
      Linux node.
- [ ] A CNI that enforces NetworkPolicy (Substrate's install relies on it to fence the API and credential provider).
- [ ] Bootstrap owners set to our operators, not the shared `ate-client` ServiceAccount.
- [ ] The factory server's namespace labeled `ate.dev/api-access=true`.

## Proposed edits to other issues

These are proposals for the owner to apply; the issues have not been edited.

**#11, worker runtime**

- Scope, replace "The launcher that starts workers in the cluster, as decided by #9: Substrate actors or Kubernetes
  Jobs" with: "Wire `internal/sandbox` into the server: run `sandbox.Reconcile` on a timer against `Jobs`, as described
  in `docs/decisions/0001-agent-runtime.md`."
- Add: "`factory worker` is one-shot: it registers, claims one item, drives it to a result, and exits 0. It reads
  `FACTORY_URL`, `FACTORY_SANDBOX`, and `FACTORY_TOKEN_FILE`, and sends `FACTORY_SANDBOX` when it registers."
- Add: "The worker image sets an `ephemeral-storage` request and limit on the worker container, and the model API key
  comes from a Secret reference, passed only to the agent session."
- Acceptance, add: "An end-to-end test with the fake runtime: one ready item makes reconcile start one sandbox, and a
  finished item leaves nothing running."

**#12, developer access**

- Exec access, replace "The transport is whatever #9 selects..." with: "The server calls `sandbox.Runtime.Exec`, which
  uses the Kubernetes exec API with the server's ServiceAccount. The CLI talks to the server over a WebSocket; the
  server bridges it to the exec streams. With no command, the server runs `/bin/sh` with a TTY."
- `factory work logs` reads the worker pod's logs through the same ServiceAccount (`pods/log`).

**#15, authentication and authorization**

- Workers, replace "If #9 adopts Substrate... Otherwise it is a projected Kubernetes service account token..." with:
  "The credential is a projected ServiceAccount token with audience `factory`, read from `FACTORY_TOKEN_FILE` on every
  request. The server verifies it against the cluster's OIDC issuer (or a TokenReview), requires the
  `factory-worker` ServiceAccount in the worker namespace, and binds the worker to the pod named in the token."
- Acceptance, add: "A token for another ServiceAccount, or for another audience, is refused."

**#17, deployment**

- Helm chart, replace "Worker resources as decided in #9: a Substrate `WorkerPool`... or a Job template with its
  ServiceAccount and RBAC" with: "The worker namespace, the `factory-worker` ServiceAccount, the server's Role and
  RoleBinding in that namespace (Jobs and pod exec, as in the decision's checklist), the worker NetworkPolicy, a
  ResourceQuota, and values for the worker image, maximum workers, and optional `runtimeClassName`. The server holds
  the Job template in code, not in the chart."
- Deployment, add: "The cluster prerequisites checklist in `docs/decisions/0001-agent-runtime.md` is done."

## What is unproven

No cluster was used. The Substrate quickstart's local path (`hack/create-kind-cluster.sh`, then
`go run ./cmd/ate-setup`) runs Substrate's own scripts, which this spike treated as untrusted and did not run, and it
pulls about 570 MB of images and builds every component. So these remain unproven:

- That the Job spec in `internal/sandbox` is accepted by a real API server and the pod becomes Ready (tests use
  client-go's fake clientset, which does not validate or run anything).
- That exec works end to end, including the WebSocket to SPDY fallback, a TTY, and RBAC on `pods/exec` (tests check
  the request URL and streams only).
- How long a large clone and `make check` take under gVisor, and whether build tools or file watchers misbehave there.
- For Substrate: any of it on a cluster, including credential injection with git and Claude Code, and snapshot cost.

## Implementation

`internal/sandbox`:

- `Runtime`: `Ensure`, `Status`, `List`, `Destroy`, `Exec`.
- `Fake`: an in-memory runtime for server tests.
- `Jobs`: the Kubernetes Job runtime, using client-go.
- `Reconcile`: the pure start-and-stop decision, with table tests.

Nothing calls it yet; the server code it plugs into does not exist on this branch. #11 wires it as described above.
