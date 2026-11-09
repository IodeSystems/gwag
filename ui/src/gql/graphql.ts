/* eslint-disable */
/** Internal type. DO NOT USE DIRECTLY. */
type Exact<T extends { [key: string]: unknown }> = { [K in keyof T]: T[K] };
/** Internal type. DO NOT USE DIRECTLY. */
export type Incremental<T> = T | { [P in keyof T]?: P extends ' $fragmentName' | '__typename' ? T[P] : never };
import type { TypedDocumentNode as DocumentNode } from '@graphql-typed-document-node/core';
export type Gateway_Adminevents_V1_ServiceChange_Action =
  | 'ACTION_DEREGISTERED'
  | 'ACTION_REGISTERED'
  | 'ACTION_UNSPECIFIED';

/** header: caller ids come from a request header and can be forged; extractor: a configured CallerIDExtractor; none: every caller is "unknown". */
export type Admin_DependencyGraphOutBodyCallerSource =
  | 'extractor'
  | 'header'
  | 'none';

/** Usage is per gateway node, in memory. */
export type Admin_DependencyGraphOutBodyScope =
  | 'node';

export type Admin_ExternalOwnerInInput = {
  contact: string | null | undefined;
  team: string | null | undefined;
};

export type Admin_ExternalPinInInput = {
  /** Accepted lines: vN or unstable, the same rule as a binding's depends. */
  lines: Array<string> | null | undefined;
  /** Provider namespace. Need not be registered yet. */
  service: string;
};

/** service: a registered line; external: a declared external dependent (consumerNamespace is its id, consumerVersion is empty). */
export type Admin_GraphDesiredEdgeConsumerKind =
  | 'external'
  | 'service';

export type Admin_GraphInstanceDrainState =
  | 'blocked'
  | 'draining'
  | 'safe_to_stop'
  | 'serving';

export type Admin_GraphInstanceState =
  | 'degraded'
  | 'ready'
  | 'waiting';

/** none: the instance declares no verify. pending or red: not routable (state waiting). */
export type Admin_GraphInstanceVerification =
  | 'green'
  | 'none'
  | 'pending'
  | 'red';

export type Admin_InstanceDrainInfoState =
  | 'blocked'
  | 'draining'
  | 'safe_to_stop'
  | 'serving';

export type Admin_PutExternalDependentInBodyInput = {
  /** Pinned lines per service; at least one. */
  depends: Array<Admin_ExternalPinInInput> | null | undefined;
  /** customer, partner, app or other. */
  kind: string;
  /** Display name, e.g. "Acme Corp". */
  name: string | null | undefined;
  note: string | null | undefined;
  owner: Admin_ExternalOwnerInInput | null | undefined;
  /** RFC 3339 date-time, required. Past it the declaration still counts; it is listed as review-due and raises a notification. */
  reviewAt: string;
};

export type Admin_ReviewExternalDependentInBodyInput = {
  /** Next review date, RFC 3339; must be in the future. The reviewer is the authenticated caller; the body names no one. */
  reviewAt: string;
};

/** Role of the presented bearer. anonymous: no bearer; reads show public services only. */
export type Admin_WhoamiOutBodyRole =
  | 'admin'
  | 'anonymous'
  | 'dev';

export type DashboardQueryVariables = Exact<{
  window: string;
}>;


export type DashboardQuery = { admin: { servicesHistory: { window: string, services: Array<{ namespace: string, version: string, buckets: Array<{ startUnixSec: string, durationSec: string, count: string, okCount: string, p50Millis: string, p95Millis: string, p99Millis: string }> | null }> | null } | null } };

export type WhoamiQueryVariables = Exact<{ [key: string]: never; }>;


export type WhoamiQuery = { admin: { whoami: { role: Admin_WhoamiOutBodyRole } | null } };

export type EnvironmentQueryVariables = Exact<{ [key: string]: never; }>;


export type EnvironmentQuery = { admin: { listServices: { environment: string } | null } };

export type ServicesQueryVariables = Exact<{ [key: string]: never; }>;


export type ServicesQuery = { admin: { listServices: { services: Array<{ namespace: string, version: string, public: boolean, hashHex: string, replicaCount: number, manualDeprecationReason: string | null }> | null, stableVN: Array<{ namespace: string, vN: number }> | null } | null, servicesStats: { window: string, services: Array<{ namespace: string, version: string, count: string, okCount: string, throughput: number, p50Millis: string, p95Millis: string, p99Millis: string }> | null } | null } };

