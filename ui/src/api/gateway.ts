/** Internal type. DO NOT USE DIRECTLY. */
type Exact<T extends { [key: string]: unknown }> = { [K in keyof T]: T[K] };
/** Internal type. DO NOT USE DIRECTLY. */
export type Incremental<T> = T | { [P in keyof T]?: P extends ' $fragmentName' | '__typename' ? T[P] : never };
import type { GraphQLClient, RequestOptions } from 'graphql-request';
import gql from 'graphql-tag';
export type Maybe<T> = T | null;
export type InputMaybe<T> = Maybe<T>;
type GraphQLClientRequestHeaders = RequestOptions['requestHeaders'];
/** All built-in and custom scalars, mapped to their actual values */
export type Scalars = {
  ID: { input: string; output: string; }
  String: { input: string; output: string; }
  Boolean: { input: boolean; output: boolean; }
  Int: { input: number; output: number; }
  Float: { input: number; output: number; }
  /** Untyped JSON value (used as a fallback for OpenAPI schemas the gateway can't map exactly). */
  JSON: { input: unknown; output: unknown; }
  /** 64-bit integer encoded as a decimal string. OpenAPI integer fields with format=int64/uint64 land here; graphql-go's built-in Int is signed 32-bit and would lose precision (or null out entirely) for values above 2^31. */
  Long: { input: unknown; output: unknown; }
};

export type AdminMutationNamespace = {
  __typename?: 'AdminMutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
  mcpExclude: Maybe<Admin_McpListOutBody>;
  mcpInclude: Maybe<Admin_McpListOutBody>;
  mcpQuery: Maybe<Admin_McpQueryOutBody>;
  mcpSchemaExpand: Maybe<Admin_McpSchemaExpandOutBody>;
  mcpSchemaSearch: Maybe<Admin_McpSchemaSearchOutBody>;
  mcpSetAutoInclude: Maybe<Admin_McpListOutBody>;
  retractStable: Maybe<Admin_RetractStableOutBody>;
  signSubscriptionToken: Maybe<Admin_SignOutBody>;
  stable: AdminStableMutationNamespace;
  undeprecate: Maybe<Admin_UndeprecateOutBody>;
  v1: AdminV1MutationNamespace;
};


export type AdminMutationNamespaceDeprecateArgs = {
  body: Admin_DeprecateInBodyInput;
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};


export type AdminMutationNamespaceDrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type AdminMutationNamespaceForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type AdminMutationNamespaceMcpExcludeArgs = {
  body: Admin_McpExcludeInBodyInput;
};


export type AdminMutationNamespaceMcpIncludeArgs = {
  body: Admin_McpIncludeInBodyInput;
};


export type AdminMutationNamespaceMcpQueryArgs = {
  body: Admin_McpQueryInBodyInput;
};


export type AdminMutationNamespaceMcpSchemaExpandArgs = {
  body: Admin_McpSchemaExpandInBodyInput;
};


export type AdminMutationNamespaceMcpSchemaSearchArgs = {
  body: Admin_McpSchemaSearchInBodyInput;
};


export type AdminMutationNamespaceMcpSetAutoIncludeArgs = {
  body: Admin_McpSetAutoIncludeInBodyInput;
};


export type AdminMutationNamespaceRetractStableArgs = {
  body: Admin_RetractStableInBodyInput;
  namespace: Scalars['String']['input'];
};


export type AdminMutationNamespaceSignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};


export type AdminMutationNamespaceUndeprecateArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};

export type AdminQueryNamespace = {
  __typename?: 'AdminQueryNamespace';
  deprecatedStats: Maybe<Admin_DeprecatedStatsOutBody>;
  listChannels: Maybe<Admin_ChannelsOutBody>;
  listInjectors: Maybe<Admin_InjectorsOutBody>;
  listPeers: Maybe<Admin_PeersOutBody>;
  listServices: Maybe<Admin_ServicesOutBody>;
  mcpList: Maybe<Admin_McpListOutBody>;
  mcpSchemaList: Maybe<Admin_McpSchemaListOutBody>;
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
  servicesHistory: Maybe<Admin_ServicesHistoryOutBody>;
  servicesStats: Maybe<Admin_ServicesStatsOutBody>;
  stable: AdminStableQueryNamespace;
  v1: AdminV1QueryNamespace;
};


export type AdminQueryNamespaceDeprecatedStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminQueryNamespaceServiceStatsArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminQueryNamespaceServicesHistoryArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminQueryNamespaceServicesStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};

export type AdminStableMutationNamespace = {
  __typename?: 'AdminStableMutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
  mcpExclude: Maybe<Admin_McpListOutBody>;
  mcpInclude: Maybe<Admin_McpListOutBody>;
  mcpQuery: Maybe<Admin_McpQueryOutBody>;
  mcpSchemaExpand: Maybe<Admin_McpSchemaExpandOutBody>;
  mcpSchemaSearch: Maybe<Admin_McpSchemaSearchOutBody>;
  mcpSetAutoInclude: Maybe<Admin_McpListOutBody>;
  retractStable: Maybe<Admin_RetractStableOutBody>;
  signSubscriptionToken: Maybe<Admin_SignOutBody>;
  undeprecate: Maybe<Admin_UndeprecateOutBody>;
};


export type AdminStableMutationNamespaceDeprecateArgs = {
  body: Admin_DeprecateInBodyInput;
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};


export type AdminStableMutationNamespaceDrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type AdminStableMutationNamespaceForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type AdminStableMutationNamespaceMcpExcludeArgs = {
  body: Admin_McpExcludeInBodyInput;
};


export type AdminStableMutationNamespaceMcpIncludeArgs = {
  body: Admin_McpIncludeInBodyInput;
};


export type AdminStableMutationNamespaceMcpQueryArgs = {
  body: Admin_McpQueryInBodyInput;
};


export type AdminStableMutationNamespaceMcpSchemaExpandArgs = {
  body: Admin_McpSchemaExpandInBodyInput;
};


export type AdminStableMutationNamespaceMcpSchemaSearchArgs = {
  body: Admin_McpSchemaSearchInBodyInput;
};


export type AdminStableMutationNamespaceMcpSetAutoIncludeArgs = {
  body: Admin_McpSetAutoIncludeInBodyInput;
};


export type AdminStableMutationNamespaceRetractStableArgs = {
  body: Admin_RetractStableInBodyInput;
  namespace: Scalars['String']['input'];
};


export type AdminStableMutationNamespaceSignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};


export type AdminStableMutationNamespaceUndeprecateArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};

export type AdminStableQueryNamespace = {
  __typename?: 'AdminStableQueryNamespace';
  deprecatedStats: Maybe<Admin_DeprecatedStatsOutBody>;
  listChannels: Maybe<Admin_ChannelsOutBody>;
  listInjectors: Maybe<Admin_InjectorsOutBody>;
  listPeers: Maybe<Admin_PeersOutBody>;
  listServices: Maybe<Admin_ServicesOutBody>;
  mcpList: Maybe<Admin_McpListOutBody>;
  mcpSchemaList: Maybe<Admin_McpSchemaListOutBody>;
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
  servicesHistory: Maybe<Admin_ServicesHistoryOutBody>;
  servicesStats: Maybe<Admin_ServicesStatsOutBody>;
};


export type AdminStableQueryNamespaceDeprecatedStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminStableQueryNamespaceServiceStatsArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminStableQueryNamespaceServicesHistoryArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminStableQueryNamespaceServicesStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};

