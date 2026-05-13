/* eslint-disable */
import * as types from './graphql';
import type { TypedDocumentNode as DocumentNode } from '@graphql-typed-document-node/core';

/**
 * Map of all GraphQL operations in the project.
 *
 * This map has several performance disadvantages:
 * 1. It is not tree-shakeable, so it will include all operations in the project.
 * 2. It is not minifiable, so the string of a GraphQL query will be multiple times inside the bundle.
 * 3. It does not support dead code elimination, so it will add unused operations.
 *
 * Therefore it is highly recommended to use the babel or swc plugin for production.
 * Learn more about it here: https://the-guild.dev/graphql/codegen/plugins/presets/preset-client#reducing-bundle-size
 */
type Documents = {
    "\n  query Dashboard($window: String!) {\n    admin {\n      servicesHistory(window: $window) {\n        window\n        services {\n          namespace\n          version\n          buckets {\n            startUnixSec\n            durationSec\n            count\n            okCount\n            p50Millis\n            p95Millis\n            p99Millis\n          }\n        }\n      }\n    }\n  }\n": typeof types.DashboardDocument,
    "\n  query Services {\n    admin {\n      listServices {\n        services {\n          namespace\n          version\n          hashHex\n          replicaCount\n          manualDeprecationReason\n        }\n        stableVN {\n          namespace\n          vN\n        }\n      }\n      servicesStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n": typeof types.ServicesDocument,
    "\n  query ServiceStats($namespace: String!, $version: String!) {\n    admin {\n      serviceStats(namespace: $namespace, version: $version, window: \"24h\") {\n        window\n        methods {\n          method\n          caller\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n": typeof types.ServiceStatsDocument,
    "\n  query DeprecatedStats {\n    admin {\n      deprecatedStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          manualReason\n          autoReason\n          totalCount\n          totalThroughput\n          methods {\n            method\n            count\n            okCount\n            throughput\n            p50Millis\n            p95Millis\n            p99Millis\n            callers {\n              caller\n              count\n              okCount\n              throughput\n              p50Millis\n              p95Millis\n              p99Millis\n            }\n          }\n        }\n      }\n    }\n  }\n": typeof types.DeprecatedStatsDocument,
    "\n  mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {\n    admin {\n      deprecate(\n        namespace: $namespace\n        version: $version\n        body: { reason: $reason }\n      ) {\n        __typename\n      }\n    }\n  }\n": typeof types.DeprecateDocument,
    "\n  mutation Undeprecate($namespace: String!, $version: String!) {\n    admin {\n      undeprecate(namespace: $namespace, version: $version) {\n        priorReason\n      }\n    }\n  }\n": typeof types.UndeprecateDocument,
    "\n  mutation RetractStable($namespace: String!, $targetVN: Int!) {\n    admin {\n      retractStable(namespace: $namespace, body: { targetVN: $targetVN }) {\n        priorVN\n        newVN\n      }\n    }\n  }\n": typeof types.RetractStableDocument,
    "\n  query Peers {\n    admin {\n      listPeers {\n        peers {\n          nodeId\n          name\n          joinedUnixMs\n        }\n      }\n    }\n  }\n": typeof types.PeersDocument,
    "\n  query Injectors {\n    admin {\n      listInjectors {\n        injectors {\n          kind\n          typeName\n          path\n          headerName\n          hide\n          nullable\n          state\n          registeredAt {\n            file\n            line\n            function\n          }\n          landings {\n            kind\n            namespace\n            version\n            op\n            typeName\n            fieldName\n            argName\n            headerName\n          }\n        }\n      }\n    }\n  }\n": typeof types.InjectorsDocument,
    "\n  mutation ForgetPeer($nodeId: String!) {\n    admin {\n      forgetPeer(nodeId: $nodeId) {\n        removed\n        newReplicas\n      }\n    }\n  }\n": typeof types.ForgetPeerDocument,
    "\n  query McpConfig {\n    admin {\n      mcpList {\n        autoInclude\n        include\n        exclude\n      }\n      mcpSchemaList {\n        entries {\n          path\n          kind\n          namespace\n          version\n          description\n        }\n      }\n    }\n  }\n": typeof types.McpConfigDocument,
    "\n  mutation McpInclude($path: String!) {\n    admin {\n      mcpInclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": typeof types.McpIncludeDocument,
    "\n  mutation McpExclude($path: String!) {\n    admin {\n      mcpExclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": typeof types.McpExcludeDocument,
    "\n  mutation McpIncludeRemove($path: String!) {\n    admin {\n      mcpIncludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": typeof types.McpIncludeRemoveDocument,
    "\n  mutation McpExcludeRemove($path: String!) {\n    admin {\n      mcpExcludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": typeof types.McpExcludeRemoveDocument,
    "\n  mutation McpSetAutoInclude($autoInclude: Boolean!) {\n    admin {\n      mcpSetAutoInclude(body: { autoInclude: $autoInclude }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": typeof types.McpSetAutoIncludeDocument,
    "\n  subscription AdminEvents($namespace: String) {\n    admin_events_watchServices(namespace: $namespace) {\n      action\n      namespace\n      version\n      addr\n      timestampUnixMs\n      replicaCount\n    }\n  }\n": typeof types.AdminEventsDocument,
};
const documents: Documents = {
    "\n  query Dashboard($window: String!) {\n    admin {\n      servicesHistory(window: $window) {\n        window\n        services {\n          namespace\n          version\n          buckets {\n            startUnixSec\n            durationSec\n            count\n            okCount\n            p50Millis\n            p95Millis\n            p99Millis\n          }\n        }\n      }\n    }\n  }\n": types.DashboardDocument,
    "\n  query Services {\n    admin {\n      listServices {\n        services {\n          namespace\n          version\n          hashHex\n          replicaCount\n          manualDeprecationReason\n        }\n        stableVN {\n          namespace\n          vN\n        }\n      }\n      servicesStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n": types.ServicesDocument,
    "\n  query ServiceStats($namespace: String!, $version: String!) {\n    admin {\n      serviceStats(namespace: $namespace, version: $version, window: \"24h\") {\n        window\n        methods {\n          method\n          caller\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n": types.ServiceStatsDocument,
    "\n  query DeprecatedStats {\n    admin {\n      deprecatedStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          manualReason\n          autoReason\n          totalCount\n          totalThroughput\n          methods {\n            method\n            count\n            okCount\n            throughput\n            p50Millis\n            p95Millis\n            p99Millis\n            callers {\n              caller\n              count\n              okCount\n              throughput\n              p50Millis\n              p95Millis\n              p99Millis\n            }\n          }\n        }\n      }\n    }\n  }\n": types.DeprecatedStatsDocument,
    "\n  mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {\n    admin {\n      deprecate(\n        namespace: $namespace\n        version: $version\n        body: { reason: $reason }\n      ) {\n        __typename\n      }\n    }\n  }\n": types.DeprecateDocument,
    "\n  mutation Undeprecate($namespace: String!, $version: String!) {\n    admin {\n      undeprecate(namespace: $namespace, version: $version) {\n        priorReason\n      }\n    }\n  }\n": types.UndeprecateDocument,
    "\n  mutation RetractStable($namespace: String!, $targetVN: Int!) {\n    admin {\n      retractStable(namespace: $namespace, body: { targetVN: $targetVN }) {\n        priorVN\n        newVN\n      }\n    }\n  }\n": types.RetractStableDocument,
    "\n  query Peers {\n    admin {\n      listPeers {\n        peers {\n          nodeId\n          name\n          joinedUnixMs\n        }\n      }\n    }\n  }\n": types.PeersDocument,
    "\n  query Injectors {\n    admin {\n      listInjectors {\n        injectors {\n          kind\n          typeName\n          path\n          headerName\n          hide\n          nullable\n          state\n          registeredAt {\n            file\n            line\n            function\n          }\n          landings {\n            kind\n            namespace\n            version\n            op\n            typeName\n            fieldName\n            argName\n            headerName\n          }\n        }\n      }\n    }\n  }\n": types.InjectorsDocument,
    "\n  mutation ForgetPeer($nodeId: String!) {\n    admin {\n      forgetPeer(nodeId: $nodeId) {\n        removed\n        newReplicas\n      }\n    }\n  }\n": types.ForgetPeerDocument,
    "\n  query McpConfig {\n    admin {\n      mcpList {\n        autoInclude\n        include\n        exclude\n      }\n      mcpSchemaList {\n        entries {\n          path\n          kind\n          namespace\n          version\n          description\n        }\n      }\n    }\n  }\n": types.McpConfigDocument,
    "\n  mutation McpInclude($path: String!) {\n    admin {\n      mcpInclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": types.McpIncludeDocument,
    "\n  mutation McpExclude($path: String!) {\n    admin {\n      mcpExclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": types.McpExcludeDocument,
    "\n  mutation McpIncludeRemove($path: String!) {\n    admin {\n      mcpIncludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": types.McpIncludeRemoveDocument,
    "\n  mutation McpExcludeRemove($path: String!) {\n    admin {\n      mcpExcludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": types.McpExcludeRemoveDocument,
    "\n  mutation McpSetAutoInclude($autoInclude: Boolean!) {\n    admin {\n      mcpSetAutoInclude(body: { autoInclude: $autoInclude }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n": types.McpSetAutoIncludeDocument,
    "\n  subscription AdminEvents($namespace: String) {\n    admin_events_watchServices(namespace: $namespace) {\n      action\n      namespace\n      version\n      addr\n      timestampUnixMs\n      replicaCount\n    }\n  }\n": types.AdminEventsDocument,
};

