// NATS JetStream Backend. Matches events/jetstream (Go) so both
// languages share streams, consumers and dead letters:
//
//   - one stream per channel, named streamName(key), capturing the
//     channel's wildcard subject. A missing stream is created (file
//     storage, limits retention, max age 7d, duplicate window 2m, one
//     replica) and an existing one is never updated;
//   - a group is a durable consumer of that name, explicit acks,
//     deliver-new (deliver-all with deliverAll), max deliver and ack
//     wait from the subscribe options. An existing consumer is used
//     as is;
//   - the message id rides in Nats-Msg-Id;
//   - a handler error naks with the backoff delay; after maxDeliver
//     failed deliveries the message is published to gwag.dlq.<stream>
//     with its original headers plus Gwag-Error, and terminated.
import {
  AckPolicy,
  DeliverPolicy,
  jetstream,
  jetstreamManager,
  JetStreamApiError,
  RetentionPolicy,
  StorageType,
  type ConsumerMessages,
  type JetStreamClient,
  type JetStreamManager,
  type JsMsg,
} from "@nats-io/jetstream";
import { headers, nanos, type NatsConnection } from "@nats-io/transport-node";
import {
  canonicalHeaderKey,
  delayMs,
  dlqSubject,
  HeaderError,
  HeaderNatsMsgID,
  type Backend,
  type Channel,
  type Handler,
  type Message,
  type ResolvedSubscribeOptions,
  type Stop,
} from "./events.ts";

const streamNameInUse = 10058;
const consumerExists = 10148;
const streamNotFound = 10059;
const consumerNotFound = 10014;

export interface JetStreamOptions {
  /** Logger for dead-letter publish failures. Default console.error. */
  logf?: (msg: string) => void;
  /**
   * How many messages a subscriber pulls ahead. Default 16. Lower
   * spreads a group's work more evenly across subscribers; prefetched
   * messages count against ackWaitMs.
   */
  prefetch?: number;
}

const day = 24 * 60 * 60 * 1000;

/** The config a missing stream is created with. */
export function streamConfig(ch: Channel) {
  return {
    name: ch.stream,
    subjects: [ch.pattern.wildcard()],
    storage: StorageType.File,
    retention: RetentionPolicy.Limits,
    max_age: nanos(7 * day),
    duplicate_window: nanos(2 * 60 * 1000),
    num_replicas: 1,
  };
}

function apiCode(e: unknown): number | undefined {
  return e instanceof JetStreamApiError ? e.apiError().err_code : undefined;
}

export class JetStreamBackend implements Backend {
  readonly #nc: NatsConnection;
  readonly #js: JetStreamClient;
  readonly #logf: (msg: string) => void;
  readonly #prefetch: number;
  #jsm: Promise<JetStreamManager> | undefined;
  readonly #ensured = new Set<string>();
  readonly #stops = new Set<Stop>();
  #closed = false;

  /** Close does not close nc. */
  constructor(nc: NatsConnection, opts: JetStreamOptions = {}) {
    this.#nc = nc;
    this.#js = jetstream(nc);
    this.#logf = opts.logf ?? ((m) => console.error(m));
    this.#prefetch = Math.max(1, opts.prefetch ?? 16);
  }

  #manager(): Promise<JetStreamManager> {
    return (this.#jsm ??= jetstreamManager(this.#nc));
  }

  async #ensureStream(ch: Channel): Promise<void> {
    if (this.#ensured.has(ch.stream)) return;
    const jsm = await this.#manager();
    try {
      await jsm.streams.info(ch.stream);
    } catch (e) {
      if (apiCode(e) !== streamNotFound) {
        throw new Error(`events/jetstream: stream ${ch.stream}: ${(e as Error).message}`, { cause: e });
      }
      try {
        await jsm.streams.add(streamConfig(ch));
      } catch (e2) {
        if (apiCode(e2) !== streamNameInUse) {
          throw new Error(`events/jetstream: stream ${ch.stream}: ${(e2 as Error).message}`, { cause: e2 });
        }
      }
    }
    this.#ensured.add(ch.stream);
  }

  async publish(ch: Channel, m: Message): Promise<void> {
    await this.#ensureStream(ch);
    const h = headers();
    for (const [k, v] of Object.entries(m.headers)) h.set(k, v);
    if (m.id !== "") h.set(HeaderNatsMsgID, m.id);
    try {
      await this.#js.publish(m.subject, m.data, { headers: h });
    } catch (e) {
      throw new Error(`events/jetstream: publish ${m.subject}: ${(e as Error).message}`, { cause: e });
    }
  }

