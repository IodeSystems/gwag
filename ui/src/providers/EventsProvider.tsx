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
import type { Client } from 'graphql-ws';
import { wsClient } from '@/api/events';

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

  const push = useCallback((entry: EventEntry) => {
    setRecent((prev) => {
      const next = [entry, ...prev];
      if (next.length > BUFFER_LIMIT) next.length = BUFFER_LIMIT;
      return next;
    });
    setUnread((u) => u + 1);
  }, []);

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

  return <EventsContext.Provider value={value}>{children}</EventsContext.Provider>;
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
