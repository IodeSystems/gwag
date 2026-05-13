# Directives

What `@directive` syntax means in gwag, what's supported, and what
isn't.

## Today

**`@deprecated`.** Auto-applied to fields and types on a `vN` slot
once `vN+1` exists. Rendered into the GraphQL SDL. See
[`lifecycle.md`](./lifecycle.md) for the tier model.

Standard `@include` / `@skip` work in client queries — query-time
selection guards from the GraphQL spec, handled by the graphql-go
fork.

Nothing else is carried.

## Annotations vs directives

GraphQL's `@directive` syntax covers two distinct things:

- **Annotations** — declarative metadata. `@deprecated`,
  `@hasRole(ADMIN)` as a label, `@pii`, `@audit("billing")`. Read by
  humans, codegen, observability. No runtime behavior implied.
- **Active directives** — a hook into the resolver chain: `next()`,
  short-circuit, transform. Middleware expressed in SDL.

Active directives in gwag would be redundant. Transforms and
providers (`Transform` / `InjectType[T]` / `InjectPath` /
`InjectHeader`, plus the ingest providers for proto / OpenAPI /
GraphQL) already do this work, declared in Go and applied
cross-protocol. Adding SDL-side hooks on top would mean writing the
same auth check twice.

Annotations beyond `@deprecated` aren't carried at all.

## Why custom annotations aren't carried

The argument for carrying `@hasRole(ADMIN)` and similar is audit
visibility: someone reading the SDL sees what's enforced without
leaving the schema. That only works when the annotation **is** the
contract — declared by the service that actually enforces the
policy. A gateway-side `@hasRole` on top of a service that doesn't
enforce it makes the SDL lie, so that path is out.

If shipped, the shape would be carrier-only: the service author
declares the annotation in their proto / OpenAPI / SDL; gwag reads
it on ingest and propagates it verbatim into every egress SDL. No
arg-type validation beyond parsing, no runtime exposure, no
generated stubs. Whatever `@hasRole` *does* is the application's
problem — write a transform that reads the IR if you need to act on
it.

No adopter has hit the wall yet. The runtime half of cross-cutting
policy is covered by transforms (in Go, cross-protocol) or by
service-side enforcement (401 / 403 from upstream, passed through).
Until a concrete pull lands — codegen consumer that hides admin-only
fields based on a directive, audit pipeline scraping the SDL for
`@pii`, similar — the visibility half stays open.

## If you want this

Open an issue. The shape is sketched in [`plan.md`](./plan.md) under
Tier 3.0 — IR annotation bindings, emitters render verbatim, runtime
stays in transforms.

## gqlgen

[gqlgen](https://gqlgen.com/reference/directives/) generates Go
stubs from SDL-declared directives that you implement to enforce
policy at field-resolve time. Reasonable when one Go service is the
source of truth and you're authoring its GraphQL surface.

Gwag composes existing services rather than authoring one. The
runtime equivalent (run something on a field) is covered by
transforms and providers, applied across gRPC / OpenAPI / GraphQL.
The piece gqlgen has and gwag doesn't is annotation visibility —
the only piece that would need building if pulled.
