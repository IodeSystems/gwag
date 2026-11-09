# Cutover: blue to green with traffic

A whole cutover from one 3-node gwag cluster (blue) to another
(green) in Docker Compose, with Envoy in front and client traffic the
whole time. The guide is [`docs/cutover.md`](../../docs/cutover.md);
this runs its [runbook](../../docs/cutover.md#runbook-in-order).

```
./cutover.sh             # build both images, run, check, remove everything (3.5 min; 87 s without the builds)
./cutover.sh --no-build  # reuse gwag:cutover and gwag-cutover-app
KEEP=1 ./cutover.sh      # leave the containers up at the end
```

Needs Docker with Compose v2, `curl`, `jq`, `openssl`, `bc`. Host
ports 28000-28192 on 127.0.0.1.

```
docker-compose.yaml   blue-0..2, green-0..2 (gwag:cutover, the repo Dockerfile), envoy, app, traffic, cli
envoy.yaml            :10000 clients (weights blue/green from runtime keys), :10001 green only, :9901 admin
cmd/app/              upstream OpenAPI service registered with both clusters; event producer and consumer
cmd/app/blue.json     spec registered with blue: getItem at /items/{id}
cmd/app/green.json    spec registered with green: getItem at /v2/items/{id} (the visible difference)
cmd/traffic/          20 clients through Envoy; counts every failed request
Dockerfile            image of app and traffic
cutover.sh            the run
```

What the script does, in order:

1. Starts blue and Envoy (all traffic to blue), the app (registers with
   blue; produces `orders.v1.OrderShipped` events on blue, consumes them
   as group `notify`), mints a system key on blue for green's mirror
   connection, sets some operator state on blue (an app key, a
   deprecation, an external dependent), starts green and registers the
   app with it, then starts the traffic.
2. Replay: mints a system key on green and writes it to the file blue
   reads (`--replay-verify-token-file`). Checks that `ListItems` and
   `AddNote` match and `GetItem` differs only in the upstream path.
3. State copy: `gwag cluster freeze`, `export`, `import --dry-run`,
   `import`, `unfreeze`; checks green's export equals blue's.
4. `gwag cluster mirror` on green; waits for lag 0.
5. `gwag cluster switchover` 5, 50, 100 percent; prints
   `switchover_total` and `switchover_green_errors_total` summed over
   blue.
6. Envoy to green (`runtime_modify`); waits until Envoy has no request
   open to blue and sends it no new one.
7. Events: stops the producer, waits for lag 0 and the blue consumer at
   0 pending, stops the mirror, moves the consumer, then the producer,
   to green.
8. Closes the app's blue registration and stops blue (SIGTERM,
   `--shutdown-delay 5s`), logging blue-0's `/api/health` as it goes
   `serving`, `draining`, refused.

It fails unless every client request succeeded, green holds blue's
state, every event was consumed exactly once and green's stream holds
all of them, and switchover forwarded requests with no fallback and no
green error. The summary lists requests and failures per step.

The gateways run `gwag up` from the repo's Dockerfile. The replay
flags (`--replay-verify-*`) were added for this example; everything
else is the documented flags.
