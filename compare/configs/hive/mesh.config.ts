// Hive Gateway compose config for the perf comparison.
//
// `mesh-compose` (@graphql-mesh/compose-cli) reads this file and writes
// supergraph.graphql; `hive-gateway supergraph` then serves it.
// Composition runs once, offline from the request path.
//
// Sources wired:
//   - hello_graphql: GraphQL upstream at hello-graphql (:50054)
//   - hello_openapi: OpenAPI upstream at hello-openapi (:50053)
//   - hello_proto:   gRPC upstream at hello-proto (:50055), from hello.proto
// Each source is encapsulated under its name so root fields match
// gwag's namespaced shape (`hello_graphql { hello(...) }`).
import { defineConfig, loadGraphQLHTTPSubgraph } from '@graphql-mesh/compose-cli';
import { createEncapsulateTransform } from '@graphql-mesh/fusion-composition';
import { loadGrpcSubgraph } from '@omnigraph/grpc';
import { loadOpenAPISubgraph } from '@omnigraph/openapi';

export const composeConfig = defineConfig({
  subgraphs: [
    {
      sourceHandler: loadGraphQLHTTPSubgraph('hello_graphql', {
        endpoint: 'http://localhost:50054/graphql',
        // The upstream is an older GraphQL server; compose-cli's
        // default introspection query asks for newer fields
        // (schema description, isRepeatable, ...) and gets errors.
        // Classic introspection only.
        introspectionOptions: {},
      }),
      transforms: [createEncapsulateTransform()],
    },
    {
      sourceHandler: loadOpenAPISubgraph('hello_openapi', {
        source: 'http://localhost:50053/openapi.json',
        endpoint: 'http://localhost:50053',
      }),
      transforms: [createEncapsulateTransform()],
    },
    {
      sourceHandler: loadGrpcSubgraph('hello_proto', {
        endpoint: 'localhost:50055',
        // hello-proto serves no reflection; load the .proto directly.
        // Path is relative to this directory (start-hive.sh cd's here).
        source: '../../../examples/multi/protos/hello.proto',
        // gRPC unary methods land under Mutation by default; gwag
        // exposes Hello under Query, so match it.
        selectQueryOrMutationField: [{ fieldName: 'hello_v1_HelloService_Hello', type: 'Query' }],
      }),
      transforms: [createEncapsulateTransform()],
    },
  ],
});
