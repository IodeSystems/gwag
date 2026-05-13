// EventsProvider — central hub for graphql-ws subscriptions.
//
// Anywhere under <EventsProvider>:
//
//   useSubscribe<T>({
//     id: 'greeter-alice',
//     query: `subscription ($name:String!,$hmac:String!,$timestamp:Int!) {
//       greeter_greetings(name:$name, hmac:$hmac, timestamp:$timestamp) {
//         greeting forName
//       }
//     }`,
//     variables: { name: 'alice', hmac: 'x', timestamp: 0 },
//     onData: (payload) => console.log(payload),
//   });
//
// Every event flows into a global ring buffer (last 50 by default),
// which the EventsTray renders as a feed. Pages that need typed
// access to specific events can also pass `onData` directly.
//
// Connection lifecycle is graphql-ws lazy: it opens on the first
// subscribe and closes when the last subscriber detaches. There is
// always exactly one WS to /api/graphql per page, regardless of how
// many useSubscribe hooks are mounted.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { Alert, Snackbar } from '@mui/material';
import { print } from 'graphql';
import type { Client } from 'graphql-ws';
import type { ResultOf } from '@graphql-typed-document-node/core';
import { wsClient } from '@/api/events';
import { AdminEventsSubscription } from '@/api/operations';

const BUFFER_LIMIT = 50;

export interface EventEntry {
  /** Stable id supplied by the subscriber; used to label the feed entry. */
  id: string;
  /** ms since epoch when the event arrived. */
  receivedAt: number;
  /** GraphQL data payload (top-level subscription field's response). */
  payload: unknown;
  /** When set, the event was an error frame; `payload` is the error array. */
  error?: boolean;
}

// ServiceChange is inferred from the AdminEventsSubscription document.
// Action values come from the proto enum (REGISTERED / DEREGISTERED);
// other fields track the schema.
export type ServiceChange = NonNullable<
  ResultOf<typeof AdminEventsSubscription>['admin_events_watchServices']
>;

interface ToastEntry {
  key: number;
  severity: 'success' | 'info' | 'warning' | 'error';
  message: string;
}

interface EventsContextValue {
  client: Client;
  recent: EventEntry[];
  unread: number;
  markRead: () => void;
  clear: () => void;
}

const EventsContext = createContext<EventsContextValue | null>(null);

export function EventsProvider({ children }: { children: ReactNode }) {
  const [recent, setRecent] = useState<EventEntry[]>([]);
  const [unread, setUnread] = useState(0);
  const [toast, setToast] = useState<ToastEntry | null>(null);
  const toastQueue = useRef<ToastEntry[]>([]);
  const toastSeq = useRef(0);

  const enqueueToast = useCallback((t: Omit<ToastEntry, 'key'>) => {
    const entry = { ...t, key: ++toastSeq.current };
    setToast((cur) => {
      if (cur === null) return entry;
      toastQueue.current.push(entry);
      return cur;
    });
  }, []);

  const dismissToast = () => {
    setToast(toastQueue.current.shift() ?? null);
  };

  const push = useCallback((entry: EventEntry) => {
    setRecent((prev) => {
      const next = [entry, ...prev];
      if (next.length > BUFFER_LIMIT) next.length = BUFFER_LIMIT;
      return next;
    });
    setUnread((u) => u + 1);
  }, []);

  // Auto-subscribe to the gateway's admin_events_watchServices stream
  // so service registrations / deregistrations show up as toasts and
  // in the events tray without per-page wiring. Errors are silenced
  // (logged to console) so a deployment without the subscription
  // field doesn't spam the UI buffer on every retry.
  useEffect(() => {
    let disposed = false;
    const dispose = wsClient.subscribe<ResultOf<typeof AdminEventsSubscription>>(
      {
        query: print(AdminEventsSubscription),
        // namespace="" → NATS wildcard match (all namespaces).
        variables: { namespace: '' },
      },
      {
        next: (msg) => {
          if (disposed) return;
          const change = msg.data?.admin_events_watchServices;
          if (!change) return;
          push({ id: 'admin_events', receivedAt: Date.now(), payload: change });
          enqueueToast(toastForServiceChange(change));
        },
        error: (e) => {
          // Silenced from the UI feed; log to console for diagnostics.
          // Likely cause: deployment without AddAdminEvents, or a
          // subscription auth misconfiguration.
          // eslint-disable-next-line no-console
          console.warn('admin_events subscription error:', e);
        },
        complete: () => {},
      },
    );
    return () => {
      disposed = true;
      dispose();
    };
  }, [push, enqueueToast]);

  // Expose the push fn through context so useSubscribe can reach it
  // without prop-drilling, but keep the public surface narrow.
  const value = useMemo<EventsContextValue & { _push: typeof push }>(
    () => ({
      client: wsClient,
      recent,
      unread,
      markRead: () => setUnread(0),
      clear: () => {
        setRecent([]);
        setUnread(0);
      },
      _push: push,
    }),
    [recent, unread, push],
  );

  return (
    <EventsContext.Provider value={value}>
      {children}
      <Snackbar
        key={toast?.key}
        open={toast !== null}
        autoHideDuration={4000}
        onClose={dismissToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'right' }}
      >
        {toast ? (
          <Alert severity={toast.severity} variant="filled" onClose={dismissToast} sx={{ width: '100%' }}>
            {toast.message}
          </Alert>
        ) : undefined}
      </Snackbar>
    </EventsContext.Provider>
  );
}

