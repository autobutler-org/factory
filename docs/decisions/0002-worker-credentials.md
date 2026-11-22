# 0002: how a worker pod holds and uses its credentials

- Status: proposed
- Issues: [#48](https://github.com/autobutler-org/factory/issues/48), the worker half of
  [#15](https://github.com/autobutler-org/factory/issues/15)
- Affects: [#11](https://github.com/autobutler-org/factory/issues/11),
  [#17](https://github.com/autobutler-org/factory/issues/17),
  [#35](https://github.com/autobutler-org/factory/issues/35)

## Decision

1. **A worker authenticates as its pod.** It sends its projected ServiceAccount token (audience `factory`) once, to
   register. The server verifies it with a Kubernetes **TokenReview**, finds the pod and the Job the launcher created
   for it, and answers with a **per-pod worker credential**: a random secret the server keeps only as a hash, bound to
   that worker, its sandbox and its sandbox key. Every later call (heartbeat, claim, stage, result, release, code host
   token) carries that credential, and the server refuses it for any worker, step, item or sandbox key but its own.
   A pod registers once: the token file is useless after that.
2. **Workers stop receiving the shared `FACTORY_API_TOKEN`.** While workload identity is on (it is whenever the
   launcher is), the shared token is refused for acting as a cluster worker. It keeps working for people and the CLI
   until the people half of #15 lands, and for a worker run outside Kubernetes against a server without the launcher.
3. **The session and the gate cannot reach the worker's credentials** (#48), with three mechanisms in the one
   container the pod already has:
   - the worker makes itself **non-dumpable** (`prctl(PR_SET_DUMPABLE, 0)`) before anything else runs, so a process
     of the same uid without `CAP_SYS_PTRACE` cannot read its `/proc/<pid>/environ`, `mem` or `maps`, or ptrace it,
     and it checks this at startup with a child process and refuses to run if the read succeeds;
   - before any git command that carries a code host token, it **kills every other process in the container**
     (`kill(-1, SIGKILL)`, until none of its uid is left), so nothing a session or gate started is alive while the
     token exists outside the worker's memory;
   - it **never runs a token-bearing git command in state a session could have written**: it resets the bare clone's
     config, removes its hooks, alternates, grafts, shallow file and replace refs, runs git by the absolute path it
     resolved at startup with a fixed `PATH`, and scans what it pushes in that same reset clone. Local git commands
     carry no token at all.
4. **The model key stays with the session.** A session calls the model API itself, so it can read the key. That is
   accepted for now and tracked as a follow-up (a per-sandbox key or a proxy), not built here.

## The problem

An agent session and the gate run as children of `factory worker`: same uid (1000), same container. Since #44 a
Claude Code session runs with `--permission-mode bypassPermissions`, so it can run any command. Before this change a
session could:

- read the worker's environment through `/proc/<worker pid>/environ`, which held the shared `FACTORY_API_TOKEN`;
  reproduced in the worker image under `--cap-drop ALL`, `--security-opt no-new-privileges`, uid 1000 (see
  [Evidence](#evidence));
- read `/var/run/factory/token`, the projected token the Job mounts but nobody verified yet;
- call the API as the worker with the shared token: `POST /api/v1/work/{id}/codehost-token` for its repository, then
  push without the gate or the secret scan, or report results it did not earn. The token is the same for every
  sandbox and for people, so it could act on any item;
- get the code host token without the API: git ran with the token in its environment (`GIT_CONFIG_VALUE_0`) in a
  bare clone the session can write, so a hook (`repo.git/hooks/*`), a config key that runs a command
  (`core.fsmonitor`, `credential.helper`, `core.alternateRefsCommand`, ...), a `git` earlier on `PATH`
  (`/home/factory/.local/bin` comes first and is the session's), or a process left running that reads the git
  child's `/proc/<pid>/environ` would see it.

The secret scan also read only the net diff `base...HEAD`, so a secret added in one commit and removed in the next
was pushed unscanned. That is the same hole from the other side and is fixed with it.

## What a projected token carries, and why TokenReview

A projected ServiceAccount token is a JWT signed by the API server, with `aud` (here `factory`), `exp`, the issuer,
and a `kubernetes.io` claim naming the namespace, the ServiceAccount, and the pod (name and uid) it is bound to; since
1.30 also the node, and a `jti`. A TokenReview returns the same facts as `status.user`: the username
`system:serviceaccount:<namespace>:<name>` and `extra` keys `authentication.kubernetes.io/pod-name` and
`authentication.kubernetes.io/pod-uid`.

| | TokenReview | JWKS (the cluster's OIDC issuer) |
| --- | --- | --- |
| Rejects a token whose pod was deleted | Yes, the API server checks the bound pod | No, only `exp` |
| Needs | `create tokenreviews` for the server's ServiceAccount (`system:auth-delegator`) | the issuer URL, egress to it, key caching and rotation, a JWT library or our own RS256 code |
| Cost per check | one API call | none after keys are cached |

The server checks a token once per pod, at registration, so the API call is cheap. TokenReview needs no new
dependency (client-go is already in), tracks key rotation and bound-object revocation for us, and the server already
talks to the API server for the launcher. **TokenReview it is.** JWKS would make sense for a server outside the
cluster, which we do not have.

The binding: the review must say `authenticated`, carry the audience `factory`, name the launcher's namespace and
ServiceAccount, and name a pod that exists with that uid, is controlled by a Job labeled as the launcher's, whose
sandbox-key annotation becomes the worker's sandbox key and whose name becomes the worker's name. The worker's claimed
name and key are not trusted.

## Options for keeping the session away from the worker

**(a) A second container for sessions and the gate**, sharing only the workspace volume. Separate containers have
separate PID namespaces by default, so the worker's `/proc` is not visible, and the token volume is not mounted there.
This is the strongest isolation. Its costs: the worker cannot start a process in another container without the
Kubernetes exec API (which would put an API credential back in the pod) or an agent of our own in the second
container (a command channel over a shared volume or a Unix socket: start, stream output, signal, exit status, idle
detection). Every session and gate goes through it. The git split gets harder too: the worker's clone has to live
where the session cannot write, and the session still expects a working git in its checkout. It is a redesign of how
the worker runs sessions, not a fix, and it still needs everything in (d).

**(b) A different uid for sessions in the same container.** Pod Security `restricted` forbids privilege escalation
and setuid helpers, and every capability is dropped, so the worker cannot switch uid for a child. A different uid
means a different container (`runAsUser` per container), which is (a).

**(c) One process tree with the credentials out of reach.** What blocks a same-uid read, checked rather than assumed:

- `/proc/<pid>/environ`, `mem` and `maps` and `ptrace` are guarded by `ptrace_may_access`. For a **non-dumpable**
  process, access needs `CAP_SYS_PTRACE` in the process's user namespace, which a pod with every capability dropped
  does not have. A non-dumpable process's `/proc` files also turn owned by root. The flag is per process; children
  get a fresh one at `execve`, so sessions stay debuggable themselves. Verified in the worker image (below).
- Yama `ptrace_scope` does not help: it restricts `PTRACE_ATTACH`, not the `PTRACE_MODE_READ` checks for `environ`,
  and the Docker Desktop kernel has no Yama at all. seccomp `RuntimeDefault` blocks `ptrace` on older runtimes only.
  `hidepid` is a mount option of `/proc` the container runtime does not set.
- A file cannot be hidden from the same uid: the projected token stays readable. So the file must be worth nothing
  once read: hence one registration per pod.
- Any child that holds a secret (git with the token) is dumpable, so it must not run while an untrusted process is
  alive: hence the sweep. A session can daemonize out of its process group (`setsid`), so killing the group is not
  enough; `kill(-1)` from the worker reaches everything of its uid except itself and PID 1 (tini).

**(d) Make a stolen credential worth little.** Bind the worker credential to its own worker, sandbox key and held
steps, so even a stolen one acts only on the chain the session already works on. This is #15's acceptance criterion
and is necessary in any option; by itself it does not stop a session pushing its own branch past the gate.

**Chosen: (c) and (d)**, because together they close both holes with changes inside `factory worker`, the server and
the Job spec, and keep one container and one way of running sessions. (a) stays the next step if (c) proves weaker
than it looks on the cluster's runtime (filed as a follow-up).

## What this closes

- A session cannot read the worker credential (memory, protected) or use the token file (already spent).
- A session cannot get a code host token: the API will not mint one without the worker credential, and git only has
  one after every other process is dead, in a clone reset to known state.
- A session cannot act on another item or sandbox: the credential is bound to one worker and sandbox key.
- The shared token is no longer in any worker pod and is refused for acting as a cluster worker.
- The secret scan covers every commit that is pushed, not the net diff.

## Residual risk, plainly

- **The session can still do anything the session is for.** It writes the branch and its review verdict; it can edit
  the repository code the gate runs (the gate command comes from the base branch, but `make check` runs the branch's
  Makefile). The gate is a check on an honest session, not a boundary. The pull request's CI, branch protection and
  a person's review are.
- **The model key.** The session has it by design and can exfiltrate it to any host the egress policy allows (the
  model API and GitHub). The gate is not given it, but a malicious session can leave it in the worktree for the gate.
- **It rests on kernel behavior.** Non-dumpable protection is Linux semantics; gVisor implements its own. The worker
  tests it at startup with a real child process and refuses to start sessions when the read succeeds, so a runtime
  that differs fails closed rather than silently.
- **The sweep needs a container of its own.** It is on only with `--contained` (the Job sets it). A derived image
  that runs something else alongside the worker loses those processes at every push.
- **Denial of service.** A session can still `kill` or `SIGSTOP` the worker (same uid). That loses the step, not a
  credential.
- **git parses session-written objects** with the token in its environment. A git object-parsing bug would be the way
  in; we rely on git's hardening there.
- **The worker credential lives until the worker is marked `gone`** (a lease of silence), not until the pod is gone.
  Only the worker's memory holds it.

## Evidence

In `ghcr.io/autobutler-org/factory-worker:v0.1.2` under `docker run --init --cap-drop ALL --security-opt
no-new-privileges --user 1000:1000` (kernel 6.12), a parent with `SECRET_TOKEN` in its environment and a child
`sh` reading `/proc/<parent>/environ`, then `/proc/<parent>/maps` and `mem` from Python:

| Parent | environ | maps | mem |
| --- | --- | --- | --- |
| dumpable (as `factory worker` ran) | `SECRET_TOKEN=s3cr3t` | read | read 16 bytes |
| `PR_SET_DUMPABLE 0` | Permission denied | Permission denied | Permission denied |

The tests in the implementation repeat this on every CI run (`internal/worker`, Linux only).

## Follow-ups

- Run sessions and the gate in a second container (option (a)) if the cluster's runtime does not honor
  non-dumpable, or to stop depending on the sweep.
- A per-sandbox or proxied model key, so a session's copy is short-lived or useless outside its pod.

## Rollout

The server and the worker image change together: a new worker cannot register with an old server, and an old worker
cannot register with a new server that has the launcher on. Upgrade the chart (server, RBAC) and the worker image to
the same release, then rebuild every image built `FROM` the worker image (quark's) from that release's tag.
[operations.md](../operations.md) has the steps.

## Implementation

- Layer 1 (#15): TokenReview registration, the per-pod credential and its bindings, the Job spec, the chart's RBAC.
- Layer 2 (#48): non-dumpable worker and its startup check, the sweep, git hygiene, the per-commit secret scan.
