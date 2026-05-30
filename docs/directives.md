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

**Custom annotations** (`@hasRole(role: "ADMIN")`, `@pii`, …) are
carried verbatim from the contract that declares them into every
egress SDL. See [Custom annotations](#custom-annotations-carriage)
below.

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

## Custom annotations (carriage)

Carrier-only by design: the **service that enforces the policy
declares the annotation**, gwag reads it on ingest and propagates it
verbatim into every egress SDL. No arg-type validation beyond
parsing, no runtime exposure, no generated stubs. Whatever
`@hasRole` *does* is the application's problem — write a transform
that reads the IR if you need to act on it. A gateway-side
`@hasRole` on top of a service that doesn't enforce it would make
the SDL lie, so there's no gateway-side authoring path.

### Declaring

**OpenAPI** (incl. huma — set it on the operation / schema): an
`x-gwag-annotations` extension, a list of `{name, args}`.

```yaml
paths:
  /projects:
    get:
      operationId: listProjects
      x-gwag-annotations:
        - name: hasRole
          args: { role: ADMIN }
        - name: audited
```

**proto**: one `@gql` leading-comment line per annotation,
GraphQL-directive syntax minus the leading `@`.

```proto
// @gql hasRole(role: "ADMIN")
// @gql audited
rpc ListProjects(ListProjectsRequest) returns (ListProjectsResponse);
```

Arg values are GraphQL scalar literals: quoted strings, numbers,
`true`/`false`, or barewords (enum). Annotations attach to
operations, object/input/enum/union/scalar types, and fields.

### Egress

- **GraphQL SDL** — real directives on the element, e.g.
  `listProjects: ... @audited @hasRole(role: "ADMIN")`, plus a
  synthesized `directive @hasRole(role: String) on …` declaration so
  the document stays self-validating. Directives render sorted by
  name for a stable diff.
- **OpenAPI** — `x-gwag-annotations` on the operation (round-trips).
- **proto** — `@gql` leading comments in the proto SDL view.

### Limits

- **GraphQL is a destination, not a source.** A downstream GraphQL
  service's directives can't be read: introspection (how gwag
  ingests `AddGraphQL`) exposes only `@deprecated`, never arbitrary
  applied directives. So annotations flow *out* to GraphQL, never
  *in* from it.
- **SDL only, not introspection.** The directives appear in the SDL
  served at `/schema/graphql`, but **not** in the introspection JSON
  (`?format=json`) — graphql-go's schema has no slot for applied
  directives. Codegen consumers that introspect the live endpoint
  won't see them; point them at the SDL endpoint instead.
- **First marker wins** on a name clash; multi-source carriage on one
  element is deferred.

## gqlgen

[gqlgen](https://gqlgen.com/reference/directives/) generates Go
stubs from SDL-declared directives that you implement to enforce
policy at field-resolve time. Reasonable when one Go service is the
source of truth and you're authoring its GraphQL surface.

Gwag composes existing services rather than authoring one. The
runtime equivalent (run something on a field) is covered by
transforms and providers, applied across gRPC / OpenAPI / GraphQL.
What gqlgen has on top — turning SDL directives into enforced Go
stubs — gwag deliberately doesn't: it carries the annotation for
visibility (see above) but leaves enforcement to the service or a
transform.