export type ServiceStatsQueryVariables = Exact<{
  namespace: string;
  version: string;
}>;


export type ServiceStatsQuery = { admin: { serviceStats: { window: string, methods: Array<{ method: string, caller: string, count: string, okCount: string, throughput: number, p50Millis: string, p95Millis: string, p99Millis: string }> | null } | null } };

export type DeprecatedStatsQueryVariables = Exact<{ [key: string]: never; }>;


export type DeprecatedStatsQuery = { admin: { deprecatedStats: { window: string, services: Array<{ namespace: string, version: string, manualReason: string | null, autoReason: string | null, totalCount: string, totalThroughput: number, methods: Array<{ method: string, count: string, okCount: string, throughput: number, p50Millis: string, p95Millis: string, p99Millis: string, callers: Array<{ caller: string, count: string, okCount: string, throughput: number, p50Millis: string, p95Millis: string, p99Millis: string }> | null }> | null }> | null } | null } };

export type DeprecateMutationVariables = Exact<{
  namespace: string;
  version: string;
  reason: string;
}>;


export type DeprecateMutation = { admin: { deprecate: { __typename: 'admin_DeprecateOutBody' } | null } };

export type UndeprecateMutationVariables = Exact<{
  namespace: string;
  version: string;
}>;


export type UndeprecateMutation = { admin: { undeprecate: { priorReason: string } | null } };

export type RetractStableMutationVariables = Exact<{
  namespace: string;
  targetVN: number;
}>;


export type RetractStableMutation = { admin: { retractStable: { priorVN: number, newVN: number } | null } };

export type SignSubscriptionTokenMutationVariables = Exact<{
  channel: string;
  ttlSeconds: string | number;
  kid: string | null | undefined;
}>;


export type SignSubscriptionTokenMutation = { admin: { signSubscriptionToken: { code: string, hmac: string | null, kid: string | null, reason: string | null, timestampUnix: string | null } | null } };

export type PeersQueryVariables = Exact<{ [key: string]: never; }>;


export type PeersQuery = { admin: { listPeers: { activeLevel: number, metaLeader: string | null, metaSize: number, metaOffline: Array<string> | null, peers: Array<{ nodeId: string, name: string | null, joinedUnixMs: string, version: string | null, minLevel: number, maxLevel: number }> | null } | null } };

export type InjectorsQueryVariables = Exact<{ [key: string]: never; }>;


export type InjectorsQuery = { admin: { listInjectors: { injectors: Array<{ kind: string, typeName: string | null, path: string | null, headerName: string | null, hide: boolean, nullable: boolean, state: string, registeredAt: { file: string | null, line: string | null, function: string | null }, landings: Array<{ kind: string, namespace: string | null, version: string | null, op: string | null, typeName: string | null, fieldName: string | null, argName: string | null, headerName: string | null }> | null }> | null } | null } };

export type ForgetPeerMutationVariables = Exact<{
  nodeId: string;
}>;


export type ForgetPeerMutation = { admin: { forgetPeer: { removed: boolean, newReplicas: number } | null } };

export type McpConfigQueryVariables = Exact<{ [key: string]: never; }>;


export type McpConfigQuery = { admin: { mcpList: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null, mcpSchemaList: { entries: Array<{ path: string, kind: string, namespace: string, version: string, description: string | null }> | null } | null } };

export type McpIncludeMutationVariables = Exact<{
  path: string;
}>;


export type McpIncludeMutation = { admin: { mcpInclude: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null } };

export type McpExcludeMutationVariables = Exact<{
  path: string;
}>;


export type McpExcludeMutation = { admin: { mcpExclude: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null } };

export type McpIncludeRemoveMutationVariables = Exact<{
  path: string;
}>;


export type McpIncludeRemoveMutation = { admin: { mcpIncludeRemove: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null } };

export type McpExcludeRemoveMutationVariables = Exact<{
  path: string;
}>;


export type McpExcludeRemoveMutation = { admin: { mcpExcludeRemove: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null } };

export type McpSetAutoIncludeMutationVariables = Exact<{
  autoInclude: boolean;
}>;


