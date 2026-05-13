// Typed GraphQL operations consumed by the React UI. Each operation
// is a `graphql(...)` tagged template; codegen (client-preset) emits
// the matching TypedDocumentNode in `src/gql/`. Variables and result
// types flow through automatically — no manual `<T>` at call sites.
//
// Add new operations here, then run `pnpm run codegen`.
//
// Admin operations live under the `admin` namespace container — the
// IR renderer nests every Query/Mutation namespace, so the `admin`
// field on Query / Mutation returns an Admin{Query,Mutation}Namespace
// object whose fields are the actual operations. Subscriptions stay
// flat (graphql-go forbids nested objects under Subscription).

import { graphql } from '../gql';

export const DashboardQuery = graphql(`
  query Dashboard($window: String!) {
    admin {
      servicesHistory(window: $window) {
        window
        services {
          namespace
          version
          buckets {
            startUnixSec
            durationSec
            count
            okCount
            p50Millis
            p95Millis
            p99Millis
          }
        }
      }
    }
  }
`);

export const ServicesQuery = graphql(`
  query Services {
    admin {
      listServices {
        services {
          namespace
          version
          hashHex
          replicaCount
          manualDeprecationReason
        }
        stableVN {
          namespace
          vN
        }
      }
      servicesStats(window: "24h") {
        window
        services {
          namespace
          version
          count
          okCount
          throughput
          p50Millis
          p95Millis
          p99Millis
        }
      }
    }
  }
`);

export const ServiceStatsQuery = graphql(`
  query ServiceStats($namespace: String!, $version: String!) {
    admin {
      serviceStats(namespace: $namespace, version: $version, window: "24h") {
        window
        methods {
          method
          caller
          count
          okCount
          throughput
          p50Millis
          p95Millis
          p99Millis
        }
      }
    }
  }
`);

export const DeprecatedStatsQuery = graphql(`
  query DeprecatedStats {
    admin {
      deprecatedStats(window: "24h") {
        window
        services {
          namespace
          version
          manualReason
          autoReason
          totalCount
          totalThroughput
          methods {
            method
            count
            okCount
            throughput
            p50Millis
            p95Millis
            p99Millis
            callers {
              caller
              count
              okCount
              throughput
              p50Millis
              p95Millis
              p99Millis
            }
          }
        }
      }
    }
  }
`);

export const DeprecateMutation = graphql(`
  mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {
    admin {
      deprecate(
        namespace: $namespace
        version: $version
        body: { reason: $reason }
      ) {
        __typename
      }
    }
  }
`);

export const UndeprecateMutation = graphql(`
  mutation Undeprecate($namespace: String!, $version: String!) {
    admin {
      undeprecate(namespace: $namespace, version: $version) {
        priorReason
      }
    }
  }
`);

export const RetractStableMutation = graphql(`
  mutation RetractStable($namespace: String!, $targetVN: Int!) {
    admin {
      retractStable(namespace: $namespace, body: { targetVN: $targetVN }) {
        priorVN
        newVN
      }
    }
  }
`);

export const PeersQuery = graphql(`
  query Peers {
    admin {
      listPeers {
        peers {
          nodeId
          name
          joinedUnixMs
        }
      }
    }
  }
`);

export const InjectorsQuery = graphql(`
  query Injectors {
    admin {
      listInjectors {
        injectors {
          kind
          typeName
          path
          headerName
          hide
          nullable
          state
          registeredAt {
            file
            line
            function
          }
          landings {
            kind
            namespace
            version
            op
            typeName
            fieldName
            argName
            headerName
          }
        }
      }
    }
  }
`);

export const ForgetPeerMutation = graphql(`
  mutation ForgetPeer($nodeId: String!) {
    admin {
      forgetPeer(nodeId: $nodeId) {
        removed
        newReplicas
      }
    }
  }
`);

export const McpConfigQuery = graphql(`
  query McpConfig {
    admin {
      mcpList {
        autoInclude
        include
        exclude
      }
      mcpSchemaList {
        entries {
          path
          kind
          namespace
          version
          description
        }
      }
    }
  }
`);

export const McpIncludeMutation = graphql(`
  mutation McpInclude($path: String!) {
    admin {
      mcpInclude(body: { path: $path }) {
        autoInclude
        include
        exclude
      }
    }
  }
`);

export const McpExcludeMutation = graphql(`
  mutation McpExclude($path: String!) {
    admin {
      mcpExclude(body: { path: $path }) {
        autoInclude
        include
        exclude
      }
    }
  }
`);

export const McpIncludeRemoveMutation = graphql(`
  mutation McpIncludeRemove($path: String!) {
    admin {
      mcpIncludeRemove(body: { path: $path }) {
        autoInclude
        include
        exclude
      }
    }
  }
`);

export const McpExcludeRemoveMutation = graphql(`
  mutation McpExcludeRemove($path: String!) {
    admin {
      mcpExcludeRemove(body: { path: $path }) {
        autoInclude
        include
        exclude
      }
    }
  }
`);

export const McpSetAutoIncludeMutation = graphql(`
  mutation McpSetAutoInclude($autoInclude: Boolean!) {
    admin {
      mcpSetAutoInclude(body: { autoInclude: $autoInclude }) {
        autoInclude
        include
        exclude
      }
    }
  }
`);

export const AdminEventsSubscription = graphql(`
  subscription AdminEvents($namespace: String) {
    admin_events_watchServices(namespace: $namespace) {
      action
      namespace
      version
      addr
      timestampUnixMs
      replicaCount
    }
  }
`);
