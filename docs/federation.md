# Apollo Federation

## What it is

Federation is a GraphQL pattern for **entity-merging across services
that share entity identity**. The classic case: `User` defined in
`auth-svc` *and* `billing-svc`, both keyed by the same `id`. A client
asks for `user { email monthlyTotal }`; the router splits the query
by which service owns each field, fetches each part, and assembles
one `User` for the client.

```
auth-svc                       billing-svc
─────────                      ───────────
type User @key(fields: "id") { type User @key(fields: "id") {
  id: ID!                        id: ID!
  email: String!                 monthlyTotal: Decimal!
}                              }
```

`@key` declares "these two `User`s are the same `User`." `@external`,
`@requires`, `@provides`, and the `_entities` query are the rest of
the resolution machinery.

If your services actually share entity identity like that, Federation
is the answer. Use [Apollo Router](https://www.apollographql.com/docs/router/).

## Why it's the wrong shape for gwag

gwag treats each service as its own namespace in the consolidated
schema. Cross-service references are foreign keys by ID — explicit,
not entity-merged:

```graphql
query {
  auth    { user(id: "u_42")        { id email } }
  billing { account(userId: "u_42") { monthlyTotal subscriptionTier } }
}
```

Federation's machinery solves a problem most service architectures
don't have. Three reasons not to reach for it by default:

- **Two teams rarely both *own* the same entity.** One owns `User`;
  the other owns `Account`-that-references-a-user. Foreign-key
  references capture this honestly. `@key` pretends two services
  own one row, which lies about who's responsible for what.
- **Entity-merging requires every participant to speak Federation
  GraphQL.** gwag ingests `.proto`, OpenAPI, and GraphQL. Federation
  handles one of three.
- **Half-implemented Federation silently returns wrong data.**
  Entity resolution order, partial references, `_entities`
  short-circuiting — get any of it wrong and the response looks
  plausible but isn't. Stitching by namespace has no such trap.

If you genuinely have the entity-overlap problem, you don't need a
gateway that pretends two services own one row — you either
consolidate ownership in one service, or accept the merge cost and
use Federation. gwag is not a Federation replacement; it's a
different shape.
