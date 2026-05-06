// graphql-ws client for subscriptions. The gateway speaks
// graphql-transport-ws on the same /api/graphql path used for
// queries/mutations. In dev, vite proxies /api with ws:true; in
// prod, the gateway is same-origin.
//
// Subscription auth: the gateway's SDL requires `hmac: String!` and
// `timestamp: Int!` on every subscription field. In insecure mode
// (`--insecure-subscribe`) any value passes; otherwise mint via
// admin_signSubscriptionToken first.

import { createClient } from 'graphql-ws';
import { getAdminToken } from './auth';

function wsURL(): string {
  if (typeof window === 'undefined') return 'ws://localhost/api/graphql';
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/api/graphql`;
}

export const wsClient = createClient({
  url: wsURL(),
  // Lazy → connection opens on first subscribe; closes when last
  // subscriber detaches. Keeps the WS quiet on pages with no events.
  lazy: true,
  // Forward the admin token (when set) in connection_init so the
  // gateway can attribute subscriptions if it ever grows that
  // surface; harmless when no auth is configured.
  connectionParams: () => {
    const token = getAdminToken();
    return token ? { authorization: `Bearer ${token}` } : {};
  },
  // Clamp retries; long-running tabs in dev can otherwise spin if
  // the gateway is temporarily down.
  shouldRetry: () => true,
  retryAttempts: 10,
});