export type McpSetAutoIncludeMutation = { admin: { mcpSetAutoInclude: { autoInclude: boolean, include: Array<string> | null, exclude: Array<string> | null } | null } };

export type AdminEventsSubscriptionVariables = Exact<{
  namespace: string | null | undefined;
}>;


export type AdminEventsSubscription = { admin_events_watchServices: { action: Gateway_Adminevents_V1_ServiceChange_Action | null, namespace: string | null, version: string | null, addr: string | null, timestampUnixMs: string | null, replicaCount: number | null } | null };

export type DependencyGraphQueryVariables = Exact<{
  window: string | null | undefined;
}>;


export type DependencyGraphQuery = { admin: { dependencyGraph: { environment: string, window: string, scope: Admin_DependencyGraphOutBodyScope, callerSource: Admin_DependencyGraphOutBodyCallerSource, nodes: Array<{ id: string, namespace: string, version: string, kind: string, public: boolean, replicaCount: number, readyCount: string, waitingCount: string, degradedCount: string, drainingCount: string, trafficCount: string, lastSeenUnixMs: string, sunsetable: boolean, sunsetReason: string, owner: { team: string, contact: string } | null, instances: Array<{ registrationId: string, instanceId: string | null, addr: string, state: Admin_GraphInstanceState, drainState: Admin_GraphInstanceDrainState, drainReasons: Array<string> | null, verification: Admin_GraphInstanceVerification, verifyReasons: Array<string> | null }> | null }> | null, desiredEdges: Array<{ consumerKind: Admin_GraphDesiredEdgeConsumerKind, consumerNamespace: string, consumerVersion: string, provider: string, lines: Array<string> | null, presentLines: Array<string> | null, met: boolean, instanceCount: string }> | null, activeEdges: Array<{ caller: string, consumerNamespace: string, consumerVersion: string, namespace: string, version: string, count: string, okCount: string, lastSeenUnixMs: string, methods: Array<{ method: string, count: string, okCount: string }> | null }> | null, externalDependents: Array<{ id: string, name: string, kind: string, note: string, reviewAt: string, reviewDue: boolean, reviewedAt: string, reviewedBy: string, updatedUnixMs: string, updatedBy: string, owner: { team: string, contact: string } | null, depends: Array<{ service: string, lines: Array<string> | null }> | null }> | null } | null } };

export type PutExternalDependentMutationVariables = Exact<{
  id: string;
  body: Admin_PutExternalDependentInBodyInput;
}>;


export type PutExternalDependentMutation = { admin: { putExternalDependent: { warnings: Array<string> | null, externalDependent: { id: string } } | null } };

export type ReviewExternalDependentMutationVariables = Exact<{
  id: string;
  body: Admin_ReviewExternalDependentInBodyInput;
}>;


export type ReviewExternalDependentMutation = { admin: { reviewExternalDependent: { warnings: Array<string> | null, externalDependent: { id: string, reviewAt: string } } | null } };

export type NotificationsQueryVariables = Exact<{ [key: string]: never; }>;


export type NotificationsQuery = { admin: { listNotifications: { items: Array<{ kind: string, id: string, title: string, detail: string, since: string, href: string, dismissable: boolean }> | null } | null } };

export type DismissNotificationMutationVariables = Exact<{
  id: string;
}>;


export type DismissNotificationMutation = { admin: { dismissNotification: { dismissed: boolean } | null } };

export type DeleteExternalDependentMutationVariables = Exact<{
  id: string;
}>;


export type DeleteExternalDependentMutation = { admin: { deleteExternalDependent: { deleted: boolean } | null } };

export type DrainInstanceMutationVariables = Exact<{
  instance: string;
  cancel: boolean | null | undefined;
}>;


export type DrainInstanceMutation = { admin: { drainInstance: { registrationId: string, state: Admin_InstanceDrainInfoState, reasons: Array<string> | null, inflight: number } | null } };


