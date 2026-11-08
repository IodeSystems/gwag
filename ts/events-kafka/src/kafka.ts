// @gwag/events-kafka: a Backend over Apache Kafka (kafkajs), in its
// own package so @gwag/events carries no Kafka dependency. Same
// mapping as events/kafka in Go (docs/events.md, "Kafka"):
//
//   - one topic per channel, named ch.stream (EV_<pkg>_<Name>),
//     created with 3 partitions and replication 1 if missing;
//   - record key = subject (ordering per subject), value = the body
//     the Serde wrote (plain proto by default), headers = Gwag-Type,
//     Gwag-Channel, Gwag-Msg-Id and extras;
//   - group = Kafka consumer group "<topic>.<group>";
//   - a failed handler is retried in process with the backoff (the
//     partition waits), then the record goes to topic
//     gwag.dlq.<topic> with Gwag-Error and the offset is committed.
//     retryTopic() moves retries to a per-group retry topic instead.
//
// The producer is idempotent with acks=-1 (all in-sync replicas).
//
// No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
// seen. Handlers must be idempotent.
import {
  delayMs,
  DLQPrefix,
  HeaderChannel,
  HeaderError,
  HeaderMsgID,
  type Backend,
  type Channel,
  type Handler,
  type Message,
  type ResolvedSubscribeOptions,
  type Stop,
  type SubscribeOptions,
  type Topics,
} from "@gwag/events";

import { Kafka, logLevel, Partitioners, type Consumer, type IHeaders, type KafkaConfig, type Producer } from "kafkajs";
import { installSnappy } from "./snappy.ts";

/** Headers this backend adds. */
export const HeaderSubject = "Gwag-Subject";
/** The delivery attempt of a record on a retry topic. */
export const HeaderAttempt = "Gwag-Attempt";
/** When a retry record is due, in Unix milliseconds. */
export const HeaderRetryAt = "Gwag-Retry-At";

/** A record as the backend reads it. */
export interface KafkaRecord {
  topic: string;
  partition: number;
  offset: string;
  key: Uint8Array | null;
  value: Uint8Array | null;
  headers: Record<string, string>;
}

/** Where one channel lives on Kafka. Missing fields take the defaults. */
export interface Topic {
  /** The topic. Default ch.stream. */
  name?: string;
  /** Partitions for a topic the backend creates. Default `partitions` (3). A group spreads over at most this many subscribers. */
  partitions?: number;
  /** Replication for a topic the backend creates. Default `replication` (1); -1 is the broker default. */
  replication?: number;
  /** The record key of a publish. Default: the subject. When the key is not the subject, the subject rides in Gwag-Subject. */
  key?: (subject: string, m: Message) => Uint8Array | string;
  /**
   * The subject of a record without Gwag-Subject or Gwag-Channel, i.e.
   * one a producer other than gwag wrote. Default: the channel pattern
   * when it has no wildcards, else the record key.
   */
  subject?: (r: KafkaRecord) => string;
  /** The Kafka consumer group for a gwag group. Default "<name>.<group>". */
  group?: (group: string) => string;
  /** The dead-letter topic. Default "gwag.dlq.<name>". */
  dlq?: string;
  /** The retry topic of a group (retryTopic mode). Default "gwag.retry.<name>.<group>". One per group, so a retry reaches only the group that failed. */
  retry?: (group: string) => string;
}

/** Returns the Topic of a channel. Leave a field out to take its default. */
export type Mapper = (ch: Channel) => Topic;

/** A Mapper over a table keyed by channel key ("orders.v1.OrderEvents"). Channels not in the table take the defaults. */
export function topicTable(table: Record<string, Topic>): Mapper {
  return (ch) => table[ch.key] ?? {};
}

export interface KafkaBackendOptions {
  /** Partitions for topics the backend creates. Default 3. */
  partitions?: number;
  /** Replication for topics the backend creates. Default 1; -1 is the broker default. */
  replication?: number;
  /** The channel-to-topic mapper. */
  topics?: Mapper;
  /**
   * kafkajs client options for every client the backend makes (the
   * producer and each subscriber): ssl, sasl, clientId, timeouts,
   * retry, logLevel. They apply after the backend's own, so they win.
   * `brokers` is the constructor's.
   */
  client?: Omit<KafkaConfig, "brokers">;
}

