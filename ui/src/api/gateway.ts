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
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
  signSubscriptionToken: Maybe<Admin_SignOutBody>;
  v1: AdminV1MutationNamespace;
};


export type AdminMutationNamespaceDrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type AdminMutationNamespaceForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type AdminMutationNamespaceSignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};

export type AdminQueryNamespace = {
  __typename?: 'AdminQueryNamespace';
  listChannels: Maybe<Admin_ChannelsOutBody>;
  listInjectors: Maybe<Admin_InjectorsOutBody>;
  listPeers: Maybe<Admin_PeersOutBody>;
  listServices: Maybe<Admin_ServicesOutBody>;
  v1: AdminV1QueryNamespace;
};

export type AdminV1MutationNamespace = {
  __typename?: 'AdminV1MutationNamespace';
  drain: Maybe<Admin_DrainOutBody>;
  forgetPeer: Maybe<Admin_ForgetOutBody>;
  signSubscriptionToken: Maybe<Admin_SignOutBody>;
};


export type AdminV1MutationNamespaceDrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type AdminV1MutationNamespaceForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type AdminV1MutationNamespaceSignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};

export type AdminV1QueryNamespace = {
  __typename?: 'AdminV1QueryNamespace';
  listChannels: Maybe<Admin_ChannelsOutBody>;
  listInjectors: Maybe<Admin_InjectorsOutBody>;
  listPeers: Maybe<Admin_PeersOutBody>;
  listServices: Maybe<Admin_ServicesOutBody>;
};

export type Mutation = {
  __typename?: 'Mutation';
  admin: AdminMutationNamespace;
};

export type Query = {
  __typename?: 'Query';
  admin: AdminQueryNamespace;
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

export type Admin_ServiceInfo = {
  __typename?: 'admin_ServiceInfo';
  hashHex: Scalars['String']['output'];
  namespace: Scalars['String']['output'];
  replicaCount: Scalars['Int']['output'];
  version: Scalars['String']['output'];
};

export type Admin_ServicesOutBody = {
  __typename?: 'admin_ServicesOutBody';
  environment: Maybe<Scalars['String']['output']>;
  services: Array<Maybe<Admin_ServiceInfo>>;
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

export type DashboardQueryVariables = Exact<{ [key: string]: never; }>;


export type DashboardQuery = { admin: { listPeers: { peers: Array<{ nodeId: string } | null> } | null, listServices: { environment: string | null, services: Array<{ namespace: string, version: string } | null> } | null } };

export type ServicesQueryVariables = Exact<{ [key: string]: never; }>;


export type ServicesQuery = { admin: { listServices: { environment: string | null, services: Array<{ namespace: string, version: string, hashHex: string, replicaCount: number } | null> } | null } };

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
      environment
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
      environment
      services {
        namespace
        version
        hashHex
        replicaCount
      }
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