export type AdminV1MutationNamespace = {
  __typename?: 'AdminV1MutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
  mcpExclude: Maybe<Admin_McpListOutBody>;
  mcpInclude: Maybe<Admin_McpListOutBody>;
  mcpQuery: Maybe<Admin_McpQueryOutBody>;
  mcpSchemaExpand: Maybe<Admin_McpSchemaExpandOutBody>;
  mcpSchemaSearch: Maybe<Admin_McpSchemaSearchOutBody>;
  mcpSetAutoInclude: Maybe<Admin_McpListOutBody>;
  retractStable: Maybe<Admin_RetractStableOutBody>;
  signSubscriptionToken: Maybe<Admin_SignOutBody>;
  undeprecate: Maybe<Admin_UndeprecateOutBody>;
};


export type AdminV1MutationNamespaceDeprecateArgs = {
  body: Admin_DeprecateInBodyInput;
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};


export type AdminV1MutationNamespaceDrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type AdminV1MutationNamespaceForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type AdminV1MutationNamespaceMcpExcludeArgs = {
  body: Admin_McpExcludeInBodyInput;
};


export type AdminV1MutationNamespaceMcpIncludeArgs = {
  body: Admin_McpIncludeInBodyInput;
};


export type AdminV1MutationNamespaceMcpQueryArgs = {
  body: Admin_McpQueryInBodyInput;
};


export type AdminV1MutationNamespaceMcpSchemaExpandArgs = {
  body: Admin_McpSchemaExpandInBodyInput;
};


export type AdminV1MutationNamespaceMcpSchemaSearchArgs = {
  body: Admin_McpSchemaSearchInBodyInput;
};


export type AdminV1MutationNamespaceMcpSetAutoIncludeArgs = {
  body: Admin_McpSetAutoIncludeInBodyInput;
};


export type AdminV1MutationNamespaceRetractStableArgs = {
  body: Admin_RetractStableInBodyInput;
  namespace: Scalars['String']['input'];
};


export type AdminV1MutationNamespaceSignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};


export type AdminV1MutationNamespaceUndeprecateArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
};

export type AdminV1QueryNamespace = {
  __typename?: 'AdminV1QueryNamespace';
  deprecatedStats: Maybe<Admin_DeprecatedStatsOutBody>;
  listChannels: Maybe<Admin_ChannelsOutBody>;
  listInjectors: Maybe<Admin_InjectorsOutBody>;
  listPeers: Maybe<Admin_PeersOutBody>;
  listServices: Maybe<Admin_ServicesOutBody>;
  mcpList: Maybe<Admin_McpListOutBody>;
  mcpSchemaList: Maybe<Admin_McpSchemaListOutBody>;
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
  servicesHistory: Maybe<Admin_ServicesHistoryOutBody>;
  servicesStats: Maybe<Admin_ServicesStatsOutBody>;
};


export type AdminV1QueryNamespaceDeprecatedStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminV1QueryNamespaceServiceStatsArgs = {
  namespace: Scalars['String']['input'];
  version: Scalars['String']['input'];
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminV1QueryNamespaceServicesHistoryArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};


export type AdminV1QueryNamespaceServicesStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};

export type Mutation = {
  __typename?: 'Mutation';
  admin: AdminMutationNamespace;
};

export type Query = {
  __typename?: 'Query';
  admin: AdminQueryNamespace;
};

export type Admin_CallerStatsRow = {
  __typename?: 'admin_CallerStatsRow';
  caller: Scalars['String']['output'];
  count: Scalars['Long']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
  p99Millis: Scalars['Long']['output'];
  throughput: Scalars['Float']['output'];
};

export type Admin_ChannelInfo = {
  __typename?: 'admin_ChannelInfo';
  consumers: Scalars['Long']['output'];
  subject: Scalars['String']['output'];
};

export type Admin_ChannelsOutBody = {
  __typename?: 'admin_ChannelsOutBody';
  channels: Array<Maybe<Admin_ChannelInfo>>;
};

export type Admin_DeprecateInBodyInput = {
  reason: Scalars['String']['input'];
};

export type Admin_DeprecateOutBody = {
  __typename?: 'admin_DeprecateOutBody';
  _void: Maybe<Scalars['String']['output']>;
};

