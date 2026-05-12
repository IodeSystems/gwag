# go-api-gateway: roadmap & decisions

## How to use this file

Source of truth for in-flight work, priority order, and the decisions log. Read at session start.

**What "make progress on plan.md" means:**
1. If Tier 1 has anything, work the top item.
2. Otherwise pick the top open todo in Tier 2 (priority order, top → bottom).
3. Read enough Done lines to understand current state.
4. Do the work.
5. Check the box (`- [ ]` → `- [x]`) and add a one-line Done entry. Verbose history goes in commit messages, not here — keep this file scannable.
6. Commit so the tree is clean. One item = one commit (or commit set if the work decomposed). Leave the working tree clean before context-switching.
7. Don't rearrange tiers without surfacing the decision in chat first.

**Item shape:** **The push** (one paragraph: why and where it sits) → **Done** (one-line entries with commit hashes) → **Todo** (commit-sized chunks with rough effort) → **Followups** (mid-flight discoveries that don't block). Once every Todo is checked, drop the entry — git history is the record.

**Tier meaning:** Tier 1 = production-blocking. Tier 2 = design-completing, ordered by priority. Tier 2.5 = roadmap (real but not active; revisit on demand). Tier 3 = polish. Known limitations = called out intentionally; not planned to fix.

## Product priorities (settled)

Phase 1 ships best-of: **utility, availability, ox/dx**. Performance and stability are something we move *toward*, not gate v1 on. A panacea-that-is-slow beats a fast tool that's hard to set up. Perf paths (codegen, plugins, etc.) layer on as opt-ins; the default code path stays reflection-based and always-works.

Architectural test: every decision should keep the default path working for any reasonable input, even if slower. Platform / toolchain / build-step constraints are fine as opt-ins, fatal as defaults.

---

## Tier 1 — production-blocking

**Active framing: public-release prep.** "Tighten the surprise questions, hit MVP purpose completeness." Tier 1 right now is the gap between what a prospective adopter expects to find on first read and what they actually find. Perf wins / new features stay deferred until release ships.

### Pub/Sub as a first-class gateway primitive: `ps.pub` / `ps.sub`

**The push.** We are an API gateway. Pub/sub is a *primitive* the gateway provides, not a transform applied to every server-streaming RPC declaration. Earlier drafts of this entry tried to dichotomize the existing implicit-channel transform into `subscribe` vs `stream` with shape detection across three formats — wrong shape; complicates every ingest path; leaves the schema lie ("rpc Foo(...) returns (stream Resp)" but no stream ever opens) only partially resolved. Direct fix: two new GraphQL fields, both gateway-defined; service-declared `stream Resp` methods stay as honest per-subscriber gRPC streams. Multiplexing isn't auto-magic'd from method shape — services that want it call `ps.pub` from their handlers and clients `ps.sub` from theirs, both explicit. Memory: `project_subscriptions_framing.md` — the canonical multi-listener / single-producer pattern is preserved, just made explicit instead of implicit. Pre-1.0 because the wire-format and decisions-log shifts are exactly the kind of break pre-1.0 is for.

**Schema.** New gateway-internal proto `gwag.ps.v1.PubSub` at `gw/proto/ps/v1/ps.proto` — the proto exists for IR / SDL / MCP / admin-listing parity with every other ingest path; *runtime* is in-process, no proxy service.

```proto
syntax = "proto3";
package gwag.ps.v1;

service PubSub {
  rpc Pub(PubRequest) returns (PubResponse);
  rpc Sub(SubRequest) returns (stream Event);
}
message PubRequest  { string channel = 1; string payload = 2; string hmac = 3; int64 ts = 4; }
message PubResponse {}
message SubRequest  { string channel = 1; string hmac = 2; int64 ts = 3; }
message Event       { string channel = 1; string payload = 2; string payload_type = 3; int64 ts = 4; }
```

Renders as `Mutation.ps.pub(channel, payload, hmac, ts)` and `Subscription.ps.sub(channel, hmac, ts) → ps.Event` via the existing nested-namespace convention (same shape `admin.listPeers` uses today). Payload is `string` — JSON, base64, agreed-encoding-of-the-day; broker doesn't care. Typed payload bindings are a deferred extension.

**Runtime: in-process dispatcher kind.** `slot.proto` today assumes replicas point at upstream gRPC servers (`pool.replicas` → `grpc.ClientConn`). For `ps`, dispatch resolves to a Go function in-gateway — no replica, no `Invoke`. Add an "internal proto" mode to `slot.proto` (or a new slot kind `slotKindInternalProto`) that points at a registered `map[methodName]func(ctx, req) (resp, error)`. IR baking + schema rebuild + admin/services listing treat it as a normal slot; the dispatch branch reads the handler map and calls it. Generally useful primitive — future migration of admin operations from huma/OpenAPI to proto could ride the same machinery if it ever feels worth it.

**Auth tiers** (mirror admin-auth's posture). Operator configures per channel pattern via `WithChannelAuth(pattern, tier)`. Patterns use NATS-style wildcards (`.` segments + `*` segment + `>` rest-match) so auth patterns and channel subjects share grammar — operators don't learn two languages.

- `open` — no auth; the `hmac` / `ts` fields are ignored.
- `hmac` — HMAC token over `(channel, ts)`, signed by a key the operator controls; verify is hot-path crypto-fast (same primitive as today's `verifySubscribe`).
- `delegate+hmac` — HMAC verified first; then a delegate authorizer (registered under `_pubsub_auth/v1`, same shape as `_admin_auth/v1`) gets the final say. UNAVAILABLE / NOT_CONFIGURED / transport falls through to HMAC-only; only explicit DENIED short-circuits.

Default tier when no pattern matches: `hmac`. Patterns match in declaration order, first hit wins.

**Wildcard subscriptions.** `Sub` requests can use NATS wildcards (`events.orders.>`). Two semantics decisions for the wildcard case:

1. **HMAC binds to the literal pattern string the client requests** — not a concrete channel. A token issued for `events.orders.42.update` does *not* satisfy a sub on `events.orders.>`; the operator who hands out tokens controls the pattern surface. Otherwise a token signed for one literal could replay against any matching one.
2. **Strictest-tier-wins** when a wildcard `Sub` spans multiple `WithChannelAuth` patterns at different tiers. The auth applied is the max over all reachable patterns. First-match-wins would leak events from private channels through wildcards subscribed at permissive patterns — the only safe rule.

**Channel → payload-type registry.** Typemap from channel pattern to proto message type — first-class IR machinery, not an SDL concept. Two registration paths into the same registry, mirroring the proto-canonical / runtime-escape-hatch symmetry the rest of the codebase has:

- **Proto-declarative (canonical).** Custom message option in the service's .proto:
    ```proto
    message OrderUpdateEvent {
      option (gwag.ps.binding) = { pattern: "events.orders.*.update"; };
      // ... fields
    }
    ```
    Definition lives in `gw/proto/ps/v1/options.proto`. Extracted at `bakeSlotIRLocked` from `FileDescriptor.MessageOptions` and stashed on `slot.ir.ChannelBindings []ChannelBinding{Pattern, MessageFQN}`. Cluster propagation rides `proto_source` for free — bindings are part of the proto's source bytes; the existing KV bucket reconciler watches them alongside the rest of the registration. No separate KV bucket.
- **Runtime API (escape hatch).** `WithChannelBinding(pattern, proto.Message)` for non-proto adopters or for gateway-shipped defaults (the gateway's own `gwag.ps.v1.*` namespace uses this). Populates the gateway-internal slot's `ChannelBindings` — same IR table, different source. Runs through the same tier/uniqueness policy as the declarative path.

Schema rebuild walks all slots once and aggregates into a gateway-wide `pattern → MessageFQN` lookup — same pass that aggregates proto/openapi/graphql IR today. Pattern matches at delivery use the aggregated table; first-hit-wins by declaration order. On `Sub` event delivery the gateway stamps `Event.payload_type` with the matched FQN; subscribers cross-reference via the existing `/schema/proto?service=...` surface for descriptor / codegen.

**Tier policy and version skew.** Bindings inherit their slot's tier. `unstable` slots' bindings overwrite on rebake (single mutable slot, identical to today's schema rebaking posture); `vN` slots' bindings lock by `(pattern, payload_fqn)` — differing pair on the same `vN` → `AlreadyExists`, mirroring `registerSlotLocked`'s existing schema-hash collision rule. Versioned messaging gets the existing version-tier story for free. Forward compat across binding versions is proto's wire-format job; subscribers refetch descriptors any time to match against the on-delivery `payload_type`.

**Pattern uniqueness across slots.** Two different `(namespace, version)` slots can't both claim `events.orders.*.update`. Conflict is hard-rejected at slot registration pre-1.0 — declaration-order shadowing sets up subtle delivery bugs where a binding silently shifts under wildcard churn. Same-slot rebake on `unstable` swaps the binding; on `vN` it's `AlreadyExists` by the rule above.

**Contract confirmation.** Defaults to documentation only — gateway stamps `Event.payload_type`, subscribers cross-check via fetched descriptor. Two opt-in strictness axes layer on, independently:

- `WithChannelBindingEnforce()` — **shape strictness.** Mirrors `WithCallerIDEnforce` / `WithQuotaEnforce`. Gateway parses payload as the bound proto message at Pub entry, rejects type mismatch with `InvalidArgument`.
- `WithStrictPayloadTypes()` — **coverage strictness.** Reject `Sub` deliveries from channels matching no `WithChannelBinding` pattern (where `payload_type` would otherwise be blank). Default: deliver permissively with empty `payload_type` so the `open` tier stays usable for ad-hoc / dev channels.

Both can be enabled together. Memory `feedback_priorities.md`: utility first, strictness as opt-in upgrade.

Admin endpoint enumerates the registry (`channel_pattern → payload_type → namespace/version → tier` rows, dogfooded via huma) so client tooling has a listing before connecting. Synthesized typed Subscription fields (`ps.subOrderUpdate` returning a typed payload directly) remain a deferred ergonomic on top of this foundation; the registry itself is the v1 must-have because without it a JSON payload is unparseable without out-of-band context.

**Server-streaming RPCs at egress.** Stop filtering with a warning. Open the upstream gRPC stream per subscriber on Subscription field resolution; forward as `graphql-transport-ws` next-frames. Reuses existing `streamSem` backpressure. No multiplexing magic; one upstream call per subscriber. This is what the proto declaration always promised.

**Todo.**

- [x] **`gw/proto/ps/v1/ps.proto` + generated bindings.** `package gwag.ps.v1` with `service PubSub { Pub; Sub (stream Event) }` plus the four messages from the plan body, generated bindings at `gw/proto/ps/v1/ps.pb.go` + `ps_grpc.pb.go` via `protoc -I . --go_out=. --go-grpc_out=. --go_opt=module=github.com/iodesystems/gwag --go-grpc_opt=module=github.com/iodesystems/gwag gw/proto/ps/v1/ps.proto`. `go vet ./...` clean. No callers wired yet — that's Todo #2.
- [x] **Internal-proto dispatcher kind.** `slotKindInternalProto` + `internalProtoSource` (gw/slot.go, gw/internal_proto.go) install in-process proto services via `addInternalProtoSlotLocked(ns, ver, fd, rawSource, handlers)`. `internalProtoDispatcher` (gw/internal_proto_dispatcher.go) wraps `InternalProtoHandler` with the user runtime middleware chain; normalises concrete/dynamic responses to `*dynamicpb.Message` so the canonical-args + gRPC ingress paths stay uniform. Cross-cut: `registerProtoDispatchersLocked` + `rebuildGRPCIngressLocked` + `rebuildIngressLocked` + `collectIRRawLocked` + `newProtoIRTypeBuilder` + `ListServices` all handle the new kind (replicaCount=1 for admin listing). Subscription (server-streaming) registration skipped — the future broker commit installs Subscription resolvers separately. Tests: `internal_proto_test.go` — register PubSub proto + stub handlers, Dispatch through registry, assert SDL surfaces both fields.
- [x] **`PubSub` handler.** `installPubSubSlot` (gw/pubsub.go) auto-installs gwag.ps.v1 as an internal-proto slot when WithCluster is set; `psPub` marshals an `Event` and publishes via `cluster.Conn.Publish(channel, ...)`; `psSub` joins the existing `subBroker` so multiple subscribers on the same channel share one NATS subscription. Wildcards rejected on publish, accepted on subscribe. New `internalProtoSubscriptionDispatcher` + `InternalProtoSubscriptionHandler` carry the streaming surface (slot's `subscriptionHandlers` map; `registerProtoDispatchersLocked` wires it). Skips install when no cluster — keeps the schema honest about deployment capability. New `registerSlotLockedSkipTierCheck` lets gateway-bundled slots bypass `--allow-tier`. Tests: `TestPubSub_RoundTrip` (sub then pub, verify Event), `TestPubSub_RejectsWildcardOnPublish`, `TestPubSub_NoClusterSkipsInstall`. Auth tier (Todo #4) deferred to next commit; hmac/ts fields accepted but not yet verified.
- [x] **`WithChannelAuth(pattern, tier)` option + verifier.** `ChannelAuthTier` (Open / HMAC / Delegate, ordered for strictest-wins) + per-rule list on config; resolver does first-hit-wins for literal Pub channels and strictest-wins across intersecting rules for wildcard Sub patterns (with implicit-default HMAC folded in when no single rule covers). NATS wildcard grammar lives in `subjectMatchesPattern` / `patternsIntersect` / `patternCovers` (gw/auth_channel.go). Verifier reuses `cfg.subAuth.Secret` + `defaultSkewWindow` via `computeSubscribeHMAC` so operators have a single HMAC config surface; HMAC binds to the channel string as requested (concrete for Pub, the wildcard pattern itself for Sub) so a token for one literal can't replay against a covering wildcard sub. Delegate tier accepted in the option API but runs the HMAC path at runtime — fall-through wiring lands in the next commit. Tests: `auth_channel_test.go` covers pattern primitives, Pub first-hit-wins, wildcard-Sub strictest-wins, end-to-end HMAC reject/accept, and the pattern-bind security property.
- [x] **`_pubsub_auth/v1` delegate proto + AdminAuthorizer-style fall-through.** `gw/proto/pubsubauth/v1/pubsubauth.proto` (`service PubSubAuthorizer { rpc Authorize }`, request carries channel/hmac/ts/wildcard/remote_addr, response code mirrors AdminAuthCode). `consultPubSubDelegate` in `gw/auth_channel_delegate.go` looks up `_pubsub_auth/v1` via `lookupPool`, picks a replica, calls Authorize with a 3s timeout. `checkChannelAuth` now takes ctx; for `ChannelAuthDelegate`-tier channels it runs HMAC first (always-works floor) then consults the delegate. OK accepts, DENIED short-circuits; UNAVAILABLE / NOT_CONFIGURED / UNSPECIFIED / transport fall through (HMAC already passed). Tests in `gw/auth_channel_delegate_test.go` cover all code paths plus the HMAC-fails-before-delegate ordering and the HMAC-tier-skips-delegate negative case.
- [x] **`gwag.ps.binding` proto option + `slot.ir.ChannelBindings` extraction.** Custom message option in `gw/proto/ps/v1/options.proto` (field number 51234 in the internal-use range) carrying a `ChannelBinding{pattern}` payload; bindings ride in the proto's source bytes through both the boot-time `AddProto` path and the control-plane `proto_source` reconciler with no side channel. `ir.ChannelBinding{Pattern, MessageFQN}` + `Service.ChannelBindings []ChannelBinding` (gw/ir/ir.go) is the format-neutral resting place; `extractChannelBindings` (gw/channel_bindings.go) walks the FileDescriptor's messages (top-level + nested) at bake and stamps every Service in the slot. Extraction uses `protoreflect.Range` over MessageOptions matching `gwag.ps.v1.binding` by full-name + reading `pattern` by field-name, so it works regardless of whether the option comes back as `*dynamicpb.Message` (protocompile-resolved imports) or the generated concrete `*psv1.ChannelBinding` (Go-registered extension) — `proto.GetExtension` would panic on the dynamicpb case. `bakeSlotIRLocked` (gw/slot.go) invokes extraction for both `slotKindProto` and `slotKindInternalProto`. Tests: `channel_bindings_test.go` covers top-level + nested message extraction, the no-options-import zero case, and the end-to-end bake into `slot.ir`.
- [x] **Tier policy + cross-slot pattern uniqueness in `registerSlotLocked`.** `slot.channelBindings` (gw/slot.go) is the canonical per-slot list, set at register-time from `extractChannelBindings(fd)` (proto / internal-proto callers) and re-used by `bakeSlotIRLocked` to stamp every IR Service. New `checkCrossSlotBindingsLocked` runs inside `registerSlotLockedSkipTierCheck` — once on fresh-insert and once before `evictSlotLocked` on the unstable-swap branch (so a rejected swap leaves the prior occupant intact). Same-slot vN rebake with differing bindings is already rejected by the existing schema-hash collision rule (proto-source bytes carry the binding option). Tests in `channel_bindings_test.go`: cross-slot conflict rejected with prior-claimant attribution, distinct-pattern coexistence, release frees the claim, unstable self-swap is not a self-conflict.
- [x] **Schema-rebuild aggregation + admin enumeration.** `rebuildChannelBindingIndexLocked` (gw/channel_bindings.go) aggregates across all slots; `channelBindingIndexSnapshot` + `GET /admin/bindings` huma endpoint (gw/admin_huma.go) exposes the registry. Tests in `channel_bindings_test.go`.
- [x] **`WithChannelBinding(pattern, messageFQN string)` runtime API.** `WithChannelBinding` option (gw/channel_bindings.go) appends to `cfg.channelBindings`; `applyRuntimeBindingsLocked` merges into the ps slot during `New()` with cross-slot uniqueness check + IR bake + schema assemble. Tests: `TestWithChannelBinding_Basic`, `TestWithChannelBinding_CrossSlotConflict`, `TestWithChannelBinding_NoPSSlot`.
- [x] **Opt-in strictness knobs (`WithChannelBindingEnforce` + `WithStrictPayloadTypes`).** `config.psBindingEnforce` / `config.psStrictPayloadTypes` bools. `WithChannelBindingEnforce()` enables shape strictness: `psPub` resolves the binding's `messageDesc` from the index and validates payload via `protojson.Unmarshal` against the bound descriptor, rejecting with `CodeInvalidArgument` on mismatch. No-op when binding has no resolved descriptor (runtime bindings with unresolvable FQN). `WithStrictPayloadTypes()` enables coverage strictness: rejects `psPub` on channels with no matching binding. `channelBindingEntry` gains `messageDesc protoreflect.MessageDescriptor`; `rebuildChannelBindingIndexLocked` resolves descriptors from all proto/internal-proto slots via `resolveMessageDescriptorsLocked` + `walkMessagesForDescriptors`. `channelBindingIndex` gains `lookupBinding` and `validatePayload` methods. Tests: `TestPubSub_StrictPayloadTypes_RejectsUnboundChannel`, `TestPubSub_ChannelBindingEnforce_ValidPayload`, `TestPubSub_ChannelBindingEnforce_InvalidPayload`, `TestPubSub_ChannelBindingEnforce_NoDescriptor`, `TestPubSub_BothStrictnessKnobs`.
- [x] **Honest server-streaming at egress.** `protoDirectSubscriptionDispatcher` (gw/proto_direct_subscription_dispatcher.go) opens a direct gRPC server-streaming call to an upstream replica per subscriber, forwarding each frame as a `map[string]any` event through a channel that graphql-go pumps as WebSocket next-frames. Stream slots (gateway-wide + per-pool `streamSem`) acquired with the same backpressure semantics as the prior NATS path. Replaces `protoSubscriptionDispatcher` in `registerProtoDispatchersLocked` for `slotKindProto`. `admin_events` migrated from `slotKindProto` with `noopAdminEventsConn` to `slotKindInternalProto` with an in-process `adminEventsWatchServicesHandler` that joins the NATS broker — preserves the gateway-internal publish/subscribe pattern without requiring a gRPC upstream. Tests: `TestSubscriptionE2E_HappyPath`, `TestSubscriptionE2E_MultipleFrames`, `TestSubscriptionE2E_UpstreamError`, `TestSubscriptionE2E_ClientCompleteCleansUp`, `TestGRPCIngress_ServerStreaming_HappyPath`, `TestGRPCIngress_ServerStreaming_CustomUpstream`, `TestHTTPIngress_SSESubscription_HappyPath`, `TestHTTPIngress_SSESubscription_MultipleFrames`.
- [ ] **Remove old implicit-channel transform.** Delete `subjectFor`-driven path in `subscriptions.go` + auto-injection of HMAC/ts into method-derived Subscription fields in `convert.go`. ~0.5d.
- [ ] **`docs/pubsub.md` + README pointer + decisions-log amendment.** Dedicated adopter-facing doc — sits alongside `docs/perf.md`. Covers: `ps.pub` / `ps.sub` GraphQL surface + the `gwag.ps.v1.PubSub` proto behind it; the three auth tiers + `WithChannelAuth` configuration; channel→type binding registry (both proto-declarative *and* runtime); version-tier + pattern-uniqueness policy; opt-in `WithChannelBindingEnforce()` mode; migration walkthrough from the v0 implicit-channel pattern. Includes a "Why custom options for channel bindings?" design-note section — the proto-option-tunneling approach is non-standard enough to warrant explaining (vs alternatives: separate manifest, runtime-only, a dedicated `service ChannelBindings { ... }` shape). README gets a one-paragraph "Pub/Sub" section pointing here. Decisions log replaces the two affected rows with the new posture. ~1.5d.

**Commit grouping.**

| # | Commits | Covers Todos | Why |
|---|---|---|---|
| 1 | `ps.proto` + generated bindings + internal-proto dispatcher kind | 1, 2 | Scaffolding; no observable dispatch behavior change. Generated code reviewable in isolation. |
| 2 | `PubSub` handler + `WithChannelAuth` + `_pubsub_auth/v1` delegate | 3, 4, 5 | Broker primitive with auth end-to-end. Tests cover all three auth tiers + broker integration + delegate fall-through. No bindings yet — `payload_type` ships blank. |
| 3 | Channel-binding registry: `gwag.ps.binding` option + IR extraction + tier-policy/uniqueness + schema aggregation + admin endpoint + runtime API | 6, 7, 8, 9 | Typemap fully wired across both registration sources. Cluster propagation rides existing `proto_source` reconciler; pattern uniqueness + tier policy land in one commit so test coverage is consistent across them. |
| 4 | Opt-in strictness knobs (`WithChannelBindingEnforce` + `WithStrictPayloadTypes`) | 10 | Two opt-in strictness axes (shape vs coverage). Pure additive on commit 3; both default off, no behavior change unless adopter turns them on. |
| 5 | Honest server-streaming at egress | 11 | Independent of pub/sub. Could land before commit 2 if convenient; logically separable. |
| 6 | Remove old implicit-channel transform + `docs/pubsub.md` + README pointer + decisions amendment | 12, 13 | Pre-1.0 break concentrated here. Schema lie removed, adopter-facing feature doc lands alongside so the migration walkthrough exists at the moment the old path disappears. |

**Followups (mid-workstream, don't block).**

- **Synthesized typed Subscription fields.** Built on the registry: `ps.subOrderUpdate(channel: ...): OrderUpdateEvent!` per registered binding, alongside the generic `ps.sub`. Pure ergonomic on top of the v1 foundation — the registry already surfaces enough info for clients to deserialize the JSON payload manually against the fetched descriptor. Pulled on adopter ask.
- **Pub HTTP endpoint** (`POST /api/pub`) for non-GraphQL producers — a Python service, a cron, shell. Shares the backend handler; ~20 lines. Pull when an adopter asks.
- **Internal-proto dispatcher reuse.** Future migration of admin operations from huma/OpenAPI to proto, if maintainer ergonomics ever warrant it. Not on the roadmap.

### Competitor performance matrix (gwag vs graphql-mesh / Apollo Router / WunderGraph)

**The push.** "How do you compare to X?" is a top-three adopter question — `docs/perf.md` answers "how does gwag perform on my hardware?", not the comparative one. Scaffolding lives at `perf/` (root-level, separate from `bench/` which is for self-measurement only): hermetic Docker image, declarative `perf/competitors.yaml`, orchestrator at `perf/cmd/compare/main.go` running each gateway serially against shared backends to avoid CPU contention. Three peers in scope for v1: graphql-mesh (closest peer — multi-format ingest), Apollo Router (federation specialist in single-subgraph mode), and gwag itself. WunderGraph deferred (codegen-heavy, dual-process — `enabled: false` in competitors.yaml). Output: `perf/comparison.md`. Runs in parallel with the pub/sub workstream — independent code path, independent commit chain.

**Done.** Scaffolding shipped: Dockerfile, `competitors.yaml` (gateways × scenarios × sweep config), orchestrator skeleton (`perf/cmd/compare/main.go`), per-peer configs (`perf/configs/apollo/`, `perf/configs/mesh/`), start scripts, `docker-build.sh` staging the graphql-go fork into `perf/.build/graphql` so the host-absolute `replace` directive works inside Docker, and the README documenting scope + caveats. Status per peer: gwag ✅ working, graphql-mesh 🟡 scaffolded, Apollo Router 🟡 scaffolded, WunderGraph ❌ deferred.

**Todo.**

- [ ] **graphql-mesh integration debug.** Most-scenarios peer (`openapi` + `graphql`) — debugging here exercises the orchestrator the most. Common issues per `perf/README.md`: npm version pins in `configs/mesh/package.json` need adjusting against current upstream packages; `query_overrides` for field-name divergence vs gwag's namespace-prefix mirror is already encoded in `competitors.yaml` but may need iteration. ~1-2d.
- [ ] **Apollo Router integration debug.** Single-subgraph "federation" mode against `hello-graphql` only (Apollo doesn't natively consume proto / openapi). `configs/apollo/supergraph.graphql` is hand-written; `@link` / `@join__*` versions may need to track the router runtime; `APOLLO_VERSION` bumps in `scripts/start-apollo.sh` + `Dockerfile`. `waitForGateway`'s `__typename` probe may need tweaking for router's 4xx-on-probe behavior. ~1-2d.
- [ ] **End-to-end hermetic Docker validation.** First build is ~1GB / 5-10min — confirm the image boots, the orchestrator binary runs the full sweep in-container, output lands at `/out/comparison.md` via the volume mount. ~0.5d.
- [ ] **Full sweep run + `perf/comparison.md` published.** RPS rungs [1k → 60k], 3 reps, 5s duration per `competitors.yaml::sweep`. ~0.5d wall-clock (not engineering hours — most of it is the sweep itself running).
- [ ] **Cross-link from `docs/perf.md` and README.** Two sentences in each, pointing at the comparison and naming the difference between the two perf surfaces (self-measurement vs head-to-head). ~0.25d.

**Commit grouping.**

| # | Commits | Covers Todos | Why |
|---|---|---|---|
| 1 | graphql-mesh debug end-to-end | 1 | Closest peer; covers most scenarios; exercises orchestrator hardest. |
| 2 | Apollo Router debug end-to-end | 2 | Smaller debug surface; could land first if mesh stalls. |
| 3 | Docker validation + full sweep + `comparison.md` + cross-links | 3, 4, 5 | Final integration; results published; README + `docs/perf.md` point at it. |

**Followups (mid-workstream, don't block).**

- **WunderGraph row.** Codegen-heavy, dual-process integration. `enabled: false` in `competitors.yaml` today. Pull post-v1 if WunderGraph keeps appearing in adopter questions.
- **Backends beyond `hello-*`.** Current scenarios are micro-bench (single-field selection). Real-world payload shapes (deeper selections, larger bodies, error paths) are a separate workstream — touch when the published comparison surfaces a "representativeness" complaint.
- **CI cadence.** Running the full matrix on every PR is expensive (~1GB Docker build + multi-minute sweep). Nightly or weekly once results stabilize.

**Followups (parked, separate workstreams):**
- CI hook reuses the same `bench traffic --json` output for diff-vs-main; revisit when the Tier 3 perf-gate item lands.
- Subscription throughput is a separate dimension (NATS-bound, not request-bound); add a `bin/bench perf --subs` scenario if/when an adopter asks.
- Driver-managed latency rungs (`perf run --upstream-latency-rungs 0,100us,1ms` iterating restarts) — operator-driven single-value workflow works for now; pull when a maintainer wants the full curve in one shot.
- Higher-throughput bench client (the current `bench traffic` caps ~4 k RPS on this 24-core host due to ticker overhead; gateway numbers above ~3 k are client-bound, not gateway-bound). Touch when the headline number matters more than getting the report out the door.

---

## Tier 2 — design-completing (priority order)

### gw/gat — GraphQL API Translator (NATS-free embedded)

**The push.** Minimum-cost entry: a single huma app wants GraphQL over its OpenAPI / proto / GraphQL specs, same port, no separate gwag process, no NATS. `gw/gat` is `import "github.com/iodesystems/gwag/gw/gat"` — `gat.New(regs...)` returns a `*Gateway` that mounts onto the adopter's existing huma router via `gat.RegisterHuma(api, prefix, g)`. Depends only on `gw/ir`, `graphql-go`, `kin-openapi`, `protobuf`. Zero NATS, zero Prometheus, zero MCP, zero admin. ~250 deps vs. ~498 for full `gw`.

**Done.** Moved `IRTypeBuilder` + naming helpers to `gw/ir/typebuilder.go`. Moved `RenderGraphQLRuntime` to `gw/ir/render_graphql_runtime.go` (exported `ParseRuntimeVersionN`, `CombineDepReason`, `IdentityName`). `gw/` delegates to `ir.` via thin wrappers. Created `gw/gat/gat.go` (Gateway struct, New, Handler, context keys) and `gw/gat/openapi.go` (openAPIDispatcher with HTTP forwarding). `gw/ir` is now NATS-free (0 NATS deps). Renamed from `gw/lite` pre-review. Paired `gat.Register` + `gat.RegisterHuma` + `gat.RegisterGRPC` (connect-go) with in-process reflection-based dispatch landed; runnable end-to-end example at `examples/gat/`; concept page at `docs/gat.md`.

**Todo.**
- [ ] **`gat.Sign` Go-only pubsub channel signer.** HMAC channel signing exposed as a Go method on `*Gateway` (not via HTTP). Import boundary == auth boundary, so no admin-token gate. Deferred — pubsub isn't on the simple-start path.
- [ ] **Proto ingest path for gat.** `gat.WithProtoFile()` that ingests via `ir.IngestProto` and registers dispatchers. Requires the gat package to carry a simplified proto-dispatch path (no gRPC pool, just `grpc.Dial` + `Invoke` per dispatch). ~1 day.
- [ ] **`gwag serve` subcommand.** `gwag serve --openapi spec.yaml --to http://localhost:8081` — boots gat without NATS. ~0.5 day.

---

### Existing tier-2 tail (parked behind real use cases)

- [ ] **Static `--openapi` / `--graphql` registration flags for `gwag`.** Runtime control-plane registration is the only path for those kinds today; the operator who wants a CLI-driven static registration pipeline pulls on this.
- [ ] **`~/.config/gwag/context.json` global fallback for `./.gw`.** kubectl-style multi-context. Wait until someone needs more than one context.
- [ ] **`gwag --admin-data-dir` flag mirroring the example gateway's token persistence.** Today `gwag up` persists, but ad-hoc `gwag` startups don't, so `gwag sign` against a local gwag falls back to `--secret HEX`.
- [ ] **`bin/bench traffic --metrics-path` flag (or auto-detect).** `MetricsURLFromGateway` always derives `/api/metrics`; raw gateways without it warn and skip server-side capture.
- [ ] **File upload (`multipart/form-data` passthrough).** Recurring question for any GraphQL system. Surface: GraphQL `Upload` scalar (graphql-multipart-request-spec) on inbound; HTTP ingress detects multipart and decodes; outbound forwards to OpenAPI services that accept multipart, or proto `bytes` field for proto services with size cap. Scope: ~3-5 days; touches inbound parsers + canonical-args shape + a couple of dispatcher branches. Public-release answer for now: "planned, not in v1; bytes field works for small payloads via base64 today."
- [ ] **Service-account token outbound auth.** Built-in helper wrapping a RoundTripper. Composable today; first-class when wanted.
- [ ] **OAuth/JWT translation outbound auth.** Inbound token → service-specific token via configurable issuer. Composable today.
- [ ] **Destructive read opt-in.** AdminMiddleware lets every GET through; gate destructive reads via per-route flag when first one shows up.
- [ ] **UI rotate-key panel.** Token rotation done; panel ships when an operator asks.
- [ ] **Interface / Union typed-mirror polish + richer oneOf/anyOf mapping.** Base cases shipped; richer projections wait for use case.

---

## Tier 2.5 — roadmap (not yet active)

Real workstreams, not parked. Opt-in performance paths that layer on top of the canonical reflection dispatch.

**Perf direction (settled).** The graphql-go fork's `ExecutePlan` is the dominant per-request cost: ~245 µs / ~800 allocs of the ~430 µs / ~972 allocs end-to-end budget in `BenchmarkProtoSchemaExec`. The dispatcher itself is ~185 µs / ~174 allocs, ~110 of which live inside `grpc.ClientConn.Invoke` and are not reclaimable from the gateway side. Static dispatcher codegen reclaims ~25-30 allocs / ~10-15 µs at the dispatcher boundary — real but ~3 % end-to-end. The append-mode executor work in the fork (see `../graphql/docs/plan.md`) is the lead push; dispatcher codegen drops to a smaller follow-on with no urgency.

### Append-mode execution in the graphql-go fork

**The push.** Use the plan cache's static knowledge to skip the `map[string]any` result tree entirely: walk the plan emitting JSON bytes straight into a pooled buffer. Projected end-to-end: **~430 µs / ~972 allocs → ~120 µs / ~200 allocs** on `BenchmarkProtoSchemaExec`, a ~3-4× wedge. Work happens in the fork at `../graphql`; the gateway repo's downstream change is wiring `ExecutePlanAppend` in place of `graphql.ExecutePlan` + `json.NewEncoder.Encode` at `gw/gateway.go:1376`.

**Surface contract.** Fork exposes `ExecutePlanAppend(plan, params, dst []byte) ([]byte, []gqlerrors.FormattedError)` as a sibling of `ExecutePlan`. Phase 1 lands the executor walker (40-50 % of executor reclaim) without touching dispatcher signatures. Phase 3 ships an opt-in `ResolveAppend` resolver API; gateway-side dispatcher rewrites land in lockstep to capture the rest of the wedge.

**Todo (gateway-side; fork-side detail in `../graphql/docs/plan.md`).**
- [ ] **Phase 1 wiring.** Once fork ships `ExecutePlanAppend`, swap the `serveGraphQLJSON` path at `gw/gateway.go:1376` to use it. Single function change; `json.Encode` at the egress dies with the swap. ~0.5 day. Wins ~40-50 % of executor reclaim with no other code change.
- [ ] **Phase 3 dispatcher rewrite.** Flip `ir.Dispatcher.Dispatch(ctx, args) (any, error)` to a buffer-write variant (signature TBD with fork; likely `Dispatch(ctx, args, w *jsonw.Writer) error`). Touches every dispatcher: `protoDispatcher` (`gw/proto_dispatcher.go`), `openAPIDispatcher` (`gw/openapi.go`), downstream-GraphQL forwarder (`gw/graphql_mirror.go`), every admin huma resolver. Mechanical but large. ~2 days.
- [ ] **Proto fast emitter.** `protojson.MarshalAppend` for the response message, respecting GraphQL field renaming (today done in `messageToMap`). Hand-rolled emitter is the perf-best variant; add only if benches show `protojson` is a meaningful tail. ~1-2 days, deferred until measurement.
- [ ] **OpenAPI byte-passthrough.** When the GraphQL selection matches the upstream JSON shape 1:1 (the common case), pipe `resp.Body` bytes straight to the buffer; only stream-decode + re-emit when projection differs. ~1 day.

**Blockers.** Phase 1 wiring is the *only* item that doesn't block on fork progress; the rest follow Phase 3 of `../graphql/docs/plan.md`.

### Static codegen — RegisterCodegen surface (demoted)

**The push.** Operators who know their service set at build time can opt into native-speed dispatch with one extra `go generate`. Reclaims ~25-30 allocs / ~10-15 µs at the dispatcher boundary — **~3 % end-to-end on its own**, and that's *before* append-mode lands (after append-mode, the gateway's overall budget shrinks, so the same ~10-15 µs is a slightly larger share of a smaller pie; rerun the projection then). The codegen spike is recorded in [`docs/codegen-spike.md`](./codegen-spike.md). Pull this only after append-mode Phase 3 lands and the actual remaining gap is measured; the spike's projection was based on pre-append-mode profile data.

**Surface contract.** Codegen output is a self-contained Go package exporting `func Dispatchers(deps SDK) map[ir.SchemaID]ir.Dispatcher`. Operator imports + calls `gw.RegisterCodegen(generated.Dispatchers(...))` alongside `AddProto`. The Plugin supervisor entry below reuses this same artifact, just runtime-compiled.

**Todo (paused; revisit post-append-mode).**
- [ ] **Plugin SDK (`gw/sdk` subpackage).** Stable, minimal interfaces (`PoolDispatcher`, `OpenAPIDispatcher` etc.) the codegen consumes. Caps the API surface vs the full `gw` package; bounded by versioning. ~1 day.
- [ ] **Codegen template + driver.** `gwag codegen --schema=foo.graphql --out=./dispatchers` walks IR, emits typed dispatchers (no reflection, no dynamicpb). ~3-4 days.
- [ ] **`gw.RegisterCodegen` registration point.** Slots into the same `DispatchRegistry`; precedence: codegen entry > reflection entry. ~0.5 day.
- [ ] **Worked example in `examples/multi`.** Operator template + measured perf vs reflection. ~0.5 day.
- [ ] **Telemetry.** `/metrics` per-dispatcher mode (reflection / codegen / plugin) + per-mode latency histograms. Operators see the upgrade path without anyone telling them. ~0.5 day.

### Plugin supervisor for dynamic-static dispatch

**The push.** Operators who want both fast *and* dynamic (control-plane registrations + codegen perf) get a supervisor that runs the codegen toolchain at runtime, builds a `.so`, and rolls it through the cluster via drain-and-restart. Each gateway loads the plugin once per process lifetime — Go plugins can't unload, but the cluster's drain primitive sidesteps that (process dies, plugin dies with it). Same artifact as the Static codegen workstream above, just runtime-compiled.

**Blocked on the Static codegen workstream above** (the codegen output is the supervisor's input).

**Todo.**
- [ ] **Compile coordinator.** Leader compiles via the toolchain; publishes `.so` to JetStream object store; peers fetch + load. Compile once per cluster, eliminates version-skew structurally. ~3 days.
- [ ] **Settle window + debounce.** Bursty registrations (5-20/sec on deploys) coalesce into one schema rev before triggering rebuild. ~30s window, tunable. ~1 day.
- [ ] **Rolling drain controller.** Uses existing `Drain()` + `/health` 503; sequenced node drain → fetch → load → up; readiness gate on "≥1 successful dispatch per pool" before draining the next node. Cold-start dwell (empty pools, no HTTP keep-alives) is real — the gate exists to avoid cascading everyone into cold-start at once. ~3 days.
- [ ] **Compile-fail fallback.** `.so` load failure → keep reflection path; alert + retry. Compile/load problems must never take the gateway down. ~1 day.
- [ ] **Toolchain placement decision.** Sidecar / init container vs in-image; security tradeoff (gateway image gains the ability to run `go build` on IR-derived source). Document in plan + README. ~0.5 day.

---

## Tier 3 — operational polish

- [ ] **Wire-level identifier rename to `gwag-*`.** JetStream bucket names (`go-api-gateway-{registry,peers,stable,deprecated,mcp-config}` → `gwag-*`), default NATS cluster name (`go-api-gateway` → `gwag`), MCP server-info string, and UI `localStorage` keys (`go-api-gateway:admin-token{,-changed}`). Data-migration adjacent: existing dev installs need fresh data dirs and UI sessions lose their saved token. No real pinning pre-1.0 — pull when we cut a release branch or someone asks.
- [ ] Connection-rate limiting / per-IP caps on WebSocket terminator.
- [ ] k8s + docker-compose example deployments for `examples/multi`.
- [x] **NATS server log noise control.** `ClusterOptions.Logger` overrides the embedded NATS server's logger via `srv.SetLogger(Logger, Debug, Trace)` for routing through a structured logger or a no-op sink. `ClusterOptions.LogLevel` is the convenience knob: `"silent"` installs `silentNATSLogger` (drops every level), `"warn"` installs `warnNATSLogger` (Notice/Debug/Trace dropped, Warn/Error/Fatal forwarded to stderr), `"debug"`/`"trace"` toggle the matching flags on top of `ConfigureLogger`, anything else (including `""` and `"info"`) is the existing `srv.ConfigureLogger()` default. `applyClusterLogger` (gw/cluster.go) is the single decision site. Tests: `TestClusterOptions_LoggerCustomReceives` (custom Logger receives Notice on startup), `TestClusterOptions_LogLevelSilent` (silent path boots cleanly).
- [ ] Metrics / tracing example middleware.
- [ ] `Cluster.Close` vs `Gateway.Close` lifecycle docs.
- [ ] Heartbeat-to-wrong-gateway smoothing (registry KV check before forcing re-register).
- [ ] Sub-fanout drop policy configurable (per-consumer watermark + behaviour knob).
- [ ] **`pickReplica` per-instance bias.** Today picks least-loaded across all replicas; doesn't bias toward replicas with free per-instance slots. The per-instance sem still bounds the result but adds wait dwell when a saturated replica is picked. Revisit if dwell metrics show pathological selection.
- [x] **Per-replica queue-depth gauge label.** Added `BackpressureConfig.Replica` (gw/backpressure.go) plus a dedicated `Metrics.SetReplicaQueueDepth(ns, ver, kind, replica string, depth int)` method exporting `go_api_gateway_replica_queue_depth{namespace, version, kind, replica}` (separate from the existing per-pool `pool_queue_depth`). `setQueueDepthForCfg` routes per-replica configs through the new gauge; service-level configs keep firing `SetQueueDepth`. Wired from both `acquireReplicaSlot` (proto: `r.addr`) and `acquireOpenAPIReplicaSlot` (openapi: `r.baseURL`). Embedding `noopMetrics` in test fixtures meant zero existing-stub churn. New `TestBackpressureMiddleware_ReplicaSplitsQueueDepth` pins the routing.
- [ ] **Per-method drilldown on the public status page.** Click a service row → drawer with per-method dot-strips. `serviceStats` already emits per-method windowed aggregates; extend to per-method history.
- [ ] **Backlog / queue-depth surfacing on the public status page.** Pool unary-queue + per-replica inflight are already tracked as Prometheus gauges (`go_api_gateway_pool_queue_depth`, etc.). If the dot-strip alone doesn't catch saturation events, add a JSON sidecar endpoint and an "is anyone backed up?" badge.
- [ ] **CI perf gate.** Run `bench traffic graphql` at a fixed RPS for ~30s on every PR; fail the gate if RPS / p95 / per-ingress `request_self_seconds` mean regresses past a tolerance band vs. main. Stack: GitHub Actions runner with cached `bench/.run/bin`, the recipe in [`docs/perf-testing.md`](./perf-testing.md), and the bench traffic JSON output (which doesn't exist yet — add `--json` to the runner's summary). Open question: what's the tolerance band that absorbs CI-host noise without losing the signal — pre-fork measurements showed runner variance up to ±10 % between identical configurations. ~1-2 days for the workflow + `--json` output; the band picks itself once we have ten clean runs to fingerprint baseline noise.
- [x] **Per-request structured log option.** `WithRequestLog(io.Writer)` (gw/gateway.go) wires an opt-in JSON-per-request sink; `logRequestLine` (gw/request_log.go) emits `{ts, ingress, path, total_us, self_us, dispatch_count}` from all three ingress paths (graphql / http / grpc). Dispatch accumulator extended from `*atomic.Int64` to a `dispatchAccumulator{Sum, Count}` struct so `addDispatchTime` bumps a per-request counter alongside the time sum without a parallel atomic. Writes are serialised with a package-level mutex (single-line writes; contention only matters when the operator points the option at a slow sink). Tests: `TestRequestLog_GraphQLEmitsOnePerRequest` (end-to-end through openapi backend, asserts shape + count), `TestRequestLog_NotAutoEnabled` (default config writes nothing), plus the existing dispatch-accumulator tests updated for the new struct shape.
- [ ] **gRPC client conn pool above one per address.** When per-replica in-flight pushes past HTTP/2's 100-stream default, round-robin across N conns. Revisit when `request_self_seconds` shows replica-side wait dominating (today's profile: not the bottleneck). ~1 day.
- [x] **Loud surface for slot-policy mismatches on `vN` joins.** Fix (1) was already in place — `removeReplica*Locked` for proto / openapi / graphql all call `releaseSlotLocked` when the last replica drops, so a heartbeat-out cycle naturally clears the slot for fresh caps. Added a regression test (`TestRegisterSlot_RejectedJoinsClearedOnSlotRelease`) pinning the behaviour. Fix (2): `registerSlotLocked` now records every vN rejection on `g.rejectedJoins[poolKey]` (Count / LastReason / LastUnixMs / Last & Current caps), `releaseSlotLocked` clears the entry alongside the slot, and `/admin/services` surfaces the summary as `serviceInfo.rejectedJoins` (Count / LastReason / Last vs Current caps) — so an operator can see "this slot rejected N joins with caps X, currently running Y" before profiling. Snapshot is taken under `g.mu` then merged into the huma response without changing the ListServices proto wire shape. Test extended in `slot_test.go::TestRegisterSlot_VN_DiffCapsRejected` for the counter shape.

---

## Tier 3.0 — leave-the-door-open

Things we're aware of, not actively planning, but whose shape should constrain today's design so we don't paint ourselves into a corner. Entries here capture **what to preserve** more than **what to build**. Promote to Tier 2 only when a concrete adopter pulls.

### WSDL / SOAP ingest — fourth kind for corporate adopters

**The push.** Corporate adopters with legacy SOAP services they can't (or won't) rewrite are a real wedge. WSDL would land as a fourth ingest kind alongside proto / OpenAPI / GraphQL — architecturally same-shape: `ServiceBinding.wsdl_source` raw bytes on the wire, gateway parses on receive (symmetric with `openapi_spec` / `proto_source`); `gw/ir/ingest_wsdl.go` walks PortType → IR Operations and XSD → IR Types; `gw/wsdl_register.go` builds SOAP envelopes and POSTs. Not on the active roadmap — WSDL's the spec, SOAP's the wire, and most cost lives in the latter. Pulled in by a concrete pilot, not speculation.

**Scope sketch (if pulled).**
- WSDL 1.1, SOAP 1.1 + 1.2, document/literal-wrapped only.
- XSD subset: complexType (sequence), simpleType restrictions, enumerations, repeated elements, optional attributes-as-fields, nillable.
- BasicAuth / bearer outbound via `ForwardHeaders` + an `OutboundClient`-style hook.
- Faults → GraphQL errors with `extensions.soapFault`.
- Reject (don't half-implement): rpc/encoded, MTOM/SwA, WS-Security, `<any>` / substitution groups.
- Subscriptions unsupported — SOAP has no streaming primitive.
- ~1–2 weeks for a v1 covering the 80% case; long open tail.

**Doors to leave open (constraints on today's work — flag PRs that violate these).**
- **`ServiceBinding` proto stays oneof-shaped.** `proto_source` / `openapi_spec` set the precedent: every kind ships raw source as bytes. A future commit adding a fourth kind that ships *compiled* artefacts violates the principle (decisions log entry "Both proto and OpenAPI ingest ship raw source over the wire" should generalise to *every* ingest kind).
- **`slot.kind` and IR `Kind` (`gw/ir/ir.go`) stay expandable.** Reconcilers (`gw/reconciler.go`), schema rebuild (`gw/schema.go`), `slot.go`'s tier-policy decisions, and per-kind helpers (`opNameForRuntime`, `mcpOpName`) already dispatch on kind — adding a fourth case stays pattern-match work. Don't refactor those into a binary "proto-or-not" assumption.
- **Subscription path stays orthogonal.** WSDL having only Query / Mutation is precedent — a fourth ingest kind that legitimately doesn't populate Subscription must work without warnings. Schema rebuild already tolerates this; keep it that way.
- **MCP path naming uses GraphQL-rendered names, not IR names.** WSDL would land as identity (SOAP op names are typically already lowerCamel-ish), but the per-kind `mcpOpName` rename established by the proto fix is the abstraction point. New kinds slot into the same function, not parallel paths.
- **Outbound auth helper generalisation.** `WithOpenAPIClient` / `OpenAPIClient(c)` is per-kind today. WSDL would want the same shape (custom `*http.Client` for cert pinning, proxy, etc.). When the next consumer lands, generalise to `OutboundClient(kind)` rather than duplicating.

**Followups (awareness, not action).**
- Go SOAP libraries are mostly codegen-side (`hooklift/gowsdl`); we'd likely write a thin parser ourselves. Likely 60% of v1 effort.
- WS-Addressing as `ForwardHeaders` analogue if a pilot needs it. WS-Security: hard pass — operator runs a proxy that strips/applies before us, same answer federation gets.
- Mock SOAP backend in `bench/` for an end-to-end story before the real pilot lands.

---

## Known limitations (won't fix unless driven by use case)

- **No Apollo Federation** *(recurring adopter question — answer below)*. Stitching covers the common case; federation's entity-merging is overkill for most teams. The public-release framing is "use stitching first; if you need entity-merging across services that already share entity identity (e.g., `User` in two services with cross-references), federation's the answer and we don't ship it." Reconsider only if a concrete operator pulls on the gap.
- **No AsyncAPI export.** GraphQL SDL with Subscription types covers TS codegen; AsyncAPI's TS tooling is patchier with little payoff.
- **One Register call = one address contributing to N pools, not N independent bindings.** Bindings share lifetime; heartbeat + deregister are atomic across all of them. Run two binaries (or two control-plane connections) for independent lifecycles.

---

## Decisions log

Settled. Reading these prevents re-litigating in future sessions.

| Decision | Rationale |
|---|---|
| **Reflection is the default dispatch path forever** | Always works; no platform/toolchain constraints; lowest setup friction. Codegen + plugin are opt-ins. (See Product priorities.) |
| **Proto/gRPC is canonical service-to-service** | GraphQL client codegen is excellent in TS/JS, fair in Go, weak elsewhere. `.proto` is the multilingual contract; SDL is *derived*. OpenAPI + downstream-GraphQL ingest are bridges. |
| **Per-pool backpressure, not gateway-wide unary cap** | Slow service X shouldn't gate dispatches to service Y. Pool is the isolation primitive. |
| **Hybrid stream caps** (per-pool + gateway-wide) | Per-pool throttles fine-grained; gateway-wide caps the actual scarce resource (FDs, RAM). Defaults: 10k per pool, 100k total. |
| **Subscriptions = NATS pub/sub, not gRPC streams** | NATS handles fan-out natively. gRPC streams require long-lived per-client gateway-to-service connections; doesn't compose at scale. |
| **HMAC verify on subscribe; sign is an exposed authenticated endpoint** | Verify is hot-path crypto-fast. Sign is the privileged path; the gateway publishes `SignSubscriptionToken` and downstream services authenticate to it (admin/signer secret, mTLS, or operator-supplied middleware) and apply their own business authz before calling. Inverted from the earlier pull-delegate model — the caller already has full request context, so composition beats trying to predict what the authorizer needs. |
| **Stitching for downstream GraphQL, not federation** | Federation solves entity-merging that most teams don't have. |
| **Proto stays canonical for events** | One source of truth; AsyncAPI would be a derived view, dropped. |
| **Tier model (`unstable` / `stable` / `vN`) is the versioning primitive; drop `--environment`** | One axis instead of two. `unstable` is mutable (single overwrite slot); `vN` is locked once registered (differing schema-hash → `AlreadyExists`); `stable` is a computed alias to the highest-ever `vN` and is monotonic — it only advances; rollback never silently moves it backward (operator-driven `RetractStable` admin RPC). Per-tier policy (`--allow-tier`) replaces env's "dev allows unstable, prod doesn't" job. NATS cluster-name auto-suffix retired — operators pick cluster names directly. The forcing function (`A.deps.stable` diverging from `A.deps.unstable` blocks A's release) makes "cut a version" a real organizational signal. |
| **Schema diff via SDL, hash parity via canonical descriptors** | Two views of compatibility: semantic + structural. |
| **Server-streaming gRPC filtered with warning, not implemented at egress** | Subscription path is NATS-backed; lifting actual gRPC streams adds a transport story we'd rather not maintain. |
| **`AdminMiddleware` gates writes, lets reads through** | UI's services/peers views must work unauthenticated for the operator to find the token in the first place. The unauth public status page leans on the same posture. |
| **OpenAPI dispatch forwards `Authorization` by default; `ForwardHeaders` overrides** | Default makes admin\_\* end-to-end work with one bearer. |
| **AdminAuthorizer fall-through: delegate → boot token** | Boot token is the always-works emergency hatch. UNAVAILABLE / transport / NOT_CONFIGURED falls through; only explicit DENIED short-circuits. |
| **GraphQL renders nested; proto / OpenAPI flatten via `FlatOperations`** | IR carries the structure; each format honors it as far as the format permits. |
| **Both proto and OpenAPI ingest ship raw source over the wire** | Symmetric `proto_source` / `openapi_spec` byte fields. Gateway compiles on receive (proto via `protocompile` with `SourceInfoStandard`; OpenAPI via `kin-openapi`). Comments / descriptions survive into IR. The earlier "ship a compiled FileDescriptorSet" path was an asymmetric one-off — retired pre-1.0. Memory: `feedback_api_symmetry.md`. |
| **Append-mode executor in the fork is the primary perf lever; codegen is the smaller follow-on** | Per-request profile shows ~245 µs / ~800 allocs of the ~430 µs / ~972 allocs end-to-end budget lives in `ExecutePlan` (result tree build + scalar boxing + final marshal). The plan cache already carries all the static info needed to write JSON directly to a buffer — Apollo Router / gqlgen optimised path. Codegen at the dispatcher reclaims ~3 % end-to-end on its own; append-mode reclaims ~70 % from the same surface. Append-mode goes first; codegen revisits once append-mode lands and the residual gap is re-measured. Fork workstream lives at `../graphql/docs/plan.md`. |

---

## Reference

### Test fixture patterns

- **Unit-shape:** httptest backend or in-process `grpc.ClientConnInterface` fake; helper-level direct calls. (`auth_admin_test.go`, `auth_admin_delegate_test.go`)
- **HTTP-shape:** httptest backend + `gw.Handler()`; full GraphQL → upstream round-trip. (`openapi_test.go`, `graphql_ingest_test.go`)
- **gRPC-shape:** in-process `grpc.Server` on `127.0.0.1:0` + `AddProtoDescriptor`. (`grpc_dispatch_test.go`)
- **Cluster-shape:** `StartCluster` + ephemeral ports + tempdir. **Lifetime gotcha:** pass `context.Background()` (not `WithTimeout`) as the parent for watch + reconciler goroutines, otherwise they die mid-test. Symptom: registry KV has the key on both nodes but B's reconciler never creates the pool. (`cluster_dispatch_test.go`)

### HTTP routing surface

`/api/*` is the gateway, everything else is the embedded UI bundle. Unmatched `/api/*` returns JSON 404; non-API requests fall back to the SPA's `index.html`. The split is an example wiring choice (`examples/multi`), not a library constraint — `gw.UIHandler(fs.FS)` and per-handler primitives (`gw.Handler()`, `gw.SchemaHandler()`, `gw.AdminMiddleware(...)`) let operators arrange routes however.

| Path | Auth | What |
|---|---|---|
| `/api/graphql` | public for queries/subs, bearer for mutations (transitive) | GraphQL + WS upgrade |
| `/api/schema/{graphql,proto,openapi}` | public | SDL / FDS / re-emitted OpenAPI |
| `/api/admin/*` reads | public | huma reads |
| `/api/admin/*` writes | bearer | huma mutations |
| `/api/health` | public | 503 during `Drain()` |
| `/api/metrics` | public (or behind reverse-proxy auth) | Prometheus |

Bearer = boot token (logged at startup; persisted to `<adminDataDir>/admin-token` if `WithAdminDataDir` is set). Pluggable AdminAuthorizer delegate (registered under `_admin_auth/v1`) consulted first; boot token underneath as the always-works hatch.

### Schema export selectors

`?service=auth:v1,library` — auth at v1 + all versions of library. Missing version → all versions of namespace; missing param → everything (subject to `_*` internal filtering).

### Dogfooding: huma → OpenAPI → GraphQL

Admin operations are defined via huma (`gw/admin_huma.go`), mounted as HTTP at `/admin/*`, and self-ingested via `AddOpenAPIBytes` so SDL gains nested `Query.admin.listPeers` / `Mutation.admin.forgetPeer`. Same path any external huma service takes — use as template for new admin operations.

### UI

React + MUI v6 + TanStack Router admin console at `ui/`. Build flow:
```
cd examples/multi && ./run.sh         # gateway up
cd ui && pnpm install && pnpm run gen # fetch schema + codegen
pnpm run dev                          # http://localhost:5173
pnpm run build                        # → dist/
```
`pnpm run gen` curls `${GATEWAY_URL}/schema/graphql` then runs graphql-codegen → `src/api/gateway.ts`. Pages: Dashboard (public status when unauth), Services, Deprecated, Injectors, Peers, Schema viewer.
