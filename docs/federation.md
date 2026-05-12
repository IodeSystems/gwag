# Do you need Apollo Federation?

Short answer: **probably not.** Use stitching first; reach for
Federation only when you have entity-merging across services that
*already share entity identity*. gwag does stitching; it does not
replicate Federation's entity-resolution machinery. If you actually
need Federation, use Federation.

This doc is here because "do I need Federation?" is the single most
common adopter question, and "no, we don't ship it" sounds like a
gap when in practice it's a deliberate scope decision. The pages
below are the diagnostic.

## What Federation actually solves

Federation is for the case where **the same entity is defined in
multiple services with overlapping fields**, and clients want a
single cohesive view of that entity at the gateway:

```
auth-svc                         billing-svc
─────────                        ───────────
type User @key(fields: "id") {   type User @key(fields: "id") {
  id: ID!                          id: ID!                # same key, different fields
  email: String!                   monthlyTotal: Decimal!
  passwordHash: String! (hidden)   subscriptionTier: String!
}                                }
```

Federation's `@key` directive declares "these two `User`s are the
same `User`." At query time the router resolves the entity from
whichever service owns each requested field, then assembles one
`User` for the client.

That's a real problem if you have it. It typically shows up when:

- Two or more teams have evolved overlapping representations of the
  same business entity (User, Order, Account, …) and neither owns
  the canonical full row.
- Client queries traverse the entity (`user { email monthlyTotal }`)
  and the gateway has to dispatch to both backends and merge.
- Splitting the entity by service would break the client contract
  (a client today asks for `email` and `monthlyTotal` in one query;
  you can't ask them to issue two queries).

## What stitching does (and why it covers the common case)

gwag stitches: each registered service occupies its own namespace
in the consolidated GraphQL schema, and cross-service references are
**explicit links by ID**, not implicit entity-merging.

```graphql
query {
  auth { user(id: "u_42") { id email } }
  billing { account(userId: "u_42") { monthlyTotal subscriptionTier } }
}
```

Two queries (or one query with two top-level selections), explicit
foreign-key references between them. No `@key`, no entity
resolution, no router-side join. The auth service owns `User`. The
billing service owns `Account` (which happens to reference a user
by id). They never merge into one type.

This shape works when:

- Each service owns a cohesive piece of the domain.
- Cross-service references are by ID (the "foreign key" pattern in
  REST / proto / SQL — the lingua franca of service boundaries).
- Clients are happy with `service.entity` namespacing — and TS /
  Go / Python codegen makes that totally fine for clients.

In our experience this is **most teams**. Federation's setup cost
buys you nothing here.

## How to tell which you need

A working diagnostic, in order of decreasing certainty:

1. **"If I ask a colleague where `User` lives, will they name one
   service?"**
   Yes → stitching. No → maybe Federation.
2. **"Do client queries ever need fields from two services on the
   same entity in one round trip?"**
   No → stitching. Yes → maybe Federation.
3. **"When a service adds a field, does it go on the entity *or*
   on a service-specific projection of it?"**
   Service-specific projection → stitching. The entity → maybe
   Federation.
4. **"Is the ID a stable, durable identifier across services (UUID,
   ULID, opaque token)?"**
   Yes → stitching works fine. No → you have an entity-identity
   problem that *neither* stitching nor Federation solves cleanly.

If #1-3 all answer "Federation territory" and #4 answers "stable
ID," you have the case Federation was designed for. Use it.

If any of #1-3 answer "stitching," gwag covers you.

## Why gwag doesn't ship Federation

Three reasons, in order of importance:

1. **Stitching covers the common case.** Most teams don't have the
   entity-overlap problem; shipping Federation for everyone would
   be carrying machinery that 80%+ of adopters never use.
2. **Federation done half-correctly is worse than no Federation.**
   The `@key` resolution flow has subtle correctness rules
   (entity references, `@external` fields, `@requires`, `@provides`,
   `_entities` query rewriting, etc.). Half an implementation
   silently returns wrong data; we'd rather ship zero than
   ship-a-trap.
3. **Multi-format ingest is the wedge gwag is built around.**
   Federation is GraphQL-only; entity-merging across `.proto` /
   OpenAPI / downstream-GraphQL services would need a parallel
   resolution path per format. That's a different project.

If we ship Federation in the future it will be because a concrete
adopter team showed up with the entity-merging problem and the
willingness to validate the implementation against production
queries. Until then it's listed as a known limitation in
[`plan.md`](./plan.md), not a roadmap gap.

## What to use if you do need Federation

- **[Apollo Router](https://www.apollographql.com/docs/router/)** —
  Rust, the reference implementation, the most-deployed router.
- **[Apollo Gateway](https://www.apollographql.com/docs/federation/v2/gateway/)** —
  Node, original implementation; still common.
- **[Hot Chocolate](https://chillicream.com/docs/hotchocolate/v13/distributed-schema/federation)** —
  .NET, if your stack is .NET-heavy.
- **[Mercurius](https://mercurius.dev/#/docs/federation)** — Node,
  Fastify-shaped.

gwag and Federation are not mutually exclusive at the deployment
level — you can run Federation in front of a gwag instance that
itself stitches a few non-Federation backends. The boundary is
where entity-merging happens; choose per service.

## What gwag gives you that Federation doesn't

If you're choosing between gwag and Federation as your only
gateway, the things gwag does that Federation doesn't are worth
naming:

- **Multi-format ingest.** `.proto` and OpenAPI 3.x are first-class
  alongside GraphQL — register a gRPC service or an OpenAPI service
  and it's in the consolidated schema. Federation requires the
  upstream to speak Federation-compatible GraphQL (subgraph SDL +
  the `_entities` query).
- **Re-emission for typed S2S clients.** Federation gives you one
  GraphQL surface. gwag gives you GraphQL *and* proto *and*
  OpenAPI off the same registry — the Python team uses
  openapi-generator, the Go team uses buf, the TS team uses
  graphql-codegen, all simultaneously. See
  [`walkthrough.md`](./walkthrough.md).
- **Tier-based versioning.** `unstable` / `stable` / `vN` lanes
  built in, with `@deprecated` propagation and CI gates on schema
  diff. Federation's versioning story is per-subgraph.
- **Hot-reload via control plane.** Services self-register; no
  supergraph composition step, no gateway restart. Federation
  needs Apollo Studio or a CI-side composition tool to roll
  subgraph schemas forward.

These are not "Federation is wrong" claims — they're "different
problems." If you need entity-merging, the items above don't
substitute for it.

## See also

- [`README.md`](../README.md) — gwag pitch + magic-moment demo
- [`walkthrough.md`](./walkthrough.md) — canonical worked example
- [`comparison.md`](./comparison.md) — broader landscape (service
  discovery / meshes / Kong / Federation)
- [`plan.md`](./plan.md) — "No Apollo Federation" entry under
  Known limitations
