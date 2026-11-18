# Operations

How to install and run factory on Kubernetes with the Helm chart in [`deploy/chart`](../deploy/chart). The
example values for our cluster are in
[`deploy/chart/values-autobutler.example.yaml`](../deploy/chart/values-autobutler.example.yaml).

## What gets installed

In the release namespace (`factory`):

- A Deployment running `factory serve`, one replica, with a ServiceAccount of its own.
- A Service on port 80 (default type `LoadBalancer`) in front of the server's port 8080. Plain HTTP.
- A Job that runs `factory migrate up` as a Helm `pre-install,pre-upgrade` hook.
- A `PodMonitor` (`azmonitoring.coreos.com/v1`), only with `metrics.podMonitor.enabled=true`. Off by default.

In the worker namespace (`workers.namespace`, default `factory-workers`):

- The ServiceAccount `factory-worker`, with no RoleBindings and no automounted token.
- A Role and RoleBinding that let the server's ServiceAccount manage worker Jobs: `batch` `jobs` create, get, list,
  delete; `pods` get, list; `pods/log` get; `pods/exec` create, get.
- A policy for worker pods (label `app.kubernetes.io/name: factory-worker`): no ingress; egress to cluster DNS, the
  factory server pods on 8080, and HTTPS. With `networkPolicy.cilium=true` it is a `CiliumNetworkPolicy` that limits
  HTTPS to the hosts in `workers.egress.fqdns` (GitHub and the model API). Otherwise it is a plain NetworkPolicy that
  allows 443 to any address outside `workers.egress.denyCidrs`.

The chart creates no namespaces and no Secrets. The worker Jobs are created by the server, not by the chart.

## Prerequisites

From the cluster checklist in [decisions/0001-agent-runtime.md](decisions/0001-agent-runtime.md#cluster-prerequisites-for-the-infrastructure-engineer):

- Kubernetes 1.33 or newer.
- The namespaces `factory` and `factory-workers` exist. `factory-workers` enforces the Pod Security `restricted` (or
  at least `baseline`) level and has a ResourceQuota that matches `launcher.maxSandboxes`.
- A CNI that enforces NetworkPolicy. Hostname egress rules (`networkPolicy.cilium=true`) need Cilium with FQDN
  policy.
- Worker nodes with enough ephemeral storage for clones: the largest repositories times the workers per node, plus
  build output.
- Image pulls from `ghcr.io/autobutler-org` are allowed.
- A Postgres database reachable from the `factory` namespace.
- Optional: a gVisor RuntimeClass, set with `launcher.runtimeClassName`.

## Secrets

Create these before installing. The names are the chart defaults.

| Namespace | Secret | Key | Holds |
| --- | --- | --- | --- |
| `factory` | `factory-database` | `url` | Postgres URL for the server and the migration Job |
| `factory` | `factory-api-token` | `token` | Bearer token for `/api/v1` |
| `factory` | `factory-github-app` | `private-key.pem` | GitHub App private key (only with `github.appId`) |
| `factory-workers` | `factory-worker-api-token` | `token` | The same value as `factory-api-token` |
| `factory-workers` | `factory-model-api-key` | `api-key` | Model API key, given to workers as `ANTHROPIC_API_KEY` |

```sh
token="$(openssl rand -hex 32)"
kubectl -n factory create secret generic factory-database --from-literal=url='postgres://...'
kubectl -n factory create secret generic factory-api-token --from-literal=token="$token"
kubectl -n factory create secret generic factory-github-app --from-file=private-key.pem=./app.private-key.pem
kubectl -n factory-workers create secret generic factory-worker-api-token --from-literal=token="$token"
kubectl -n factory-workers create secret generic factory-model-api-key --from-literal=api-key='sk-ant-...'
```

The two token Secrets must hold the same value. To rotate it, update both and restart the server.

## Install

```sh
helm upgrade --install factory deploy/chart -n factory \
  -f deploy/chart/values-autobutler.example.yaml \
  --set image.tag=<release> \
  --set github.appId=<app id>
```

`image.tag` sets both the server and worker image tags; `workers.image.tag` overrides the worker's. Leave
`github.appId` out to run without a GitHub App. Then:

```sh
kubectl -n factory rollout status deployment/factory
kubectl -n factory get service factory
```

## Upgrade

Run the same `helm upgrade --install` with the new `image.tag`. The migration Job runs first. If it fails, Helm
aborts the upgrade and the old pods keep serving; the failed Job stays until the next attempt so its logs can be read:

```sh
kubectl -n factory logs job/factory-migrate
```

## Worker Jobs

```sh
kubectl -n factory-workers get jobs -l app.kubernetes.io/name=factory-worker
kubectl -n factory-workers logs job/<name>
```

Kubernetes deletes a finished Job an hour after it ends.

## Turning the launcher off

```sh
helm upgrade factory deploy/chart -n factory --reuse-values --set launcher.enabled=false
```

The server then behaves as it did before the launcher existed and starts no Jobs. The worker namespace resources stay
in place.

## Cutting a release

Push a tag `vX.Y.Z` on `main`. The `images` workflow builds both images for linux/amd64 and linux/arm64 and pushes
`ghcr.io/autobutler-org/factory` and `ghcr.io/autobutler-org/factory-worker` tagged `X.Y.Z`, `vX.Y.Z` and
`latest`, with the version stamped into the binary. Pull requests build both images without pushing. Then install or
upgrade with `--set image.tag=X.Y.Z`. The chart is not published as a package yet; install it from this repository at
the same tag. Locally, `make build/image` and `make build/image/worker` build the two images.

## Known limitations

- The load balancer serves plain HTTP with no TLS, so the API and its bearer token cross the network unencrypted.
  This must be resolved before real use.
- One shared bearer token serves both the API and the workers until #15.
- The server has no `/metrics` endpoint yet (#13), so the PodMonitor has nothing to scrape.
- No dashboards or alerts.
- No NetworkPolicy for the server pods yet.