export const DashboardDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Dashboard"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"window"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"servicesHistory"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"window"},"value":{"kind":"Variable","name":{"kind":"Name","value":"window"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"window"}},{"kind":"Field","name":{"kind":"Name","value":"services"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"buckets"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"startUnixSec"}},{"kind":"Field","name":{"kind":"Name","value":"durationSec"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"p50Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p95Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p99Millis"}}]}}]}}]}}]}}]}}]} as unknown as DocumentNode<DashboardQuery, DashboardQueryVariables>;
export const WhoamiDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Whoami"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"whoami"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"role"}}]}}]}}]}}]} as unknown as DocumentNode<WhoamiQuery, WhoamiQueryVariables>;
export const EnvironmentDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Environment"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"listServices"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"environment"}}]}}]}}]}}]} as unknown as DocumentNode<EnvironmentQuery, EnvironmentQueryVariables>;
export const ServicesDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Services"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"listServices"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"services"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"public"}},{"kind":"Field","name":{"kind":"Name","value":"hashHex"}},{"kind":"Field","name":{"kind":"Name","value":"replicaCount"}},{"kind":"Field","name":{"kind":"Name","value":"manualDeprecationReason"}}]}},{"kind":"Field","name":{"kind":"Name","value":"stableVN"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"vN"}}]}}]}},{"kind":"Field","name":{"kind":"Name","value":"servicesStats"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"window"},"value":{"kind":"StringValue","value":"24h","block":false}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"window"}},{"kind":"Field","name":{"kind":"Name","value":"services"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"throughput"}},{"kind":"Field","name":{"kind":"Name","value":"p50Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p95Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p99Millis"}}]}}]}}]}}]}}]} as unknown as DocumentNode<ServicesQuery, ServicesQueryVariables>;
export const ServiceStatsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"ServiceStats"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"version"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"serviceStats"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"namespace"},"value":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}}},{"kind":"Argument","name":{"kind":"Name","value":"version"},"value":{"kind":"Variable","name":{"kind":"Name","value":"version"}}},{"kind":"Argument","name":{"kind":"Name","value":"window"},"value":{"kind":"StringValue","value":"24h","block":false}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"window"}},{"kind":"Field","name":{"kind":"Name","value":"methods"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"method"}},{"kind":"Field","name":{"kind":"Name","value":"caller"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"throughput"}},{"kind":"Field","name":{"kind":"Name","value":"p50Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p95Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p99Millis"}}]}}]}}]}}]}}]} as unknown as DocumentNode<ServiceStatsQuery, ServiceStatsQueryVariables>;
export const DeprecatedStatsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"DeprecatedStats"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deprecatedStats"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"window"},"value":{"kind":"StringValue","value":"24h","block":false}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"window"}},{"kind":"Field","name":{"kind":"Name","value":"services"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"manualReason"}},{"kind":"Field","name":{"kind":"Name","value":"autoReason"}},{"kind":"Field","name":{"kind":"Name","value":"totalCount"}},{"kind":"Field","name":{"kind":"Name","value":"totalThroughput"}},{"kind":"Field","name":{"kind":"Name","value":"methods"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"method"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"throughput"}},{"kind":"Field","name":{"kind":"Name","value":"p50Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p95Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p99Millis"}},{"kind":"Field","name":{"kind":"Name","value":"callers"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"caller"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"throughput"}},{"kind":"Field","name":{"kind":"Name","value":"p50Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p95Millis"}},{"kind":"Field","name":{"kind":"Name","value":"p99Millis"}}]}}]}}]}}]}}]}}]}}]} as unknown as DocumentNode<DeprecatedStatsQuery, DeprecatedStatsQueryVariables>;
export const DeprecateDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"Deprecate"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"version"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"reason"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deprecate"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"namespace"},"value":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}}},{"kind":"Argument","name":{"kind":"Name","value":"version"},"value":{"kind":"Variable","name":{"kind":"Name","value":"version"}}},{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"reason"},"value":{"kind":"Variable","name":{"kind":"Name","value":"reason"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"__typename"}}]}}]}}]}}]} as unknown as DocumentNode<DeprecateMutation, DeprecateMutationVariables>;
export const UndeprecateDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"Undeprecate"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"version"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"undeprecate"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"namespace"},"value":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}}},{"kind":"Argument","name":{"kind":"Name","value":"version"},"value":{"kind":"Variable","name":{"kind":"Name","value":"version"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"priorReason"}}]}}]}}]}}]} as unknown as DocumentNode<UndeprecateMutation, UndeprecateMutationVariables>;
export const RetractStableDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"RetractStable"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"targetVN"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"Int"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"retractStable"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"namespace"},"value":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}}},{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"targetVN"},"value":{"kind":"Variable","name":{"kind":"Name","value":"targetVN"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"priorVN"}},{"kind":"Field","name":{"kind":"Name","value":"newVN"}}]}}]}}]}}]} as unknown as DocumentNode<RetractStableMutation, RetractStableMutationVariables>;
export const SignSubscriptionTokenDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"SignSubscriptionToken"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"channel"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"ttlSeconds"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"Long"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"kid"}},"type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"signSubscriptionToken"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"channel"},"value":{"kind":"Variable","name":{"kind":"Name","value":"channel"}}},{"kind":"ObjectField","name":{"kind":"Name","value":"ttlSeconds"},"value":{"kind":"Variable","name":{"kind":"Name","value":"ttlSeconds"}}},{"kind":"ObjectField","name":{"kind":"Name","value":"kid"},"value":{"kind":"Variable","name":{"kind":"Name","value":"kid"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"code"}},{"kind":"Field","name":{"kind":"Name","value":"hmac"}},{"kind":"Field","name":{"kind":"Name","value":"kid"}},{"kind":"Field","name":{"kind":"Name","value":"reason"}},{"kind":"Field","name":{"kind":"Name","value":"timestampUnix"}}]}}]}}]}}]} as unknown as DocumentNode<SignSubscriptionTokenMutation, SignSubscriptionTokenMutationVariables>;
export const PeersDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Peers"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"listPeers"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"peers"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"nodeId"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"joinedUnixMs"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"minLevel"}},{"kind":"Field","name":{"kind":"Name","value":"maxLevel"}}]}},{"kind":"Field","name":{"kind":"Name","value":"activeLevel"}},{"kind":"Field","name":{"kind":"Name","value":"metaLeader"}},{"kind":"Field","name":{"kind":"Name","value":"metaSize"}},{"kind":"Field","name":{"kind":"Name","value":"metaOffline"}}]}}]}}]}}]} as unknown as DocumentNode<PeersQuery, PeersQueryVariables>;
export const InjectorsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Injectors"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"listInjectors"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"injectors"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"typeName"}},{"kind":"Field","name":{"kind":"Name","value":"path"}},{"kind":"Field","name":{"kind":"Name","value":"headerName"}},{"kind":"Field","name":{"kind":"Name","value":"hide"}},{"kind":"Field","name":{"kind":"Name","value":"nullable"}},{"kind":"Field","name":{"kind":"Name","value":"state"}},{"kind":"Field","name":{"kind":"Name","value":"registeredAt"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"file"}},{"kind":"Field","name":{"kind":"Name","value":"line"}},{"kind":"Field","name":{"kind":"Name","value":"function"}}]}},{"kind":"Field","name":{"kind":"Name","value":"landings"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"op"}},{"kind":"Field","name":{"kind":"Name","value":"typeName"}},{"kind":"Field","name":{"kind":"Name","value":"fieldName"}},{"kind":"Field","name":{"kind":"Name","value":"argName"}},{"kind":"Field","name":{"kind":"Name","value":"headerName"}}]}}]}}]}}]}}]}}]} as unknown as DocumentNode<InjectorsQuery, InjectorsQueryVariables>;
export const ForgetPeerDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"ForgetPeer"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"nodeId"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"forgetPeer"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"nodeId"},"value":{"kind":"Variable","name":{"kind":"Name","value":"nodeId"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"removed"}},{"kind":"Field","name":{"kind":"Name","value":"newReplicas"}}]}}]}}]}}]} as unknown as DocumentNode<ForgetPeerMutation, ForgetPeerMutationVariables>;
export const McpConfigDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"McpConfig"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpList"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}},{"kind":"Field","name":{"kind":"Name","value":"mcpSchemaList"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"entries"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"path"}},{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"description"}}]}}]}}]}}]}}]} as unknown as DocumentNode<McpConfigQuery, McpConfigQueryVariables>;
export const McpIncludeDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"McpInclude"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"path"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpInclude"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"path"},"value":{"kind":"Variable","name":{"kind":"Name","value":"path"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}}]}}]}}]} as unknown as DocumentNode<McpIncludeMutation, McpIncludeMutationVariables>;
export const McpExcludeDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"McpExclude"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"path"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpExclude"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"path"},"value":{"kind":"Variable","name":{"kind":"Name","value":"path"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}}]}}]}}]} as unknown as DocumentNode<McpExcludeMutation, McpExcludeMutationVariables>;
export const McpIncludeRemoveDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"McpIncludeRemove"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"path"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpIncludeRemove"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"path"},"value":{"kind":"Variable","name":{"kind":"Name","value":"path"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}}]}}]}}]} as unknown as DocumentNode<McpIncludeRemoveMutation, McpIncludeRemoveMutationVariables>;
export const McpExcludeRemoveDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"McpExcludeRemove"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"path"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpExcludeRemove"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"path"},"value":{"kind":"Variable","name":{"kind":"Name","value":"path"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}}]}}]}}]} as unknown as DocumentNode<McpExcludeRemoveMutation, McpExcludeRemoveMutationVariables>;
export const McpSetAutoIncludeDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"McpSetAutoInclude"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"autoInclude"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"Boolean"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"mcpSetAutoInclude"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"autoInclude"},"value":{"kind":"Variable","name":{"kind":"Name","value":"autoInclude"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"autoInclude"}},{"kind":"Field","name":{"kind":"Name","value":"include"}},{"kind":"Field","name":{"kind":"Name","value":"exclude"}}]}}]}}]}}]} as unknown as DocumentNode<McpSetAutoIncludeMutation, McpSetAutoIncludeMutationVariables>;
export const AdminEventsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"subscription","name":{"kind":"Name","value":"AdminEvents"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}},"type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin_events_watchServices"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"namespace"},"value":{"kind":"Variable","name":{"kind":"Name","value":"namespace"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"action"}},{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"addr"}},{"kind":"Field","name":{"kind":"Name","value":"timestampUnixMs"}},{"kind":"Field","name":{"kind":"Name","value":"replicaCount"}}]}}]}}]} as unknown as DocumentNode<AdminEventsSubscription, AdminEventsSubscriptionVariables>;
export const DependencyGraphDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"DependencyGraph"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"window"}},"type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"dependencyGraph"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"window"},"value":{"kind":"Variable","name":{"kind":"Name","value":"window"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"environment"}},{"kind":"Field","name":{"kind":"Name","value":"window"}},{"kind":"Field","name":{"kind":"Name","value":"scope"}},{"kind":"Field","name":{"kind":"Name","value":"callerSource"}},{"kind":"Field","name":{"kind":"Name","value":"nodes"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"public"}},{"kind":"Field","name":{"kind":"Name","value":"owner"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"team"}},{"kind":"Field","name":{"kind":"Name","value":"contact"}}]}},{"kind":"Field","name":{"kind":"Name","value":"replicaCount"}},{"kind":"Field","name":{"kind":"Name","value":"readyCount"}},{"kind":"Field","name":{"kind":"Name","value":"waitingCount"}},{"kind":"Field","name":{"kind":"Name","value":"degradedCount"}},{"kind":"Field","name":{"kind":"Name","value":"drainingCount"}},{"kind":"Field","name":{"kind":"Name","value":"trafficCount"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenUnixMs"}},{"kind":"Field","name":{"kind":"Name","value":"sunsetable"}},{"kind":"Field","name":{"kind":"Name","value":"sunsetReason"}},{"kind":"Field","name":{"kind":"Name","value":"instances"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"registrationId"}},{"kind":"Field","name":{"kind":"Name","value":"instanceId"}},{"kind":"Field","name":{"kind":"Name","value":"addr"}},{"kind":"Field","name":{"kind":"Name","value":"state"}},{"kind":"Field","name":{"kind":"Name","value":"drainState"}},{"kind":"Field","name":{"kind":"Name","value":"drainReasons"}},{"kind":"Field","name":{"kind":"Name","value":"verification"}},{"kind":"Field","name":{"kind":"Name","value":"verifyReasons"}}]}}]}},{"kind":"Field","name":{"kind":"Name","value":"desiredEdges"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"consumerKind"}},{"kind":"Field","name":{"kind":"Name","value":"consumerNamespace"}},{"kind":"Field","name":{"kind":"Name","value":"consumerVersion"}},{"kind":"Field","name":{"kind":"Name","value":"provider"}},{"kind":"Field","name":{"kind":"Name","value":"lines"}},{"kind":"Field","name":{"kind":"Name","value":"presentLines"}},{"kind":"Field","name":{"kind":"Name","value":"met"}},{"kind":"Field","name":{"kind":"Name","value":"instanceCount"}}]}},{"kind":"Field","name":{"kind":"Name","value":"activeEdges"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"caller"}},{"kind":"Field","name":{"kind":"Name","value":"consumerNamespace"}},{"kind":"Field","name":{"kind":"Name","value":"consumerVersion"}},{"kind":"Field","name":{"kind":"Name","value":"namespace"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenUnixMs"}},{"kind":"Field","name":{"kind":"Name","value":"methods"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"method"}},{"kind":"Field","name":{"kind":"Name","value":"count"}},{"kind":"Field","name":{"kind":"Name","value":"okCount"}}]}}]}},{"kind":"Field","name":{"kind":"Name","value":"externalDependents"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"owner"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"team"}},{"kind":"Field","name":{"kind":"Name","value":"contact"}}]}},{"kind":"Field","name":{"kind":"Name","value":"depends"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"service"}},{"kind":"Field","name":{"kind":"Name","value":"lines"}}]}},{"kind":"Field","name":{"kind":"Name","value":"note"}},{"kind":"Field","name":{"kind":"Name","value":"reviewAt"}},{"kind":"Field","name":{"kind":"Name","value":"reviewDue"}},{"kind":"Field","name":{"kind":"Name","value":"reviewedAt"}},{"kind":"Field","name":{"kind":"Name","value":"reviewedBy"}},{"kind":"Field","name":{"kind":"Name","value":"updatedUnixMs"}},{"kind":"Field","name":{"kind":"Name","value":"updatedBy"}}]}}]}}]}}]}}]} as unknown as DocumentNode<DependencyGraphQuery, DependencyGraphQueryVariables>;
export const PutExternalDependentDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"PutExternalDependent"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"body"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"admin_PutExternalDependentInBodyInput"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"putExternalDependent"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}},{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"Variable","name":{"kind":"Name","value":"body"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"externalDependent"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}}]}},{"kind":"Field","name":{"kind":"Name","value":"warnings"}}]}}]}}]}}]} as unknown as DocumentNode<PutExternalDependentMutation, PutExternalDependentMutationVariables>;
export const ReviewExternalDependentDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"ReviewExternalDependent"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"body"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"admin_ReviewExternalDependentInBodyInput"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"reviewExternalDependent"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}},{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"Variable","name":{"kind":"Name","value":"body"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"externalDependent"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"reviewAt"}}]}},{"kind":"Field","name":{"kind":"Name","value":"warnings"}}]}}]}}]}}]} as unknown as DocumentNode<ReviewExternalDependentMutation, ReviewExternalDependentMutationVariables>;
export const NotificationsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Notifications"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"listNotifications"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"items"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"kind"}},{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"title"}},{"kind":"Field","name":{"kind":"Name","value":"detail"}},{"kind":"Field","name":{"kind":"Name","value":"since"}},{"kind":"Field","name":{"kind":"Name","value":"href"}},{"kind":"Field","name":{"kind":"Name","value":"dismissable"}}]}}]}}]}}]}}]} as unknown as DocumentNode<NotificationsQuery, NotificationsQueryVariables>;
export const DismissNotificationDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DismissNotification"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"dismissNotification"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"dismissed"}}]}}]}}]}}]} as unknown as DocumentNode<DismissNotificationMutation, DismissNotificationMutationVariables>;
export const DeleteExternalDependentDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DeleteExternalDependent"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deleteExternalDependent"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deleted"}}]}}]}}]}}]} as unknown as DocumentNode<DeleteExternalDependentMutation, DeleteExternalDependentMutationVariables>;
export const DrainInstanceDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DrainInstance"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"instance"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"cancel"}},"type":{"kind":"NamedType","name":{"kind":"Name","value":"Boolean"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"admin"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"drainInstance"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"instance"},"value":{"kind":"Variable","name":{"kind":"Name","value":"instance"}}},{"kind":"Argument","name":{"kind":"Name","value":"body"},"value":{"kind":"ObjectValue","fields":[{"kind":"ObjectField","name":{"kind":"Name","value":"cancel"},"value":{"kind":"Variable","name":{"kind":"Name","value":"cancel"}}}]}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"registrationId"}},{"kind":"Field","name":{"kind":"Name","value":"state"}},{"kind":"Field","name":{"kind":"Name","value":"reasons"}},{"kind":"Field","name":{"kind":"Name","value":"inflight"}}]}}]}}]}}]} as unknown as DocumentNode<DrainInstanceMutation, DrainInstanceMutationVariables>;