const retryKey = "kafka.retry";
const retryProcessMode = "process";
const retryTopicMode = "topic";

/**
 * Retry a failed record in the subscriber, waiting the backoff between
 * attempts. The partition waits too: later records on it are not
 * delivered until this one is acked or dead-lettered. The default.
 * Spread into the subscribe options: `{ ...retryInProcess(), maxDeliver: 5 }`.
 */
export function retryInProcess(): Pick<SubscribeOptions, "backend"> {
  return { backend: { [retryKey]: retryProcessMode } };
}

/**
 * Move a failed record to the group's retry topic (Topic.retry) with
 * Gwag-Attempt and Gwag-Retry-At, and commit it on the channel topic,
 * so the partition moves on. A second consumer reads the retry topic,
 * waits until each record is due, and delivers it again; after
 * maxDeliver failed deliveries the record goes to the DLQ. Order per
 * key is not kept across a retry. A record due later holds up the
 * retry records behind it (one retry topic per group, not one per
 * delay).
 */
export function retryTopic(): Pick<SubscribeOptions, "backend"> {
  return { backend: { [retryKey]: retryTopicMode } };
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

class Stopped extends Error {}

const STARTING = new Set([
  "UNKNOWN_TOPIC_OR_PARTITION",
  "LEADER_NOT_AVAILABLE",
  "NOT_LEADER_FOR_PARTITION",
  "COORDINATOR_NOT_AVAILABLE",
  "COORDINATOR_LOAD_IN_PROGRESS",
  "NOT_COORDINATOR",
]);

function isStarting(e: unknown): boolean {
  for (let c = e as { type?: string; retriable?: boolean; cause?: unknown } | undefined; c; c = c.cause as typeof c) {
    if ((c.type !== undefined && STARTING.has(c.type)) || c.retriable === true) return true;
  }
  return false;
}

/**
 * Runs fn until it succeeds, fails with an error that is not about the
 * broker still starting up, or 30s pass. A new topic has no leader
 * everywhere yet, and a fresh broker has no group coordinator until
 * __consumer_offsets is created.
 */
async function retryStartup<T>(fn: () => Promise<T>): Promise<T> {
  for (const deadline = Date.now() + 30000; ; ) {
    try {
      return await fn();
    } catch (e) {
      if (!isStarting(e) || Date.now() > deadline) throw e;
    }
    await sleep(50);
  }
}

type FilledTopic = Required<Omit<Topic, "key">> & Pick<Topic, "key">;

interface Out {
  key: Buffer | null;
  value: Buffer;
  headers: IHeaders;
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

interface Sub {
  ch: Channel;
  t: FilledTopic;
  opts: ResolvedSubscribeOptions;
  h: Handler;
  retryTopic: string;
  stopped: boolean;
}

export class KafkaBackend implements Backend, Topics {
  readonly #kafka: Kafka;
  readonly #partitions: number;
  readonly #replication: number;
  readonly #mapper: Mapper | undefined;
  readonly #producer: Producer;
  readonly #admin;
  readonly #topics = new Set<string>();
  readonly #stops = new Set<Stop>();
  #connected: Promise<void> | undefined;
  #closed = false;

  constructor(brokers: string[], opts: KafkaBackendOptions = {}) {
    installSnappy();
    const base: KafkaConfig = { clientId: "gwag-events", brokers, logLevel: logLevel.NOTHING, ...opts.client };
    this.#kafka = new Kafka(base);
    this.#partitions = opts.partitions ?? 3;
    this.#replication = opts.replication ?? 1;
    this.#mapper = opts.topics;
    this.#producer = this.#kafka.producer({
      createPartitioner: Partitioners.DefaultPartitioner,
      allowAutoTopicCreation: true,
      // Idempotent: the broker drops a batch the producer retried.
      // kafkajs numbers batches per partition; with one request in
      // flight a retried batch cannot overtake a later one. acks is -1
      // on every send (kafkajs requires it when idempotent).
      idempotent: true,
      maxInFlightRequests: 1,
    });
    this.#admin = this.#kafka.admin();
  }

  /** ch's topic: the mapper's, defaults filled in. */
  topicOf(ch: Channel): FilledTopic {
    const t = this.#mapper?.(ch) ?? {};
    const name = t.name || ch.stream;
    const literal = ch.pattern.segments().length === 0;
    return {
      name,
      partitions: t.partitions || this.#partitions,
      replication: t.replication || this.#replication,
      key: t.key,
      subject: t.subject ?? ((r) => (literal ? ch.pattern.raw : (r.key ? Buffer.from(r.key).toString() : ""))),
      group: t.group ?? ((g) => `${name}.${g}`),
      dlq: t.dlq || DLQPrefix + name,
      retry: t.retry ?? ((g) => `gwag.retry.${name}.${g}`),
    };
  }

  /** The topic name; a Serde may derive a schema subject from it. */
  topic(ch: Channel): string {
    return this.topicOf(ch).name;
  }

  #connect(): Promise<void> {
    return (this.#connected ??= Promise.all([this.#producer.connect(), this.#admin.connect()]).then(() => undefined));
  }

  async #ensureTopic(topic: string, partitions: number, replication: number): Promise<void> {
    if (this.#topics.has(topic)) return;
    await this.#connect();
    // createTopics resolves false when the topic exists already.
    await this.#admin.createTopics({ waitForLeaders: false, topics: [{ topic, numPartitions: partitions, replicationFactor: replication }] });
    // A new topic's metadata can take a moment to reach every broker.
    for (let i = 0; ; i++) {
      try {
        const md = await this.#admin.fetchTopicMetadata({ topics: [topic] });
        if (md.topics[0]?.partitions.every((p) => p.leader >= 0)) break;
      } catch (e) {
        if (i >= 100) throw e;
      }
      if (i >= 100) throw new Error(`kafka: topic ${topic}: no leaders after 10s`);
      await sleep(100);
    }
    this.#topics.add(topic);
  }

  /**
   * The start position of a group that has no commit yet, per partition:
   * the end now, or the start with deliverAll. A new group otherwise
   * resolves its position at its first fetch, after it has joined, so a
   * record produced between the join and that fetch is skipped although
   * subscribe has returned. The consumer seeks to these on joining
   * (kafkajs applies a seek before it resolves offsets). Partitions with
   * a commit keep it.
   */
  async #startOffsets(topic: string, groupId: string, deliverAll: boolean): Promise<Map<number, string>> {
    await this.#connect();
    const committed = await retryStartup(() => this.#admin.fetchOffsets({ groupId, topics: [topic] }));
    const have = new Set<number>();
    for (const t of committed) for (const p of t.partitions) if (Number(p.offset) >= 0) have.add(p.partition);
    const offsets = await retryStartup(() => this.#admin.fetchTopicOffsets(topic));
    const start = new Map<number, string>();
    for (const o of offsets) if (!have.has(o.partition)) start.set(o.partition, deliverAll ? o.low : o.high);
    return start;
  }

  #record(key: Uint8Array | string | null, value: Uint8Array, m: Message): Out {
    const headers: IHeaders = { ...m.headers };
    if (m.id !== "") headers[HeaderMsgID] = m.id;
    return { key: key === null ? null : Buffer.from(key), value: Buffer.from(value), headers };
  }

  async #send(topic: string, record: Out): Promise<void> {
    await this.#producer.send({ topic, messages: [record], acks: -1 });
  }

  async publish(ch: Channel, m: Message): Promise<void> {
    const t = this.topicOf(ch);
    await this.#ensureTopic(t.name, t.partitions, t.replication);
    let key: Uint8Array | string = m.subject;
    let headers = m.headers;
    if (t.key) {
      key = t.key(m.subject, m);
      if (Buffer.from(key).toString() !== m.subject) headers = { ...headers, [HeaderSubject]: m.subject };
    }
    await this.#send(t.name, this.#record(key, m.data, { ...m, headers }));
  }

  async subscribe(ch: Channel, group: string, opts: ResolvedSubscribeOptions, h: Handler): Promise<Stop> {
    if (this.#closed) throw new Error("kafka: closed");
    const t = this.topicOf(ch);
    const mode = (opts.backend[retryKey] as string | undefined) ?? retryProcessMode;
    if (mode !== retryProcessMode && mode !== retryTopicMode) throw new Error(`kafka: unknown retry mode "${mode}"`);
    await this.#ensureTopic(t.name, t.partitions, t.replication);
    // Create the DLQ topic now. A reader of the DLQ that starts before
    // the first dead letter would otherwise wait for a metadata
    // refresh to find a topic created after it.
    await this.#ensureTopic(t.dlq, t.partitions, t.replication);
    const s: Sub = { ch, t, opts, h, retryTopic: "", stopped: false };
    const mainGroup = t.group(group);
    const consumers: Consumer[] = [];
    const stop: Stop = async () => {
      if (s.stopped) return;
      s.stopped = true;
      this.#stops.delete(stop);
      await Promise.all(consumers.map((c) => c.disconnect().catch(() => {})));
    };
    try {
      consumers.push(await this.#start(s, t.name, mainGroup, opts.deliverAll, (r, hb) => this.#handleMain(s, r, hb)));
      if (mode === retryTopicMode) {
        s.retryTopic = t.retry(group);
        await this.#ensureTopic(s.retryTopic, t.partitions, t.replication);
        consumers.push(await this.#start(s, s.retryTopic, `${mainGroup}.retry`, false, (r, hb) => this.#handleRetry(s, r, hb)));
      }
    } catch (e) {
      await stop();
      throw e;
    }
    this.#stops.add(stop);
    return stop;
  }

  /** Seeds groupId on topic, starts a consumer that calls handle per record, and waits for its partition assignment. */
  async #start(s: Sub, topic: string, groupId: string, deliverAll: boolean, handle: (r: KafkaRecord, heartbeat: () => Promise<void>) => Promise<void>): Promise<Consumer> {
    const start = await this.#startOffsets(topic, groupId, deliverAll);
    const consumer = this.#kafka.consumer({ groupId, allowAutoTopicCreation: false });
    const joined = new Promise<void>((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`kafka: group ${groupId}: no partition assignment after 30s`)), 30000);
      consumer.on(consumer.events.GROUP_JOIN, () => {
        // Once: a later rebalance must not rewind partitions already consumed.
        for (const [partition, offset] of start) consumer.seek({ topic, partition, offset });
        start.clear();
        clearTimeout(t);
        resolve();
      });
    });
    joined.catch(() => {});
    try {
      await consumer.connect();
      await consumer.subscribe({ topic, fromBeginning: deliverAll });
      await consumer.run({
        autoCommit: true,
        eachMessage: async ({ topic: tp, partition, message, heartbeat }) => {
          const headers: Record<string, string> = {};
          for (const [k, v] of Object.entries(message.headers ?? {})) {
            const first = Array.isArray(v) ? v[0] : v;
            if (first !== undefined) headers[k] = first.toString();
          }
          await handle(
            { topic: tp, partition, offset: message.offset, key: message.key, value: message.value, headers },
            heartbeat,
          );
        },
      });
      await joined;
    } catch (e) {
      await consumer.disconnect().catch(() => {});
      throw e;
    }
    return consumer;
  }

  /** Turns a record into the message handlers see. */
  #message(s: Sub, r: KafkaRecord): Message {
    const headers = { ...r.headers };
    const id = headers[HeaderMsgID] ?? "";
    delete headers[HeaderMsgID];
    let subject: string;
    const own = headers[HeaderSubject];
    if (own !== undefined) {
      subject = own;
      delete headers[HeaderSubject];
    } else if ((headers[HeaderChannel] ?? "") !== "") {
      subject = r.key ? Buffer.from(r.key).toString() : "";
    } else {
      subject = s.t.subject(r);
    }
    const m: Message = { subject, id, headers, data: new Uint8Array(r.value ?? []), delivery: 0 };
    if (r.key) m.key = new Uint8Array(r.key);
    return m;
  }

  /** The record to write for m on topic: m's headers, the original key and raw value. */
  #rewrite(r: KafkaRecord, m: Message) {
    const headers = { ...m.headers };
    if (r.key === null || Buffer.from(r.key).toString() !== m.subject) headers[HeaderSubject] = m.subject;
    return this.#record(r.key, r.value ?? new Uint8Array(), { ...m, headers });
  }

  async #handleMain(s: Sub, r: KafkaRecord, heartbeat: () => Promise<void>): Promise<void> {
    const m = this.#message(s, r);
    if (s.retryTopic !== "") {
      m.delivery = 1;
      try {
        await s.h(m);
      } catch (e) {
        await this.#fail(s, r, m, e);
      }
      return;
    }
    let err: unknown;
    for (let attempt = 1; attempt <= s.opts.maxDeliver; attempt++) {
      m.delivery = attempt;
      try {
        await s.h(m);
        return;
      } catch (e) {
        err = e;
      }
      if (attempt < s.opts.maxDeliver) await this.#wait(s, delayMs(s.opts, attempt), heartbeat);
    }
    await this.#deadLetter(s, r, m, err);
  }

  async #handleRetry(s: Sub, r: KafkaRecord, heartbeat: () => Promise<void>): Promise<void> {
    const m = this.#message(s, r);
    const attempt = Math.max(2, Number.parseInt(m.headers[HeaderAttempt] ?? "", 10) || 0);
    const at = Number(m.headers[HeaderRetryAt]);
    if (Number.isFinite(at) && m.headers[HeaderRetryAt] !== undefined) await this.#wait(s, at - Date.now(), heartbeat);
    delete m.headers[HeaderAttempt];
    delete m.headers[HeaderRetryAt];
    m.delivery = attempt;
    try {
      await s.h(m);
    } catch (e) {
      await this.#fail(s, r, m, e);
    }
  }

  /** Waits ms in slices so the group does not consider this member dead. */
  async #wait(s: Sub, ms: number, heartbeat: () => Promise<void>): Promise<void> {
    for (let left = ms; left > 0; left -= 1000) {
      if (s.stopped) throw new Stopped("kafka: stopped");
      await sleep(Math.min(left, 1000));
      await heartbeat();
    }
  }

  /**
   * Sends a record that failed delivery m.delivery to the retry topic,
   * or to the DLQ after the last attempt. It returns once the record
   * is written, so the offset is committed only after that.
   */
  async #fail(s: Sub, r: KafkaRecord, m: Message, err: unknown): Promise<void> {
    if (m.delivery >= s.opts.maxDeliver) return this.#deadLetter(s, r, m, err);
    const next: Message = {
      ...m,
      headers: { ...m.headers, [HeaderAttempt]: String(m.delivery + 1), [HeaderRetryAt]: String(Date.now() + delayMs(s.opts, m.delivery)) },
    };
    // A retry record that is lost would drop the message: write until it lands.
    for (;;) {
      try {
        await this.#send(s.retryTopic, this.#rewrite(r, next));
        return;
      } catch {
        if (s.stopped) throw new Stopped("kafka: stopped");
        await sleep(1000);
      }
    }
  }

  async #deadLetter(s: Sub, r: KafkaRecord, m: Message, err: unknown): Promise<void> {
    const headers: Record<string, string> = { ...m.headers, [HeaderError]: errText(err) };
    delete headers[HeaderAttempt];
    delete headers[HeaderRetryAt];
    try {
      await this.#ensureTopic(s.t.dlq, s.t.partitions, s.t.replication);
      await this.#send(s.t.dlq, this.#rewrite(r, { ...m, headers }));
    } catch {
      // Same as the Go backend: a failed dead-letter publish drops the record.
    }
  }

  /** Stops every subscriber and the producer. */
  async close(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    await Promise.all([...this.#stops].map((s) => s()));
    if (this.#connected) {
      await this.#producer.disconnect().catch(() => {});
      await this.#admin.disconnect().catch(() => {});
    }
  }
}
