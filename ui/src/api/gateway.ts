import type { GraphQLClient, RequestOptions } from 'graphql-request';
import gql from 'graphql-tag';
export type Maybe<T> = T | null;
export type InputMaybe<T> = Maybe<T>;
export type Exact<T extends { [key: string]: unknown }> = { [K in keyof T]: T[K] };
export type MakeOptional<T, K extends keyof T> = Omit<T, K> & { [SubKey in K]?: Maybe<T[SubKey]> };
export type MakeMaybe<T, K extends keyof T> = Omit<T, K> & { [SubKey in K]: Maybe<T[SubKey]> };
export type MakeEmpty<T extends { [key: string]: unknown }, K extends keyof T> = { [_ in K]?: never };
export type Incremental<T> = T | { [P in keyof T]?: P extends ' $fragmentName' | '__typename' ? T[P] : never };
type GraphQLClientRequestHeaders = RequestOptions['requestHeaders'];
/** All built-in and custom scalars, mapped to their actual values */
export type Scalars = {
  ID: { input: string; output: string; }
  String: { input: string; output: string; }
  Boolean: { input: boolean; output: boolean; }
  Int: { input: number; output: number; }
  Float: { input: number; output: number; }
};

export type Mutation = {
  __typename?: 'Mutation';
  admin_drain: Maybe<Admin_DrainOutBody>;
  admin_forgetPeer: Maybe<Admin_ForgetOutBody>;
  admin_signSubscriptionToken: Maybe<Admin_SignOutBody>;
};


export type MutationAdmin_DrainArgs = {
  body: Admin_DrainInBodyInput;
};


export type MutationAdmin_ForgetPeerArgs = {
  nodeId: Scalars['String']['input'];
};


export type MutationAdmin_SignSubscriptionTokenArgs = {
  body: Admin_SignInBodyInput;
};

export type Query = {
  __typename?: 'Query';
  admin_listChannels: Maybe<Admin_ChannelsOutBody>;
  admin_listPeers: Maybe<Admin_PeersOutBody>;
  admin_listServices: Maybe<Admin_ServicesOutBody>;
};

export type Admin_ChannelInfo = {
  __typename?: 'admin_ChannelInfo';
  consumers: Scalars['Int']['output'];
  subject: Scalars['String']['output'];
};

export type Admin_ChannelsOutBody = {
  __typename?: 'admin_ChannelsOutBody';
  channels: Array<Maybe<Admin_ChannelInfo>>;
};

export type Admin_DrainInBodyInput = {
  timeoutSeconds: InputMaybe<Scalars['Int']['input']>;
};

export type Admin_DrainOutBody = {
  __typename?: 'admin_DrainOutBody';
  activeStreams: Scalars['Int']['output'];
  drained: Scalars['Boolean']['output'];
  reason: Maybe<Scalars['String']['output']>;
};

export type Admin_ForgetOutBody = {
  __typename?: 'admin_ForgetOutBody';
  newReplicas: Scalars['Int']['output'];
  removed: Scalars['Boolean']['output'];
};

export type Admin_PeerInfo = {
  __typename?: 'admin_PeerInfo';
  joinedUnixMs: Scalars['Int']['output'];
  name: Maybe<Scalars['String']['output']>;
  nodeId: Scalars['String']['output'];
};

export type Admin_PeersOutBody = {
  __typename?: 'admin_PeersOutBody';
  peers: Array<Maybe<Admin_PeerInfo>>;
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
  ttlSeconds: Scalars['Int']['input'];
};

export type Admin_SignOutBody = {
  __typename?: 'admin_SignOutBody';
  code: Scalars['String']['output'];
  hmac: Maybe<Scalars['String']['output']>;
  kid: Maybe<Scalars['String']['output']>;
  reason: Maybe<Scalars['String']['output']>;
  timestampUnix: Maybe<Scalars['Int']['output']>;
};

export type DashboardQueryVariables = Exact<{ [key: string]: never; }>;


export type DashboardQuery = { __typename?: 'Query', admin_listPeers: { __typename?: 'admin_PeersOutBody', peers: Array<{ __typename?: 'admin_PeerInfo', nodeId: string } | null> } | null, admin_listServices: { __typename?: 'admin_ServicesOutBody', environment: string | null, services: Array<{ __typename?: 'admin_ServiceInfo', namespace: string, version: string } | null> } | null };

export type ServicesQueryVariables = Exact<{ [key: string]: never; }>;


export type ServicesQuery = { __typename?: 'Query', admin_listServices: { __typename?: 'admin_ServicesOutBody', environment: string | null, services: Array<{ __typename?: 'admin_ServiceInfo', namespace: string, version: string, hashHex: string, replicaCount: number } | null> } | null };

export type PeersQueryVariables = Exact<{ [key: string]: never; }>;


export type PeersQuery = { __typename?: 'Query', admin_listPeers: { __typename?: 'admin_PeersOutBody', peers: Array<{ __typename?: 'admin_PeerInfo', nodeId: string, name: string | null, joinedUnixMs: number } | null> } | null };

export type ForgetPeerMutationVariables = Exact<{
  nodeId: Scalars['String']['input'];
}>;


export type ForgetPeerMutation = { __typename?: 'Mutation', admin_forgetPeer: { __typename?: 'admin_ForgetOutBody', removed: boolean, newReplicas: number } | null };


export const DashboardDocument = gql`
    query Dashboard {
  admin_listPeers {
    peers {
      nodeId
    }
  }
  admin_listServices {
    environment
    services {
      namespace
      version
    }
  }
}
    `;
export const ServicesDocument = gql`
    query Services {
  admin_listServices {
    environment
    services {
      namespace
      version
      hashHex
      replicaCount
    }
  }
}
    `;
export const PeersDocument = gql`
    query Peers {
  admin_listPeers {
    peers {
      nodeId
      name
      joinedUnixMs
    }
  }
}
    `;
export const ForgetPeerDocument = gql`
    mutation ForgetPeer($nodeId: String!) {
  admin_forgetPeer(nodeId: $nodeId) {
    removed
    newReplicas
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
    ForgetPeer(variables: ForgetPeerMutationVariables, requestHeaders?: GraphQLClientRequestHeaders, signal?: RequestInit['signal']): Promise<ForgetPeerMutation> {
      return withWrapper((wrappedRequestHeaders) => client.request<ForgetPeerMutation>({ document: ForgetPeerDocument, variables, requestHeaders: { ...requestHeaders, ...wrappedRequestHeaders }, signal }), 'ForgetPeer', 'mutation', variables);
    }
  };
}
export type Sdk = ReturnType<typeof getSdk>;