  async subscribe(ch: Channel, group: string, opts: ResolvedSubscribeOptions, h: Handler): Promise<Stop> {
    if (this.#closed) throw new Error("events/jetstream: closed");
    await this.#ensureStream(ch);
    const consumer = await this.#consumer(ch, group, opts);
    const messages = await consumer.consume({ max_messages: this.#prefetch });
    let stopped = false;
    void this.#run(ch, opts, h, messages);
    const stop: Stop = async () => {
      if (stopped) return;
      stopped = true;
      this.#stops.delete(stop);
      messages.stop();
    };
    this.#stops.add(stop);
    return stop;
  }

  async #consumer(ch: Channel, group: string, opts: ResolvedSubscribeOptions) {
    try {
      return await this.#js.consumers.get(ch.stream, group);
    } catch (e) {
      if (apiCode(e) !== consumerNotFound) {
        throw new Error(`events/jetstream: consumer ${ch.stream}/${group}: ${(e as Error).message}`, { cause: e });
      }
    }
    const jsm = await this.#manager();
    try {
      await jsm.consumers.add(ch.stream, {
        durable_name: group,
        ack_policy: AckPolicy.Explicit,
        deliver_policy: opts.deliverAll ? DeliverPolicy.All : DeliverPolicy.New,
        max_deliver: opts.maxDeliver,
        ack_wait: nanos(opts.ackWaitMs),
      });
    } catch (e) {
      if (apiCode(e) !== consumerExists) {
        // A racing subscriber may have created it with the same config.
        try {
          return await this.#js.consumers.get(ch.stream, group);
        } catch {
          throw new Error(`events/jetstream: create consumer ${ch.stream}/${group}: ${(e as Error).message}`, { cause: e });
        }
      }
    }
    return this.#js.consumers.get(ch.stream, group);
  }

  async #run(ch: Channel, opts: ResolvedSubscribeOptions, h: Handler, messages: ConsumerMessages): Promise<void> {
    try {
      for await (const msg of messages) {
        await this.#handle(ch, opts, h, msg);
      }
    } catch (e) {
      if (!this.#closed) this.#logf(`events/jetstream: consume ${ch.stream}: ${(e as Error).message}`);
    }
  }

  async #handle(ch: Channel, opts: ResolvedSubscribeOptions, h: Handler, msg: JsMsg): Promise<void> {
    const m: Message = { subject: msg.subject, id: "", headers: {}, data: msg.data, delivery: msg.info.deliveryCount };
    for (const k of msg.headers?.keys() ?? []) {
      const v = msg.headers?.values(k)[0];
      if (v !== undefined) m.headers[canonicalHeaderKey(k)] = v;
    }
    m.id = m.headers[HeaderNatsMsgID] ?? "";
    delete m.headers[HeaderNatsMsgID];
    try {
      await h(m);
      msg.ack();
    } catch (e) {
      if (m.delivery >= opts.maxDeliver) {
        await this.#deadLetter(ch, msg, e);
        msg.term();
        return;
      }
      msg.nak(delayMs(opts, m.delivery));
    }
  }

  async #deadLetter(ch: Channel, msg: JsMsg, cause: unknown): Promise<void> {
    const subject = dlqSubject(ch.stream);
    const h = headers();
    for (const k of msg.headers?.keys() ?? []) {
      for (const v of msg.headers?.values(k) ?? []) h.append(k, v);
    }
    h.set(HeaderError, cause instanceof Error ? cause.message : String(cause));
    try {
      try {
        await this.#js.publish(subject, msg.data, { headers: h });
      } catch (e) {
        // A stream may capture gwag.dlq.>; without one, fall back to a
        // plain publish so live listeners still see it.
        if (!/no responders|503/i.test(String((e as Error)?.message) + String((e as { code?: unknown })?.code))) throw e;
        this.#nc.publish(subject, msg.data, { headers: h });
      }
    } catch (e) {
      this.#logf(`events/jetstream: dead-letter ${msg.subject}: ${(e as Error).message}`);
    }
  }

  /** Stops every subscriber started by this backend. */
  async close(): Promise<void> {
    this.#closed = true;
    await Promise.all([...this.#stops].map((s) => s()));
  }
}
