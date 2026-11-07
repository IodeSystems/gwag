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

// Role of the stored token (anonymous / dev / admin); see api/role.ts.
export const WhoamiQuery = graphql(`
  query Whoami {
    admin {
      whoami {
        role
      }
    }
  }
`);

export const EnvironmentQuery = graphql(`
  query Environment {
    admin {
      listServices {
        environment
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
          public
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

export const SignSubscriptionTokenMutation = graphql(`
  mutation SignSubscriptionToken($channel: String!, $ttlSeconds: Long!, $kid: String) {
    admin {
      signSubscriptionToken(
        body: { channel: $channel, ttlSeconds: $ttlSeconds, kid: $kid }
      ) {
        code
        hmac
        kid
        reason
        timestampUnix
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

export const DependencyGraphQuery = graphql(`
  query DependencyGraph($window: String) {
    admin {
      dependencyGraph(window: $window) {
        environment
        window
        scope
        callerSource
        nodes {
          id
          namespace
          version
          kind
          public
          owner {
            team
            contact
          }
          replicaCount
          readyCount
          waitingCount
          degradedCount
          drainingCount
          trafficCount
          lastSeenUnixMs
          sunsetable
          sunsetReason
          instances {
            registrationId
            instanceId
            addr
            state
            drainState
            drainReasons
          }
        }
        desiredEdges {
          consumerKind
          consumerNamespace
          consumerVersion
          provider
          lines
          presentLines
          met
          instanceCount
        }
        activeEdges {
          caller
          consumerNamespace
          consumerVersion
          namespace
          version
          count
          okCount
          lastSeenUnixMs
          methods {
            method
            count
            okCount
          }
        }
        externalDependents {
          id
          name
          kind
          owner {
            team
            contact
          }
          depends {
            service
            lines
          }
          note
          reviewAt
          reviewDue
          reviewedAt
          reviewedBy
          updatedUnixMs
        }
      }
    }
  }
`);

export const PutExternalDependentMutation = graphql(`
  mutation PutExternalDependent($id: String!, $body: admin_PutExternalDependentInBodyInput!) {
    admin {
      putExternalDependent(id: $id, body: $body) {
        externalDependent {
          id
        }
        warnings
      }
    }
  }
`);

export const ReviewExternalDependentMutation = graphql(`
  mutation ReviewExternalDependent($id: String!, $body: admin_ReviewExternalDependentInBodyInput!) {
    admin {
      reviewExternalDependent(id: $id, body: $body) {
        externalDependent {
          id
          reviewAt
        }
        warnings
      }
    }
  }
`);

export const NotificationsQuery = graphql(`
  query Notifications {
    admin {
      listNotifications {
        items {
          kind
          id
          title
          detail
          since
          href
        }
      }
    }
  }
`);

export const DeleteExternalDependentMutation = graphql(`
  mutation DeleteExternalDependent($id: String!) {
    admin {
      deleteExternalDependent(id: $id) {
        deleted
      }
    }
  }
`);

export const DrainInstanceMutation = graphql(`
  mutation DrainInstance($instance: String!, $cancel: Boolean) {
    admin {
      drainInstance(instance: $instance, body: { cancel: $cancel }) {
        registrationId
        state
        reasons
        inflight
      }
    }
  }
`);
