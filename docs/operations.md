# Operations

How to install and run factory on Kubernetes with the Helm chart in [`deploy/chart`](../deploy/chart). The
values for our cluster are in
[`deploy/chart/values-autobutler.yaml`](../deploy/chart/values-autobutler.yaml).

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
  HTTPS to the hosts in `workers.egress.fqdns` (GitHub and the model API by default; our cluster's values add the
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
  -f deploy/chart/values-autobutler.yaml \
  --set image.tag=<release>
```

`image.tag` sets both the server and worker image tags; `workers.image.tag` overrides the worker's. Our cluster's
values set `github.appId` (not secret) and `service.type: ClusterIP`; on another cluster, leave `github.appId` empty
to run without a GitHub App. On our cluster a release tag does this for you ([Deploying a
release](#deploying-a-release)). Then:

```sh
kubectl -n factory rollout status deployment/factory
kubectl -n factory get service factory
```

## Enqueue a first item

With the server running, port-forward its Service and read the API token from its Secret:

```sh
kubectl -n factory port-forward service/factory 8080:80 &
export FACTORY_API_TOKEN="$(kubectl -n factory get secret factory-api-token -o jsonpath='{.data.token}' | base64 -d)"
```

POST one item to `/api/v1/work` with a title, a body and the repositories to work on. The `Idempotency-Key` header
makes a repeat of the same request return the first item instead of creating another
([api.md](api.md#enqueueing-work)):

```sh
id="$(curl -sS -X POST http://127.0.0.1:8080/api/v1/work \
  -H "Authorization: Bearer $FACTORY_API_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: first-item' \
  -d '{"title": "fix a typo in the README", "body": "README.md says \"recieve\"; make it \"receive\".", "repos": ["autobutler-org/quark"]}' \
  | jq -r .item.id)"
echo "$id"
```

The item starts in `ready`. The launcher plans it, moves it to `in_progress`, and starts a worker Job for it. With a
GitHub App, each repository needs a `.factory/config.toml` on its default branch and the App installed on it;
without one the item goes to `blocked` with the reason. Watch it:

```sh
curl -sS -H "Authorization: Bearer $FACTORY_API_TOKEN" "http://127.0.0.1:8080/api/v1/work/$id/events" | jq
curl -sS -H "Authorization: Bearer $FACTORY_API_TOKEN" "http://127.0.0.1:8080/api/v1/work/$id/graph" | jq
kubectl -n factory-workers get jobs -l app.kubernetes.io/name=factory-worker --watch
```

The events list the item's transitions, oldest first, with the reason for any block. The graph answers `404` until
the item is planned, then shows each step and its state. The web UI at `http://127.0.0.1:8080/` shows the same item.

Check the whole setup with `factory doctor`, through the port-forward and token above:

```sh
FACTORY_SERVER=http://127.0.0.1:8080 factory doctor
```

Each failing check prints its fix; [cli.md](cli.md#factory-doctor) lists the checks.

## Upgrade

On our cluster a release tag upgrades it ([Deploying a release](#deploying-a-release)). Elsewhere, run the same
`helm upgrade --install` with the new `image.tag`. Each new server pod runs the migrations in its
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

Kubernetes deletes a finished Job an hour after it ends, and its log with it. Without cluster access, the API serves
the same log to any holder of the API token: `GET /api/v1/work/{id}/sandboxes` lists an item's sandboxes and
`GET /api/v1/work/{id}/sandboxes/{name}/logs` reads one ([api.md](api.md#sandbox-logs)). The server reads it with the
`pods/log` get its Role grants in the worker namespace.

## Giving a repository its own tools

The worker image carries what every repository needs. A repository that needs more, such as Flutter, gets an image
of its own instead of adding to the shared one:

1. Write a Dockerfile `FROM ghcr.io/autobutler-org/factory-worker:<tag>` that installs the tools and keeps the
   contract at the top of [`Dockerfile.worker`](../Dockerfile.worker): `tini -- factory` as the entrypoint, ending as
   `USER 1000:1000`, every tool on `PATH` (no other environment variable reaches the gate or a session, and the
   gate's `HOME` is an empty scratch directory), and Claude Code left as installed.
2. Build it for the worker nodes' architecture (arm64 here) and push it under a prefix in `launcher.allowedImages`
   (`ghcr.io/autobutler-org/` in our cluster's values), public or otherwise pullable without credentials: the Jobs
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
`latest`, with the version stamped into the binary. Pull requests build both images without pushing. Once both are
pushed, the same run deploys the tag to our cluster ([Deploying a release](#deploying-a-release)). The chart is not
published as a package yet; install it from this repository at the same tag. Locally, `make build/image` and
`make build/image/worker` build the two images.

## Deploying a release

The `deploy` workflow ([`.github/workflows/deploy.yml`](../.github/workflows/deploy.yml)) upgrades the `factory`
release in the `factory` namespace of the `autobutler-factory` AKS cluster. `images.yml` calls it after both images
for a `v*` tag are published; it never runs before they exist. It:

1. Checks out the tag, so the chart and [`deploy/chart/values-autobutler.yaml`](../deploy/chart/values-autobutler.yaml)
   are the tagged ones.
2. Logs in to Azure with GitHub OIDC (`azure/login`) as the deploy identity `id-autobutler-factory-deploy`. No Azure
   credential is stored in GitHub.
3. Gets a kubeconfig with `az aks get-credentials` and converts it with `kubelogin convert-kubeconfig -l azurecli`
   (local accounts are off; Kubernetes access is Entra with Azure RBAC).
4. Runs `helm upgrade --install factory deploy/chart -n factory -f deploy/chart/values-autobutler.yaml --set
   image.tag=<tag> --rollback-on-failure --wait --timeout 10m`. `--rollback-on-failure` is Helm 4's name for
   `--atomic`: a release that is not ready within ten minutes is rolled back to the previous revision and the job
   fails, with the previous release still serving.
5. Writes the Helm history, the rollout status, the server and worker image, and the pods to the job summary.
6. Checks `/readyz` through the API server's Service proxy
   (`kubectl get --raw /api/v1/namespaces/factory/services/factory:http/proxy/readyz`), so no public address is
   needed.

One deploy runs at a time (concurrency group `deploy-production`), and a running deploy is never canceled. If several
are queued, GitHub keeps only the newest.

### Rolling back

Run the workflow by hand with the earlier tag: Actions, `deploy`, Run workflow, `tag: v1.2.3`, or

```sh
gh workflow run deploy.yml --repo autobutler-org/factory -f tag=v1.2.3
```

It deploys that tag's chart, values and images the same way. Only tags that contain `deploy/chart/values-autobutler.yaml`
can be deployed this way; for an older tag, deploy by hand. `helm rollback factory <revision> -n factory` also works
from a machine with cluster access.

Migrations run in the server pod's `migrate` init container, so an upgrade applies them, and a rollback does not undo
them. Rolling back is safe only if the earlier version runs against the newer schema: keep every migration backward
compatible for one release (add columns and tables; drop or rename in the release after).

Running worker Jobs keep the image they started with until they finish; the launcher starts new Jobs on the new tag.
The deploy does not wait for them. The server and worker promise no compatibility across versions yet
([Giving a repository its own tools](#giving-a-repository-its-own-tools)), so a worker on the old image may fail its
step against the new server, and that step then fails or is retried under the lease rules
([architecture/workers.md](architecture/workers.md)). That is accepted for now; to avoid it, tag when
`kubectl -n factory-workers get jobs` shows none running.

### Deploying by hand

If GitHub Actions is down, run the same steps from a checkout of the tag with your own access (see
[`modules/factory/README.md`](https://github.com/autobutler-org/iac/blob/main/modules/factory/README.md) in iac for
the roles you need):

```sh
git checkout v1.2.3
az aks get-credentials --resource-group autobutler-factory --name autobutler-factory
kubelogin convert-kubeconfig -l azurecli
helm upgrade --install factory deploy/chart -n factory -f deploy/chart/values-autobutler.yaml \
  --set image.tag=v1.2.3 --rollback-on-failure --wait --timeout 10m
```

### Setup

The `production` GitHub environment holds the workflow's variables. None is secret, and the workflow reads no secret:

| Variable | Value |
| --- | --- |
| `AZURE_CLIENT_ID` | the iac output `factory_deploy_identity_client_id` |
| `AZURE_TENANT_ID` | the Entra tenant id |
| `AZURE_SUBSCRIPTION_ID` | the subscription holding `autobutler-factory` |
| `AKS_RESOURCE_GROUP` | `autobutler-factory` |
| `AKS_CLUSTER_NAME` | `autobutler-factory` |

The deploy identity trusts only the OIDC subject
`repo:autobutler-org@217851255/factory@1463156387:environment:production`, so only a job in that environment can
log in as it ([iac#127](https://github.com/autobutler-org/iac/issues/127)). Restrict the environment's deployment
branches and tags to `main` and `v*`, and add required reviewers there to gate deploys; the workflow does not change.

What the deploy identity may do, from iac `modules/factory/cluster.tf`, at the scope of the whole cluster:

- Azure Kubernetes Service Cluster User Role: fetch the kubeconfig.
- Azure Kubernetes Service RBAC Cluster Admin: every Kubernetes verb in every namespace.

That is more than the upgrade needs. The chart writes in `factory` and `factory-workers` and one cluster-scoped
StorageClass, and the checks read pods and proxy to the Service. Narrowing it to those namespaces is
[iac#128](https://github.com/autobutler-org/iac/issues/128).

## In-cluster Postgres

Azure will not create the managed flexible server for our subscription, so for now Postgres runs in the cluster
(`postgres.enabled=true`, set in our cluster's values). It is one Postgres 17 pod with its data on an Azure disk.
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

The data is on the claim `data-factory-postgres-0`, backed by an Azure disk. With our cluster's values the claim
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
- Every holder of that token can read every worker's log, which carries the agent transcript: command output and
  file contents the session printed. Logs are not kept after a worker Job is deleted.
- The server has no `/metrics` endpoint yet (#13), so the PodMonitor has nothing to scrape.
- No dashboards or alerts.
- No NetworkPolicy for the server pods yet.
- The in-cluster Postgres has no backups and one replica ([In-cluster Postgres](#in-cluster-postgres)).