export type Admin_DeprecatedMethodRow = {
  __typename?: 'admin_DeprecatedMethodRow';
  callers: Array<Maybe<Admin_CallerStatsRow>>;
  count: Scalars['Long']['output'];
  method: Scalars['String']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
  p99Millis: Scalars['Long']['output'];
  throughput: Scalars['Float']['output'];
};

export type Admin_DeprecatedServiceRow = {
  __typename?: 'admin_DeprecatedServiceRow';
  autoReason: Maybe<Scalars['String']['output']>;
  manualReason: Maybe<Scalars['String']['output']>;
  methods: Array<Maybe<Admin_DeprecatedMethodRow>>;
  namespace: Scalars['String']['output'];
  totalCount: Scalars['Long']['output'];
  totalThroughput: Scalars['Float']['output'];
  version: Scalars['String']['output'];
};

export type Admin_DeprecatedStatsOutBody = {
  __typename?: 'admin_DeprecatedStatsOutBody';
  services: Array<Maybe<Admin_DeprecatedServiceRow>>;
  window: Scalars['String']['output'];
};

export type Admin_DrainInBodyInput = {
  timeoutSeconds: InputMaybe<Scalars['Long']['input']>;
};

export type Admin_DrainOutBody = {
  __typename?: 'admin_DrainOutBody';
  activeStreams: Scalars['Long']['output'];
  drained: Scalars['Boolean']['output'];
  reason: Maybe<Scalars['String']['output']>;
};

export type Admin_ForgetOutBody = {
  __typename?: 'admin_ForgetOutBody';
  newReplicas: Scalars['Int']['output'];
  removed: Scalars['Boolean']['output'];
};

export type Admin_HistoryBucketOut = {
  __typename?: 'admin_HistoryBucketOut';
  count: Scalars['Long']['output'];
  durationSec: Scalars['Long']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
  p99Millis: Scalars['Long']['output'];
  startUnixSec: Scalars['Long']['output'];
};

export type Admin_InjectorInfo = {
  __typename?: 'admin_InjectorInfo';
  headerName: Maybe<Scalars['String']['output']>;
  hide: Scalars['Boolean']['output'];
  kind: Scalars['String']['output'];
  landings: Array<Maybe<Admin_InjectorLandingInfo>>;
  nullable: Scalars['Boolean']['output'];
  path: Maybe<Scalars['String']['output']>;
  registeredAt: Admin_RegisteredAtInfo;
  state: Scalars['String']['output'];
  typeName: Maybe<Scalars['String']['output']>;
};

export type Admin_InjectorLandingInfo = {
  __typename?: 'admin_InjectorLandingInfo';
  argName: Maybe<Scalars['String']['output']>;
  fieldName: Maybe<Scalars['String']['output']>;
  headerName: Maybe<Scalars['String']['output']>;
  kind: Scalars['String']['output'];
  namespace: Maybe<Scalars['String']['output']>;
  op: Maybe<Scalars['String']['output']>;
  typeName: Maybe<Scalars['String']['output']>;
  version: Maybe<Scalars['String']['output']>;
};

export type Admin_InjectorsOutBody = {
  __typename?: 'admin_InjectorsOutBody';
  injectors: Array<Maybe<Admin_InjectorInfo>>;
};

export type Admin_McpChannelEvent = {
  __typename?: 'admin_MCPChannelEvent';
  channel: Scalars['String']['output'];
  count: Scalars['Long']['output'];
  preview: Maybe<Scalars['JSON']['output']>;
};

export type Admin_McpEventsBundle = {
  __typename?: 'admin_MCPEventsBundle';
  channels: Array<Maybe<Admin_McpChannelEvent>>;
  level: Scalars['String']['output'];
};

export type Admin_McpResponseWithEvents = {
  __typename?: 'admin_MCPResponseWithEvents';
  events: Admin_McpEventsBundle;
  response: Scalars['JSON']['output'];
};

export type Admin_McpExcludeInBodyInput = {
  path: Scalars['String']['input'];
};

