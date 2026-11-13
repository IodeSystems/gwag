# Deploy

`gwag deploy` runs a gwag cluster on Docker Compose or minikube with
one command, and writes deployment files for Compose, Kubernetes,
minikube and ECS. It generates plain files (no Helm) from templates
inside the binary, so it works from a downloaded `gwag`.

```bash
gwag deploy up --platform docker-compose          # 1 node
gwag deploy login --platform docker-compose       # saves the admin token to .gw/credentials.json
gwag ctl services list
gwag deploy down --platform docker-compose
```

`up` builds the image, writes the files into `--dir` (default
`./gwag-deploy`) and starts the nodes. `login` reads the admin token
and the addresses from there (Compose) or from the cluster (minikube),
so `gwag ctl peer list` and `gwag ctl services list` work straight away.

## Image

The root [`Dockerfile`](../Dockerfile) builds the gateway: stage 1 runs
`bin/build --no-go` (UI install, GraphQL codegen, Vite build, lint) on
Node 22 and Go; stage 2 builds `gw/cmd/gwag` with the UI embedded;
the runtime is `gcr.io/distroless/static-debian12:nonroot` (44 MB
image, uid 65532, no shell). State is in `/data`, a volume. The
entrypoint is `/gwag`; `/gwag probe URL` is the container health check
(there is no curl).

`deploy up` runs `docker build -t gwag:dev <src>` when `<src>/Dockerfile`
exists (`--src`, default `.`), so run it in a checkout, or pass
`--no-build --image REF` for an image you already have. For minikube
it then runs `minikube image load`. Nothing is pushed: no registry, no
CI publishing. `--image` on `export` is a plain reference; ECS needs
one that Fargate can pull.

## Replicas

`--replicas` is 1 (default) or an odd number, 3 or more.

| Value | Result |
|---|---|
| 1 | Standalone node, no `--nats-peer`. |
| odd, 3 or more | Every node lists the other nodes (k8s: all, itself included) as `--nats-peer`. Survives one lost node at 3. |
| 2 | Accepted with a warning. Under the default `odd` replica policy the buckets stay at R1, and NATS needs both nodes up to elect a meta leader. One node down means no JetStream. |
| even, 4 or more | Accepted with a warning: tolerates no more failures than one node less. |
| 0, over 9 | Refused. |

