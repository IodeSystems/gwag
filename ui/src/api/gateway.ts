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
  /** 64-bit integer encoded as a decimal string. OpenAPI integer fields with format=int64/uint64 land here; graphql-go's built-in Int is signed 32-bit and would lose precision (or null out entirely) for values above 2^31. */
  Long: { input: unknown; output: unknown; }
};

export type AdminMutationNamespace = {
  __typename?: 'AdminMutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
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
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
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


export type AdminQueryNamespaceServicesStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};

export type AdminStableMutationNamespace = {
  __typename?: 'AdminStableMutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
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
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
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


export type AdminStableQueryNamespaceServicesStatsArgs = {
  window: InputMaybe<Scalars['String']['input']>;
};

export type AdminV1MutationNamespace = {
  __typename?: 'AdminV1MutationNamespace';
  deprecate: Maybe<Admin_DeprecateOutBody>;
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
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
  serviceStats: Maybe<Admin_ServiceStatsOutBody>;
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

export type Admin_MethodStatsOut = {
  __typename?: 'admin_MethodStatsOut';
  caller: Scalars['String']['output'];
  count: Scalars['Long']['output'];
  method: Scalars['String']['output'];
  okCount: Scalars['Long']['output'];
  p50Millis: Scalars['Long']['output'];
  p95Millis: Scalars['Long']['output'];
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

export type Admin_RetractStableInBodyInput = {
  targetVN: Scalars['Int']['input'];
};

export type Admin_RetractStableOutBody = {
  __typename?: 'admin_RetractStableOutBody';
  newVN: Scalars['Int']['output'];
  priorVN: Scalars['Int']['output'];
};

export type Admin_ServiceInfo = {
  __typename?: 'admin_ServiceInfo';
  hashHex: Scalars['String']['output'];
  manualDeprecationReason: Maybe<Scalars['String']['output']>;
  namespace: Scalars['String']['output'];
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
  throughput: Scalars['Float']['output'];
  version: Scalars['String']['output'];
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

export type DashboardQueryVariables = Exact<{ [key: string]: never; }>;


export type DashboardQuery = { admin: { listPeers: { peers: Array<{ nodeId: string } | null> } | null, listServices: { services: Array<{ namespace: string, version: string } | null> } | null } };

export type ServicesQueryVariables = Exact<{ [key: string]: never; }>;


export type ServicesQuery = { admin: { listServices: { services: Array<{ namespace: string, version: string, hashHex: string, replicaCount: number, manualDeprecationReason: string | null } | null>, stableVN: Array<{ namespace: string, vN: number } | null> } | null, servicesStats: { window: string, services: Array<{ namespace: string, version: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown } | null> } | null } };

export type ServiceStatsQueryVariables = Exact<{
  namespace: string;
  version: string;
}>;


export type ServiceStatsQuery = { admin: { serviceStats: { window: string, methods: Array<{ method: string, caller: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown } | null> } | null } };

export type DeprecatedStatsQueryVariables = Exact<{ [key: string]: never; }>;


export type DeprecatedStatsQuery = { admin: { deprecatedStats: { window: string, services: Array<{ namespace: string, version: string, manualReason: string | null, autoReason: string | null, totalCount: unknown, totalThroughput: number, methods: Array<{ method: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown, callers: Array<{ caller: string, count: unknown, okCount: unknown, throughput: number, p50Millis: unknown, p95Millis: unknown } | null> } | null> } | null> } | null } };

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
    query Dashboard {
  admin {
    listPeers {
      peers {
        nodeId
      }
    }
    listServices {
      services {
        namespace
        version
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
          callers {
            caller
            count
            okCount
            throughput
            p50Millis
            p95Millis
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
    Dashboard(variables?: DashboardQueryVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<DashboardQuery> {
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