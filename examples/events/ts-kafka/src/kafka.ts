// Backend over Apache Kafka (kafkajs). An example, in its own package
// so @gwag/events carries no Kafka dependency; copy it into your
// project. Same mapping as ../go-kafka/kafka:
//
//   - one topic per channel, named ch.stream (EV_<pkg>_<Name>), created
//     with `partitions` partitions if missing;
//   - record key = subject (ordering per subject), value = proto body,
//     headers = Gwag-Type, Gwag-Channel, Gwag-Msg-Id and extras;
//   - group = Kafka consumer group "<stream>.<group>";
//   - a failed handler is retried in process with the backoff (the
//     partition waits), then the record goes to topic gwag.dlq.<stream>
//     with Gwag-Error and the offset is committed.
//
// No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
// seen. Handlers must be idempotent.
import {
  delayMs,
  dlqSubject,
  HeaderError,
  HeaderMsgID,
  type Backend,
  type Channel,
  type Handler,
  type Message,
  type ResolvedSubscribeOptions,
  type Stop,
} from "@gwag/events";
import { Kafka, logLevel, Partitioners, type Consumer, type IHeaders, type Producer } from "kafkajs";

export interface KafkaBackendOptions {
  /** Partitions for topics the backend creates. Default 3. A group spreads over at most this many subscribers. */
  partitions?: number;
  clientId?: string;
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

export class KafkaBackend implements Backend {
  readonly #kafka: Kafka;
  readonly #partitions: number;
  readonly #producer: Producer;
  readonly #admin;
  readonly #topics = new Set<string>();
  readonly #stops = new Set<Stop>();
  #connected: Promise<void> | undefined;
  #closed = false;

  constructor(brokers: string[], opts: KafkaBackendOptions = {}) {
    this.#kafka = new Kafka({ clientId: opts.clientId ?? "gwag-events", brokers, logLevel: logLevel.NOTHING });
    this.#partitions = opts.partitions ?? 3;
    this.#producer = this.#kafka.producer({ createPartitioner: Partitioners.DefaultPartitioner, allowAutoTopicCreation: true });
    this.#admin = this.#kafka.admin();
  }

  #connect(): Promise<void> {
    return (this.#connected ??= Promise.all([this.#producer.connect(), this.#admin.connect()]).then(() => undefined));
  }

  async #ensureTopic(topic: string): Promise<void> {
    if (this.#topics.has(topic)) return;
    await this.#connect();
    // createTopics resolves false when the topic exists already.
    await this.#admin.createTopics({ waitForLeaders: false, topics: [{ topic, numPartitions: this.#partitions, replicationFactor: 1 }] });
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

  #record(m: Message) {
    const headers: IHeaders = { ...m.headers };
    if (m.id !== "") headers[HeaderMsgID] = m.id;
    return { key: m.subject, value: Buffer.from(m.data), headers };
  }

  async publish(ch: Channel, m: Message): Promise<void> {
    await this.#ensureTopic(ch.stream);
    await this.#producer.send({ topic: ch.stream, messages: [this.#record(m)], acks: -1 });
  }

  async subscribe(ch: Channel, group: string, opts: ResolvedSubscribeOptions, h: Handler): Promise<Stop> {
    if (this.#closed) throw new Error("kafka: closed");
    await this.#ensureTopic(ch.stream);
    await this.#ensureTopic(dlqSubject(ch.stream));
    const groupId = `${ch.stream}.${group}`;
    const start = await this.#startOffsets(ch.stream, groupId, opts.deliverAll);
    const consumer = this.#kafka.consumer({ groupId, allowAutoTopicCreation: false });
    let stopped = false;
    const joined = new Promise<void>((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`kafka: group ${groupId}: no partition assignment after 30s`)), 30000);
      consumer.on(consumer.events.GROUP_JOIN, () => {
        // Once: a later rebalance must not rewind partitions already consumed.
        for (const [partition, offset] of start) consumer.seek({ topic: ch.stream, partition, offset });
        start.clear();
        clearTimeout(t);
        resolve();
      });
    });
    await consumer.connect();
    await consumer.subscribe({ topic: ch.stream, fromBeginning: opts.deliverAll });
    await consumer.run({
      autoCommit: true,
      eachMessage: async ({ message, heartbeat }) => {
        const m: Message = { subject: message.key?.toString() ?? "", id: "", headers: {}, data: new Uint8Array(message.value ?? []), delivery: 0 };
        for (const [k, v] of Object.entries(message.headers ?? {})) {
          const first = Array.isArray(v) ? v[0] : v;
          if (first !== undefined) m.headers[k] = first.toString();
        }
        m.id = m.headers[HeaderMsgID] ?? "";
        delete m.headers[HeaderMsgID];
        await this.#handle(ch, opts, h, m, heartbeat, () => stopped);
      },
    });
    try {
      await joined;
    } catch (e) {
      await consumer.disconnect().catch(() => {});
      throw e;
    }
    const stop: Stop = async () => {
      if (stopped) return;
      stopped = true;
      this.#stops.delete(stop);
      await this.#disconnect(consumer);
    };
    this.#stops.add(stop);
    return stop;
  }

  async #disconnect(c: Consumer): Promise<void> {
    await c.disconnect().catch(() => {});
  }

  async #handle(
    ch: Channel,
    opts: ResolvedSubscribeOptions,
    h: Handler,
    m: Message,
    heartbeat: () => Promise<void>,
    stopped: () => boolean,
  ): Promise<void> {
    let err: unknown;
    for (let attempt = 1; attempt <= opts.maxDeliver; attempt++) {
      m.delivery = attempt;
      try {
        await h(m);
        return;
      } catch (e) {
        err = e;
      }
      if (attempt < opts.maxDeliver) {
        // Wait out the backoff in slices so the group does not
        // consider this member dead.
        for (let left = delayMs(opts, attempt); left > 0; left -= 1000) {
          if (stopped()) throw new Stopped("kafka: stopped");
          await sleep(Math.min(left, 1000));
          await heartbeat();
        }
      }
    }
    const dlq = dlqSubject(ch.stream);
    const dead: Message = { subject: m.subject, id: m.id, headers: { ...m.headers, [HeaderError]: err instanceof Error ? err.message : String(err) }, data: m.data, delivery: 0 };
    try {
      await this.#ensureTopic(dlq);
      await this.#producer.send({ topic: dlq, messages: [this.#record(dead)], acks: -1 });
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