export type Admin_McpIncludeInBodyInput = {
  path: Scalars['String']['input'];
};

export type Admin_McpListOutBody = {
  __typename?: 'admin_McpListOutBody';
  autoInclude: Scalars['Boolean']['output'];
  exclude: Array<Maybe<Scalars['String']['output']>>;
  include: Array<Maybe<Scalars['String']['output']>>;
};

export type Admin_McpQueryInBodyInput = {
  operationName: InputMaybe<Scalars['String']['input']>;
  query: Scalars['String']['input'];
  variables: InputMaybe<Scalars['JSON']['input']>;
};

export type Admin_McpQueryOutBody = {
  __typename?: 'admin_McpQueryOutBody';
  result: Admin_McpResponseWithEvents;
};

export type Admin_McpSchemaExpandInBodyInput = {
  name: Scalars['String']['input'];
};

export type Admin_McpSchemaExpandOutBody = {
  __typename?: 'admin_McpSchemaExpandOutBody';
  result: Admin_SchemaExpandResult;
};

export type Admin_McpSchemaListOutBody = {
  __typename?: 'admin_McpSchemaListOutBody';
  entries: Array<Maybe<Admin_SchemaListEntry>>;
};

export type Admin_McpSchemaSearchInBodyInput = {
  pathGlob: InputMaybe<Scalars['String']['input']>;
  regex: InputMaybe<Scalars['String']['input']>;
};

export type Admin_McpSchemaSearchOutBody = {
  __typename?: 'admin_McpSchemaSearchOutBody';
  entries: Array<Maybe<Admin_SchemaSearchEntry>>;
};

export type Admin_McpSetAutoIncludeInBodyInput = {
  autoInclude: Scalars['Boolean']['input'];
};

export type Admin_MethodStatsOut = {
  __typename?: 'admin_MethodStatsOut';
  caller: Scalars['String']['output'];
  count: Scalars['Long']['output'];
  method: Scalars['String']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
  p99Millis: Scalars['Long']['output'];
  throughput: Scalars['Float']['output'];
};

export type Admin_PeerInfo = {
  __typename?: 'admin_PeerInfo';
  joinedUnixMs: Scalars['Long']['output'];
  name: Maybe<Scalars['String']['output']>;
  nodeId: Scalars['String']['output'];
};

export type Admin_PeersOutBody = {
  __typename?: 'admin_PeersOutBody';
  peers: Array<Maybe<Admin_PeerInfo>>;
};

export type Admin_RegisteredAtInfo = {
  __typename?: 'admin_RegisteredAtInfo';
  file: Maybe<Scalars['String']['output']>;
  function: Maybe<Scalars['String']['output']>;
  line: Maybe<Scalars['Long']['output']>;
};

export type Admin_RejectedJoinInfo = {
  __typename?: 'admin_RejectedJoinInfo';
  count: Scalars['Int']['output'];
  currentMaxConcurrency: Scalars['Long']['output'];
  currentMaxConcurrencyPerInstance: Scalars['Long']['output'];
  lastMaxConcurrency: Scalars['Long']['output'];
  lastMaxConcurrencyPerInstance: Scalars['Long']['output'];
  lastReason: Scalars['String']['output'];
  lastUnixMs: Scalars['Long']['output'];
};

export type Admin_RetractStableInBodyInput = {
  targetVN: Scalars['Int']['input'];
};

export type Admin_RetractStableOutBody = {
  __typename?: 'admin_RetractStableOutBody';
  newVN: Scalars['Int']['output'];
  priorVN: Scalars['Int']['output'];
};

export type Admin_SchemaExpandEnumValue = {
  __typename?: 'admin_SchemaExpandEnumValue';
  deprecated: Maybe<Scalars['String']['output']>;
  description: Maybe<Scalars['String']['output']>;
  name: Scalars['String']['output'];
};