A 1-node deploy cannot grow into a cluster later. Restarting it with
peers does not carry its data over; the workaround (new nodes, re-register
services, no application data kept) is in
[`cluster.md`](./cluster.md#single-node-to-three). Start with 3 if you
will need 3.

Fresh nodes started together, which is what every platform here does
on first start, formed one group in every run on Compose (3 nodes, 4
starts, 4.0 to 4.7 s to all healthy, `jetstream meta: ... size=3`). The
caution in [`cluster.md`](./cluster.md#replica-swap) is about starting
new nodes into an existing cluster: do that one at a time.

## What every platform gets

- One node per replica with a stable DNS name for `--nats-peer`:
  Compose service names (`gwag-1`), a StatefulSet behind a headless
  Service (`gwag-0.gwag-peers`), Cloud Map (`gwag-1.gwag.local`).
- A persistent volume per node at `/data`.
- Readiness on `GET /api/health` (200 only when serving). Liveness is
  separate and lenient: a node whose peers are down reports 503
  `starting` for as long as they are down, and must not be killed.
- `--shutdown-delay=10s --shutdown-timeout=30s`; the stop grace period
  is 90 s, above the worst case of delay + 2 x timeout (70 s). No
  preStop hook: the image has no shell and SIGTERM already runs the
  drain.
- At most one node unavailable during an update.
- The admin token as a platform secret, passed to the node as
  `GWAG_ADMIN_TOKEN` (or `--admin-token-file`). `gwag up` does not log
  it when supplied this way. All nodes share it. The dev token is still
  generated per node in `/data/dev-token`.
- Ports: GraphQL and HTTP 8080, control-plane gRPC 50090, NATS client
  14222, NATS cluster routes 14248 (internal only).

The token is generated once into `<dir>/secrets/admin-token` and reused
by later `deploy up` or `export` runs. `<dir>/secrets/` has a
`.gitignore` that ignores everything in it. Do not commit it.

## Docker Compose

```bash
gwag deploy up --platform docker-compose --replicas 3
gwag deploy status --platform docker-compose
gwag deploy down --platform docker-compose           # keeps volumes
gwag deploy down --platform docker-compose --purge   # deletes them
```

Files: `docker-compose.yaml` (one service per node, one named volume per
node, health check `/gwag probe`, `stop_grace_period: 90s`),
`secrets/gwag.env` (0600, `env_file`), `secrets/admin-token`. A Compose
file secret is not used because it keeps the host file's mode, and a
0600 file is unreadable to uid 65532. The env var shows in
`docker inspect`.

Ports are published on `127.0.0.1` (`--bind` changes it). Node N gets
host ports `8080+N-1` (HTTP), `50090+N-1` (control plane), `14222+N-1`
(NATS client); `--http-port`, `--cp-port`, `--nats-port` set node 1.
Down takes about 10 s: the nodes drain.

Measured on this box (Docker 29.9.3, Compose v5.7.0, image already built):
1 node healthy after 0.9 s; 3 nodes healthy after 4.0 to 4.7 s in 4
runs (4.4, 4.6, 4.7, 4.0), each ending with `jetstream meta: leader=gwag-N size=3 offline=none`.
Restarting `gwag-2` alone: its health answered 200 within 1 s after
the restart command returned (the restart itself took 11 s, the drain).
A cold build of the image took about 1.5 min.

## Kubernetes and minikube

```bash
gwag deploy up --platform minikube --replicas 3
gwag deploy login --platform minikube
gwag deploy export --platform k8s --replicas 3 --out k8s/    # then kubectl apply
```

Files: `namespace.yaml`, `gwag.yaml`, `secrets/secret.yaml`. Apply in
that order (`export` prints the command). `gwag.yaml` holds:

- headless Service `gwag-peers` with `publishNotReadyAddresses: true`
  (peers must resolve before they are ready) and Service `gwag`
  (ready pods only);
- StatefulSet `gwag` with `volumeClaimTemplates` (`--storage`, 1Gi),
  `RollingUpdate`, `terminationGracePeriodSeconds: 90`, non-root uid
  65532, read-only root filesystem, preferred pod anti-affinity by host;
- readiness `httpGet /api/health`, a `startupProbe` and a
  `livenessProbe` on the TCP port only;
- PodDisruptionBudget `maxUnavailable: 1` (3 or more nodes only).

`podManagementPolicy` is `Parallel`, not `OrderedReady`. A node with
peers is not ready until it has a meta leader, which needs a second
node, so under `OrderedReady` pod 0 would never become ready and pod 1
would never start. It only changes scaling; rolling updates still go
one pod at a time and wait for readiness.

Every pod lists all N nodes, itself included, because one pod template
cannot differ per pod. NATS drops a route to itself: 3 local nodes each
listing all 3 formed `size=3`.

minikube differs in two ways: `imagePullPolicy: Never` (the image comes
from `minikube image load`) and a NodePort Service `gwag-nodeport`
(HTTP 30080, control plane 30090 on `minikube ip`), which `deploy
login` uses.

Checked: `kubeconform v0.6.7 -strict` accepts all four files for k8s and
minikube at 1 and 3 replicas. Not run: any of it against a cluster
(kubectl, minikube and kind are not installed on the box this was written
on).

## ECS (Fargate)

```bash
gwag deploy export --platform ecs --replicas 3 --image ACCOUNT.dkr.ecr.REGION.amazonaws.com/gwag:TAG --out ecs/
aws cloudformation deploy --template-file ecs/cloudformation.json --stack-name gwag \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides VpcId=vpc-... SubnetA=subnet-... SubnetB=subnet-...
```

Files: `cloudformation.json` (the stack) and `taskdef-gwag-N.json` (one
per node, for `aws ecs register-task-definition --cli-input-json`; its
`REPLACE_*` values are the account-specific ids that the stack fills in
itself). `deploy up`, `down`, `status` and `login` do not support ECS.
Read the token with the `AdminSecretArn` output:
`aws secretsmanager get-secret-value --secret-id ARN --query SecretString --output text`,
then `gwag login --name ecs --gateway HOST:50090 --endpoint http://ALB --token TOKEN`.

The stack has one ECS service per node (desired count 1):

- Peer DNS: each service registers in a Cloud Map private namespace
  (`NamespaceName`, default `gwag.local`) as `gwag-N`; nodes dial
  `gwag-M.gwag.local:14248`.
- Volume: EFS (encrypted, elastic throughput), one access point per node
  (uid 65532). Chosen over EBS because an ECS-managed EBS volume is deleted
  with its task and is not reattached to the replacement, so a replaced
  node would start with an empty data directory and a new node ID. The
  cost: JetStream fsyncs over NFS. Not measured.
- Load balancer: internet-facing ALB on port 80, target group (type ip)
  with health check `/api/health` expecting 200, deregistration delay
  30 s. The control plane is not behind the ALB; reach it at the task IPs
  or through Cloud Map (`InternalCidr`, default `10.0.0.0/8`, may reach
  port 50090).
- Deployment: minimum healthy 0 %, maximum 100 % per service, so the old
  task stops before the new one starts and two never share a data
  directory or a node name; circuit breaker with rollback.
- One node at a time across services: CloudFormation updates services in
  parallel, and a `DependsOn` chain would deadlock a first create
  (service 1 needs service 2 to be healthy). So each node has its own
  image parameter, `Image1` to `ImageN`. Update one per stack update and
  wait for `gwag ctl peer list` to show every node before the next.
- Token: a Secrets Manager secret with a generated 64-character hex
  value, injected as `GWAG_ADMIN_TOKEN`. The execution role may read it.
- `HealthCheckGracePeriodSeconds` 180, container health check `/gwag
  probe --live` (any HTTP answer), `StopTimeout` 90.

`AssignPublicIp` defaults to `ENABLED` for public subnets. With private
subnets set `DISABLED` and give them a NAT gateway or VPC endpoints, or
the image cannot be pulled. `SubnetA` and `SubnetB` must be in different
zones.

Checked: `cfn-lint 1.22.5` reports nothing for 1 and 3 replicas. Not
done: a deploy to AWS. Untested: that the node DNS names resolve in time
for the first peers, how long a first create takes, EFS fsync latency,
and the update sequence above.

## Upgrades

Pull the new image and follow [`cluster.md`](./cluster.md#upgrades):
rolling restart (Compose: `docker compose up -d --no-deps gwag-N`, one
node at a time, wait for `gwag ctl peer list`; Kubernetes: change the image,
the StatefulSet does one pod at a time) or the replica swap runbook.
`gwag ctl cluster finalize` afterwards. A replica swap starts new nodes
into an existing cluster one at a time; the generated files do not do
that for you.

## What is not tested

- Kubernetes and minikube: manifests validated offline only.
- ECS: template linted only.
- Compose with `--replicas 2`: started and healthy, but its failure
  behaviour was not exercised beyond what [`cluster.md`](./cluster.md)
  already measured.
- A node lost and replaced on any platform other than the Compose
  restart above.

## Related

- [`running.md`](./running.md) — topologies, routing, measured failure modes.
- [`cluster.md`](./cluster.md) — flags, bootstrap rule, upgrades, load balancer probe.
- [`operations.md`](./operations.md) — health states and drain.