/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 *
 *
 * @example
 * ```ts
 * const query = graphql(`query GetUser($id: ID!) { user(id: $id) { name } }`);
 * ```
 *
 * The query argument is unknown!
 * Please regenerate the types.
 */
export function graphql(source: string): unknown;

/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query Dashboard($window: String!) {\n    admin {\n      servicesHistory(window: $window) {\n        window\n        services {\n          namespace\n          version\n          buckets {\n            startUnixSec\n            durationSec\n            count\n            okCount\n            p50Millis\n            p95Millis\n            p99Millis\n          }\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query Dashboard($window: String!) {\n    admin {\n      servicesHistory(window: $window) {\n        window\n        services {\n          namespace\n          version\n          buckets {\n            startUnixSec\n            durationSec\n            count\n            okCount\n            p50Millis\n            p95Millis\n            p99Millis\n          }\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query Services {\n    admin {\n      listServices {\n        services {\n          namespace\n          version\n          hashHex\n          replicaCount\n          manualDeprecationReason\n        }\n        stableVN {\n          namespace\n          vN\n        }\n      }\n      servicesStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query Services {\n    admin {\n      listServices {\n        services {\n          namespace\n          version\n          hashHex\n          replicaCount\n          manualDeprecationReason\n        }\n        stableVN {\n          namespace\n          vN\n        }\n      }\n      servicesStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query ServiceStats($namespace: String!, $version: String!) {\n    admin {\n      serviceStats(namespace: $namespace, version: $version, window: \"24h\") {\n        window\n        methods {\n          method\n          caller\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query ServiceStats($namespace: String!, $version: String!) {\n    admin {\n      serviceStats(namespace: $namespace, version: $version, window: \"24h\") {\n        window\n        methods {\n          method\n          caller\n          count\n          okCount\n          throughput\n          p50Millis\n          p95Millis\n          p99Millis\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query DeprecatedStats {\n    admin {\n      deprecatedStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          manualReason\n          autoReason\n          totalCount\n          totalThroughput\n          methods {\n            method\n            count\n            okCount\n            throughput\n            p50Millis\n            p95Millis\n            p99Millis\n            callers {\n              caller\n              count\n              okCount\n              throughput\n              p50Millis\n              p95Millis\n              p99Millis\n            }\n          }\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query DeprecatedStats {\n    admin {\n      deprecatedStats(window: \"24h\") {\n        window\n        services {\n          namespace\n          version\n          manualReason\n          autoReason\n          totalCount\n          totalThroughput\n          methods {\n            method\n            count\n            okCount\n            throughput\n            p50Millis\n            p95Millis\n            p99Millis\n            callers {\n              caller\n              count\n              okCount\n              throughput\n              p50Millis\n              p95Millis\n              p99Millis\n            }\n          }\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {\n    admin {\n      deprecate(\n        namespace: $namespace\n        version: $version\n        body: { reason: $reason }\n      ) {\n        __typename\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {\n    admin {\n      deprecate(\n        namespace: $namespace\n        version: $version\n        body: { reason: $reason }\n      ) {\n        __typename\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation Undeprecate($namespace: String!, $version: String!) {\n    admin {\n      undeprecate(namespace: $namespace, version: $version) {\n        priorReason\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation Undeprecate($namespace: String!, $version: String!) {\n    admin {\n      undeprecate(namespace: $namespace, version: $version) {\n        priorReason\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation RetractStable($namespace: String!, $targetVN: Int!) {\n    admin {\n      retractStable(namespace: $namespace, body: { targetVN: $targetVN }) {\n        priorVN\n        newVN\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation RetractStable($namespace: String!, $targetVN: Int!) {\n    admin {\n      retractStable(namespace: $namespace, body: { targetVN: $targetVN }) {\n        priorVN\n        newVN\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query Peers {\n    admin {\n      listPeers {\n        peers {\n          nodeId\n          name\n          joinedUnixMs\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query Peers {\n    admin {\n      listPeers {\n        peers {\n          nodeId\n          name\n          joinedUnixMs\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query Injectors {\n    admin {\n      listInjectors {\n        injectors {\n          kind\n          typeName\n          path\n          headerName\n          hide\n          nullable\n          state\n          registeredAt {\n            file\n            line\n            function\n          }\n          landings {\n            kind\n            namespace\n            version\n            op\n            typeName\n            fieldName\n            argName\n            headerName\n          }\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query Injectors {\n    admin {\n      listInjectors {\n        injectors {\n          kind\n          typeName\n          path\n          headerName\n          hide\n          nullable\n          state\n          registeredAt {\n            file\n            line\n            function\n          }\n          landings {\n            kind\n            namespace\n            version\n            op\n            typeName\n            fieldName\n            argName\n            headerName\n          }\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation ForgetPeer($nodeId: String!) {\n    admin {\n      forgetPeer(nodeId: $nodeId) {\n        removed\n        newReplicas\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation ForgetPeer($nodeId: String!) {\n    admin {\n      forgetPeer(nodeId: $nodeId) {\n        removed\n        newReplicas\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  query McpConfig {\n    admin {\n      mcpList {\n        autoInclude\n        include\n        exclude\n      }\n      mcpSchemaList {\n        entries {\n          path\n          kind\n          namespace\n          version\n          description\n        }\n      }\n    }\n  }\n"): (typeof documents)["\n  query McpConfig {\n    admin {\n      mcpList {\n        autoInclude\n        include\n        exclude\n      }\n      mcpSchemaList {\n        entries {\n          path\n          kind\n          namespace\n          version\n          description\n        }\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation McpInclude($path: String!) {\n    admin {\n      mcpInclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation McpInclude($path: String!) {\n    admin {\n      mcpInclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation McpExclude($path: String!) {\n    admin {\n      mcpExclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation McpExclude($path: String!) {\n    admin {\n      mcpExclude(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation McpIncludeRemove($path: String!) {\n    admin {\n      mcpIncludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation McpIncludeRemove($path: String!) {\n    admin {\n      mcpIncludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation McpExcludeRemove($path: String!) {\n    admin {\n      mcpExcludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation McpExcludeRemove($path: String!) {\n    admin {\n      mcpExcludeRemove(body: { path: $path }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  mutation McpSetAutoInclude($autoInclude: Boolean!) {\n    admin {\n      mcpSetAutoInclude(body: { autoInclude: $autoInclude }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"): (typeof documents)["\n  mutation McpSetAutoInclude($autoInclude: Boolean!) {\n    admin {\n      mcpSetAutoInclude(body: { autoInclude: $autoInclude }) {\n        autoInclude\n        include\n        exclude\n      }\n    }\n  }\n"];
/**
 * The graphql function is used to parse GraphQL queries into a document that can be used by GraphQL clients.
 */
export function graphql(source: "\n  subscription AdminEvents($namespace: String) {\n    admin_events_watchServices(namespace: $namespace) {\n      action\n      namespace\n      version\n      addr\n      timestampUnixMs\n      replicaCount\n    }\n  }\n"): (typeof documents)["\n  subscription AdminEvents($namespace: String) {\n    admin_events_watchServices(namespace: $namespace) {\n      action\n      namespace\n      version\n      addr\n      timestampUnixMs\n      replicaCount\n    }\n  }\n"];

export function graphql(source: string) {
  return (documents as any)[source] ?? {};
}

export type DocumentType<TDocumentNode extends DocumentNode<any, any>> = TDocumentNode extends DocumentNode<  infer TType,  any>  ? TType  : never;