export type Admin_SchemaExpandField = {
  __typename?: 'admin_SchemaExpandField';
  deprecated: Maybe<Scalars['String']['output']>;
  description: Maybe<Scalars['String']['output']>;
  name: Scalars['String']['output'];
  required: Scalars['Boolean']['output'];
  type: Scalars['String']['output'];
};

export type Admin_SchemaExpandOp = {
  __typename?: 'admin_SchemaExpandOp';
  args: Array<Maybe<Admin_SchemaSearchArg>>;
  deprecated: Maybe<Scalars['String']['output']>;
  description: Maybe<Scalars['String']['output']>;
  kind: Scalars['String']['output'];
  namespace: Scalars['String']['output'];
  outputType: Maybe<Scalars['String']['output']>;
  path: Scalars['String']['output'];
  version: Scalars['String']['output'];
};

export type Admin_SchemaExpandResult = {
  __typename?: 'admin_SchemaExpandResult';
  op: Maybe<Admin_SchemaExpandOp>;
  path: Maybe<Scalars['String']['output']>;
  type: Maybe<Admin_SchemaExpandType>;
  types: Array<Maybe<Admin_SchemaExpandType>>;
};

export type Admin_SchemaExpandType = {
  __typename?: 'admin_SchemaExpandType';
  description: Maybe<Scalars['String']['output']>;
  enumValues: Maybe<Array<Maybe<Admin_SchemaExpandEnumValue>>>;
  fields: Maybe<Array<Maybe<Admin_SchemaExpandField>>>;
  kind: Scalars['String']['output'];
  name: Scalars['String']['output'];
  variants: Maybe<Array<Maybe<Scalars['String']['output']>>>;
};

export type Admin_SchemaListEntry = {
  __typename?: 'admin_SchemaListEntry';
  description: Maybe<Scalars['String']['output']>;
  kind: Scalars['String']['output'];
  namespace: Scalars['String']['output'];
  path: Scalars['String']['output'];
  version: Scalars['String']['output'];
};

export type Admin_SchemaSearchArg = {
  __typename?: 'admin_SchemaSearchArg';
  description: Maybe<Scalars['String']['output']>;
  name: Scalars['String']['output'];
  required: Scalars['Boolean']['output'];
  type: Scalars['String']['output'];
};

export type Admin_SchemaSearchEntry = {
  __typename?: 'admin_SchemaSearchEntry';
  args: Array<Maybe<Admin_SchemaSearchArg>>;
  description: Maybe<Scalars['String']['output']>;
  kind: Scalars['String']['output'];
  namespace: Scalars['String']['output'];
  outputType: Maybe<Scalars['String']['output']>;
  path: Scalars['String']['output'];
  version: Scalars['String']['output'];
};

export type Admin_ServiceHistoryRow = {
  __typename?: 'admin_ServiceHistoryRow';
  buckets: Array<Maybe<Admin_HistoryBucketOut>>;
  namespace: Scalars['String']['output'];
  version: Scalars['String']['output'];
};

export type Admin_ServiceInfo = {
  __typename?: 'admin_ServiceInfo';
  hashHex: Scalars['String']['output'];
  manualDeprecationReason: Maybe<Scalars['String']['output']>;
  namespace: Scalars['String']['output'];
  rejectedJoins: Maybe<Admin_RejectedJoinInfo>;
  replicaCount: Scalars['Int']['output'];
  version: Scalars['String']['output'];
};

export type Admin_ServiceStatsOutBody = {
  __typename?: 'admin_ServiceStatsOutBody';
  methods: Array<Maybe<Admin_MethodStatsOut>>;
  window: Scalars['String']['output'];
};

export type Admin_ServiceStatsRow = {
  __typename?: 'admin_ServiceStatsRow';
  count: Scalars['Long']['output'];
  namespace: Scalars['String']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
  p99Millis: Scalars['Long']['output'];
  throughput: Scalars['Float']['output'];
  version: Scalars['String']['output'];
};

export type Admin_ServicesHistoryOutBody = {
  __typename?: 'admin_ServicesHistoryOutBody';
  services: Array<Maybe<Admin_ServiceHistoryRow>>;
  window: Scalars['String']['output'];
};

