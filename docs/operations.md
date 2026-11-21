# Operations

How to install and run factory on Kubernetes with the Helm chart in [`deploy/chart`](../deploy/chart). The
example values for our cluster are in
[`deploy/chart/values-autobutler.example.yaml`](../deploy/chart/values-autobutler.example.yaml).

## What gets installed

In the release namespace (`factory`):

- A Deployment running `factory serve`, one replica, with a ServiceAccount of its own.
- A Service on port 80 (default type `LoadBalancer`) in front of the server's port 8080. Plain HTTP. The same
  address serves the web UI at `/`; it asks for `FACTORY_API_TOKEN` once per browser tab.
- An init container `migrate` in each server pod that runs `factory migrate up` before the server starts.
- With `postgres.enabled=true`, an in-cluster Postgres: a StatefulSet, a headless Service `factory-postgres`, a
  NetworkPolicy, and, with `postgres.persistence.retainStorageClass.create=true`, a cluster-scoped StorageClass
  `factory-postgres`. See [In-cluster Postgres](#in-cluster-postgres).
- A `PodMonitor` (`azmonitoring.coreos.com/v1`), only with `metrics.podMonitor.enabled=true`. Off by default.

In the worker namespace (`workers.namespace`, default `factory-workers`):

- The ServiceAccount `factory-worker`, with no RoleBindings and no automounted token.
- A Role and RoleBinding that let the server's ServiceAccount manage worker Jobs: `batch` `jobs` create, get, list,
  delete; `pods` get, list; `pods/log` get; `pods/exec` create, get.
- A policy for worker pods (label `app.kubernetes.io/name: factory-worker`): no ingress; egress to cluster DNS, the
  factory server pods on 8080, and HTTPS. With `networkPolicy.cilium=true` it is a `CiliumNetworkPolicy` that limits
  HTTPS to the hosts in `workers.egress.fqdns` (GitHub and the model API by default; the example values add the
  package registries a gate downloads from, such as `pub.dev`, `proxy.golang.org` and `registry.npmjs.org`. A gate
  that needs another host fails at its first download: add the host to that list and upgrade the release). Otherwise it is a plain NetworkPolicy that
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
- A Postgres database reachable from the `factory` namespace, or `postgres.enabled=true` to run one in the cluster.
- Optional: a gVisor RuntimeClass, set with `launcher.runtimeClassName`.

## Secrets

Create these before installing. The names are the chart defaults.

| Namespace | Secret | Key | Holds |
| --- | --- | --- | --- |
| `factory` | `factory-database` | `url` | Postgres URL for the server and its migrate init container |
| `factory` | `factory-postgres` | `password` | Password of the in-cluster Postgres (only with `postgres.enabled`) |
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

Run the same `helm upgrade --install` with the new `image.tag`. Each new server pod runs the migrations in its
`migrate` init container before the server starts. If they fail, the new pod never becomes ready, the old pod keeps
serving, and the kubelet retries with backoff. Add `--wait` to make `helm upgrade` fail when that happens. To read
the migration's output:

```sh
kubectl -n factory logs deployment/factory -c migrate
```

## Worker Jobs

```sh
kubectl -n factory-workers get jobs -l app.kubernetes.io/name=factory-worker
kubectl -n factory-workers logs job/<name>
```

Kubernetes deletes a finished Job an hour after it ends.

## Giving a repository its own tools

The worker image carries what every repository needs. A repository that needs more, such as Flutter, gets an image
of its own instead of adding to the shared one:

1. Write a Dockerfile `FROM ghcr.io/autobutler-org/factory-worker:<tag>` that installs the tools and keeps the
   contract at the top of [`Dockerfile.worker`](../Dockerfile.worker): `factory` as the entrypoint, ending as
   `USER 1000:1000`, every tool on `PATH` (no other environment variable reaches the gate or a session, and the
   gate's `HOME` is an empty scratch directory), and Claude Code left as installed.
2. Build it for the worker nodes' architecture (arm64 here) and push it under a prefix in `launcher.allowedImages`
   (`ghcr.io/autobutler-org/` in the example values), public or otherwise pullable without credentials: the Jobs
   have no `imagePullSecrets`. Keep it small enough to pull within `launcher.pendingTimeout`.
3. Name it in the repository's `.factory/config.toml` with a tag or digest:
   `image = "ghcr.io/autobutler-org/quark-worker:1.4.0"`.

The next item planned for the repository runs in that image. An item already planned keeps the image it was planned
with until it is replanned. An image outside the allowlist, or written without a tag or digest, blocks the item with
the reason; it never falls back to the default image. The image runs in the cluster with the model API key and a
code host token, so the allowlist (`launcher.allowedImages`, `--launcher-allowed-images`) is the control on what
code may run there; leave it empty to keep every repository on the default image.

The base tag pins the worker binary as well. The server and the worker promise each other no compatibility across
versions yet, so rebuild each derived image from the new base tag when you cut a factory release, and bump the tag in
the repository's config.

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

## In-cluster Postgres

Azure will not create the managed flexible server for our subscription, so for now Postgres runs in the cluster
(`postgres.enabled=true`, set in the example values). It is one Postgres 17 pod with its data on an Azure disk.
**There are no backups.**

### Secrets

The chart creates no Secrets. Create the password, then the URL the server reads, before installing. The password
is hex so it needs no escaping in the URL:

```sh
password="$(openssl rand -hex 24)"
kubectl -n factory create secret generic factory-postgres --from-literal=password="$password"
kubectl -n factory create secret generic factory-database \
  --from-literal=url="postgres://factory:${password}@factory-postgres.factory.svc.cluster.local:5432/factory?sslmode=disable"
```

The host is `<release>-postgres.<namespace>.svc.cluster.local`; the commands above are for release `factory` in
namespace `factory`. The user and database are `postgres.user` and `postgres.database` (both `factory`).

`sslmode=disable` is acceptable here because the connection never leaves the cluster network and the chart's
NetworkPolicy admits port 5432 only from this release's server pods. It is still unencrypted traffic between
pods, a known gap.

The password Secret is read only when the data directory is first created. Changing it later does not change the
password. To rotate, change it in Postgres, then update both Secrets and restart the server:

```sh
kubectl -n factory exec -it factory-postgres-0 -- psql -U factory -d factory -c "ALTER USER factory PASSWORD '<new>'"
```

### Install

Run the `helm upgrade --install` from [Install](#install). On a first install the database is still starting when
the server pod starts: its `migrate` init container fails and is retried with backoff (the pod shows
`Init:Error` or `Init:CrashLoopBackOff` for a minute or two) until Postgres answers, then the server starts.

```sh
kubectl -n factory rollout status statefulset/factory-postgres
kubectl -n factory rollout status deployment/factory --timeout=10m
```

### Connecting

```sh
kubectl -n factory exec -it factory-postgres-0 -- psql -U factory -d factory
```

The socket connection inside the pod needs no password. Nothing outside the server pods can reach port 5432 while
`networkPolicy.enabled` is true; use `kubectl exec` or `kubectl port-forward` for anything else.

### What survives what

The data is on the claim `data-factory-postgres-0`, backed by an Azure disk. With the example values the claim
uses the chart's StorageClass `factory-postgres` (`reclaimPolicy: Retain`, StandardSSD_LRS); with the cluster
default `managed-csi` the reclaim policy is Delete.

| Event | Data |
| --- | --- |
| Pod restart, crash, or rollout | Kept. The same disk is mounted again. |
| Node drain, upgrade, or replacement | Kept. The disk detaches and attaches to the new node. An LRS disk is zonal, so the pod can only run on a node in the disk's zone; with none there it stays Pending. |
| `helm uninstall` | Kept. Helm never deletes a StatefulSet's claims, and the claim retention policy is Retain. Installing again with the same release name and namespace reuses the claim. The StorageClass is deleted, which does not affect the bound volume. |
| Scaling the StatefulSet down | Kept, for the same reason. |
| Deleting the claim | With the Retain class, the PersistentVolume and the disk are kept (the volume becomes Released; a human must clear its `claimRef` to bind it again). With `managed-csi` or any Delete class, the disk is deleted and the data is gone. |
| Deleting the AKS cluster | **Gone.** A dynamically provisioned disk lives in the cluster's node resource group (`MC_...`), which Azure deletes with the cluster, whatever the reclaim policy. |

There are no backups. Take a dump before anything risky:

```sh
kubectl -n factory exec factory-postgres-0 -- pg_dump -U factory -d factory --format=custom > factory.dump
```

### Moving to a managed server later

1. Stop writes: `kubectl -n factory scale deployment/factory --replicas=0`.
2. Dump: the `pg_dump` command above.
3. Restore into the new server, which must exist with an empty database:
   `pg_restore --no-owner --dbname='<new url>' factory.dump`.
4. Point the server at it: `kubectl -n factory create secret generic factory-database --from-literal=url='<new url>'
   --dry-run=client -o yaml | kubectl apply -f -`.
5. `helm upgrade --install ... --set postgres.enabled=false` (or set it in your values). The server comes back on the
   new database. The claim and disk stay until deleted by hand:
   `kubectl -n factory delete pvc data-factory-postgres-0`, then the Released PersistentVolume and its disk.

### Limitations

- One replica: the database, and so the server, is down while the pod restarts or moves to another node.
- No backups. Losing the disk or the cluster loses the data.
- No TLS between the server and Postgres (`sslmode=disable`).
- Major-version upgrades are manual: a new major cannot open the old data directory, so dump, install the new major
  on a new claim, and restore. `renovate.json` keeps Renovate on Postgres 17.
- Deferred: backups, replication, TLS for the database connection, a metrics exporter, and a disk owned by Terraform
  outside the node resource group.

## Known limitations

- The load balancer serves plain HTTP with no TLS, so the API and its bearer token cross the network unencrypted.
  This must be resolved before real use.
- One shared bearer token serves both the API and the workers until #15.
- The server has no `/metrics` endpoint yet (#13), so the PodMonitor has nothing to scrape.
- No dashboards or alerts.
- No NetworkPolicy for the server pods yet.
- The in-cluster Postgres has no backups and one replica ([In-cluster Postgres](#in-cluster-postgres)).