function toastForServiceChange(c: ServiceChange): Omit<ToastEntry, 'key'> {
  const what = `${c.namespace}/${c.version}`;
  switch (c.action) {
    case 'ACTION_REGISTERED':
      return {
        severity: 'success',
        message: `Service registered: ${what} (${c.replicaCount} replica${c.replicaCount === 1 ? '' : 's'})`,
      };
    case 'ACTION_DEREGISTERED':
      return {
        severity: 'warning',
        message:
          c.replicaCount === 0
            ? `Service deregistered: ${what}`
            : `Replica removed: ${what} (${c.replicaCount} remaining)`,
      };
    default:
      return { severity: 'info', message: `Service change: ${what} (${c.action})` };
  }
}

export function useEvents(): EventsContextValue {
  const ctx = useContext(EventsContext);
  if (!ctx) throw new Error('useEvents() outside <EventsProvider>');
  return ctx;
}

interface SubscribeOpts<T> {
  /** Stable identifier — shown in the events tray. */
  id: string;
  /** GraphQL subscription document. */
  query: string;
  /** Variables (hmac/timestamp included if the field requires them). */
  variables?: Record<string, unknown>;
  /** Per-event callback. The provider also pushes into the global feed. */
  onData?: (payload: T) => void;
  /** Per-error callback. */
  onError?: (err: unknown) => void;
  /** Disable to skip subscribing without unmounting. */
  enabled?: boolean;
}

/**
 * Subscribes via the shared graphql-ws client. Every emitted event is
 * also pushed into the EventsProvider's ring buffer, which the
 * EventsTray renders.
 */
export function useSubscribe<T = unknown>(opts: SubscribeOpts<T>): void {
  const { id, query, variables, onData, onError, enabled = true } = opts;
  // _push lives on the context but isn't part of the public type.
  const ctx = useContext(EventsContext) as
    | (EventsContextValue & { _push: (e: EventEntry) => void })
    | null;
  if (!ctx) throw new Error('useSubscribe() outside <EventsProvider>');

  // Snapshot callbacks in refs so we don't tear down the subscription
  // every render when the parent re-creates closures.
  const onDataRef = useRef(onData);
  const onErrorRef = useRef(onError);
  useEffect(() => {
    onDataRef.current = onData;
    onErrorRef.current = onError;
  });

  useEffect(() => {
    if (!enabled) return;
    const dispose = ctx.client.subscribe<{ data: T }>(
      { query, variables },
      {
        next: (msg) => {
          const payload = (msg as { data?: T }).data ?? msg;
          ctx._push({ id, receivedAt: Date.now(), payload });
          onDataRef.current?.(payload as T);
        },
        error: (err) => {
          ctx._push({ id, receivedAt: Date.now(), payload: err, error: true });
          onErrorRef.current?.(err);
        },
        complete: () => {},
      },
    );
    return () => dispose();
  }, [id, query, JSON.stringify(variables ?? null), enabled, ctx]);
}