export type Admin_ServicesOutBody = {
  __typename?: 'admin_ServicesOutBody';
  services: Array<Maybe<Admin_ServiceInfo>>;
  stableVN: Array<Maybe<Admin_StableVnEntry>>;
};

export type Admin_ServicesStatsOutBody = {
  __typename?: 'admin_ServicesStatsOutBody';
  services: Array<Maybe<Admin_ServiceStatsRow>>;
  window: Scalars['String']['output'];
};

export type Admin_SignInBodyInput = {
  channel: Scalars['String']['input'];
  kid: InputMaybe<Scalars['String']['input']>;
  ttlSeconds: Scalars['Long']['input'];
};

export type Admin_SignOutBody = {
  __typename?: 'admin_SignOutBody';
  code: Scalars['String']['output'];
  hmac: Maybe<Scalars['String']['output']>;
  kid: Maybe<Scalars['String']['output']>;
  reason: Maybe<Scalars['String']['output']>;
  timestampUnix: Maybe<Scalars['Long']['output']>;
};

export type Admin_StableVnEntry = {
  __typename?: 'admin_StableVNEntry';
  namespace: Scalars['String']['output'];
  vN: Scalars['Int']['output'];
};

export type Admin_UndeprecateOutBody = {
  __typename?: 'admin_UndeprecateOutBody';
  priorReason: Scalars['String']['output'];
};

export type DashboardQueryVariables = Exact<{
  window: string;
}>;


export type DashboardQuery = { admin: { servicesHistory: { window: string, services: Array<{ namespace: string, version: string, buckets: Array<{ startUnixSec: unknown, durationSec: unknown, count: unknown, okCount: unknown, p50Millis: unknown, p95Millis: unknown, p99Millis: unknown } | null> } | null> } | null } };

export type ServicesQueryVariables = Exact<{ [key: string]: never; }>;


export type ServicesQuery = { admin: { listServices: { services: Array<{ namespace: string, version: string, hashHex: string, replicaCount: number, manualDeprecationReason: string | null } | null>, stableVN: Array<{ namespace: string, vN: number } | null> } | null, servicesStats: { window: string, services: Array<{ namespace: string, version: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown, p99Millis: unknown } | null> } | null } };

export type ServiceStatsQueryVariables = Exact<{
  namespace: string;
  version: string;
}>;


export type ServiceStatsQuery = { admin: { serviceStats: { window: string, methods: Array<{ method: string, caller: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown, p99Millis: unknown } | null> } | null } };

export type DeprecatedStatsQueryVariables = Exact<{ [key: string]: never; }>;


export type DeprecatedStatsQuery = { admin: { deprecatedStats: { window: string, services: Array<{ namespace: string, version: string, manualReason: string | null, autoReason: string | null, totalCount: unknown, totalThroughput: number, methods: Array<{ method: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown, p99Millis: unknown, callers: Array<{ caller: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown, p99Millis: unknown } | null> } | null> } | null> } | null } };

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

export type PeersQueryVariables = Exact<{ [key: string]: never; }>;


export type PeersQuery = { admin: { listPeers: { peers: Array<{ nodeId: string, name: string | null, joinedUnixMs: unknown } | null> } | null } };

export type InjectorsQueryVariables = Exact<{ [key: string]: never; }>;


export type InjectorsQuery = { admin: { listInjectors: { injectors: Array<{ kind: string, typeName: string | null, path: string | null, headerName: string | null, hide: boolean, nullable: boolean, state: string, registeredAt: { file: string | null, line: unknown, function: string | null }, landings: Array<{ kind: string, namespace: string | null, version: string | null, op: string | null, typeName: string | null, fieldName: string | null, argName: string | null, headerName: string | null } | null> } | null> } | null } };

export type ForgetPeerMutationVariables = Exact<{
  nodeId: string;
}>;


export type ForgetPeerMutation = { admin: { forgetPeer: { removed: boolean, newReplicas: number } | null } };


export const DashboardDocument = gql`
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
    `;
export const ServicesDocument = gql`
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
    `;
export const ServiceStatsDocument = gql`
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
    `;
export const DeprecatedStatsDocument = gql`
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
    `;
export const DeprecateDocument = gql`
    mutation Deprecate($namespace: String!, $version: String!, $reason: String!) {
  admin {
    deprecate(namespace: $namespace, version: $version, body: {reason: $reason}) {
      __typename
    }
  }
}
    `;
export const UndeprecateDocument = gql`
    mutation Undeprecate($namespace: String!, $version: String!) {
  admin {
    undeprecate(namespace: $namespace, version: $version) {
      priorReason
    }
  }
}
    `;
export const RetractStableDocument = gql`
    mutation RetractStable($namespace: String!, $targetVN: Int!) {
  admin {
    retractStable(namespace: $namespace, body: {targetVN: $targetVN}) {
      priorVN
      newVN
    }
  }
}
    `;
export const PeersDocument = gql`
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
    `;
export const InjectorsDocument = gql`
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
    `;
export const ForgetPeerDocument = gql`
    mutation ForgetPeer($nodeId: String!) {
  admin {
    forgetPeer(nodeId: $nodeId) {
      removed
      newReplicas
    }
  }
}
    `;

export type SdkFunctionWrapper = <T>(action: (requestHeaders?:Record<string, string>) => Promise<T>, operationName: string, operationType?: string, variables?: any) => Promise<T>;


const defaultWrapper: SdkFunctionWrapper = (action, _operationName, _operationType, _variables) => action();

export function getSdk(client: GraphQLClient, withWrapper: SdkFunctionWrapper = defaultWrapper) {
  return {
    Dashboard(variables: DashboardQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<DashboardQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<DashboardQuery>({ document: DashboardDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Dashboard', 'query', variables);
    },
    Services(variables?: ServicesQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<ServicesQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<ServicesQuery>({ document: ServicesDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Services', 'query', variables);
    },
    ServiceStats(variables: ServiceStatsQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<ServiceStatsQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<ServiceStatsQuery>({ document: ServiceStatsDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'ServiceStats', 'query', variables);
    },
    DeprecatedStats(variables?: DeprecatedStatsQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<DeprecatedStatsQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<DeprecatedStatsQuery>({ document: DeprecatedStatsDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'DeprecatedStats', 'query', variables);
    },
    Deprecate(variables: DeprecateMutationVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<DeprecateMutation> {
      return withWrapper((wrappedRequestHeaders) => client.request<DeprecateMutation>({ document: DeprecateDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Deprecate', 'mutation', variables);
    },
    Undeprecate(variables: UndeprecateMutationVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<UndeprecateMutation> {
      return withWrapper((wrappedRequestHeaders) => client.request<UndeprecateMutation>({ document: UndeprecateDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Undeprecate', 'mutation', variables);
    },
    RetractStable(variables: RetractStableMutationVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<RetractStableMutation> {
      return withWrapper((wrappedRequestHeaders) => client.request<RetractStableMutation>({ document: RetractStableDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'RetractStable', 'mutation', variables);
    },
    Peers(variables?: PeersQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<PeersQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<PeersQuery>({ document: PeersDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Peers', 'query', variables);
    },
    Injectors(variables?: InjectorsQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<InjectorsQuery> {
      return withWrapper((wrappedRequestHeaders) => client.request<InjectorsQuery>({ document: InjectorsDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'Injectors', 'query', variables);
    },
    ForgetPeer(variables: ForgetPeerMutationVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<ForgetPeerMutation> {
      return withWrapper((wrappedRequestHeaders) => client.request<ForgetPeerMutation>({ document: ForgetPeerDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'ForgetPeer', 'mutation', variables);
    }
  };
}
export type Sdk = ReturnType<typeof getSdk>;