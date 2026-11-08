import type { DescMessage } from "@bufbuild/protobuf";
import {
  canonicalHeaderKey,
  hasTopics,
  ProtoSerde,
  HeaderChannel,
  HeaderType,
  messageType,
  newID,
  resolveSubscribeOptions,
  validGroup,
  type Backend,
  type Channel,
  type Message,
  type PublishOptions,
  type Serde,
  type Stop,
  type SubscribeOptions,
  type Subscription,
} from "./events.ts";

export interface CoreOptions {
  /** Logger for skipped messages. Default console.warn. */
  logf?: (msg: string) => void;
  /** Wire format of the body. Default ProtoSerde (plain proto binary). */
  serde?: Serde;
}

/** Gets the wildcard segment values (pattern order) of the delivered subject. */
export type SegmentHandler = (segments: string[], m: Message) => Promise<void> | void;

/**
 * What a generated bus wraps: fills subjects, records subscriptions
 * and calls the Backend. Encoding stays in the generated code.
 */
export class Core {
  readonly backend: Backend;
  readonly serde: Serde;
  readonly #logf: (msg: string) => void;
  readonly #subs: Subscription[] = [];
  readonly #warned = new Set<string>();

  constructor(backend: Backend, opts: CoreOptions = {}) {
    this.backend = backend;
    this.serde = opts.serde ?? new ProtoSerde();
    this.#logf = opts.logf ?? ((m) => console.warn(m));
  }

  #topic(ch: Channel): string {
    return hasTopics(this.backend) ? this.backend.topic(ch) : ch.stream;
  }

  /**
   * Publishes data (the proto body of message type type) on the
   * subject built from segments (in pattern order). type is the
   * message schema (generated code passes it), or only its full name.
   */
  async publish(ch: Channel, segments: readonly string[], type: DescMessage | string, data: Uint8Array, opts: PublishOptions = {}): Promise<void> {
    let subject: string;
    try {
      subject = ch.pattern.subject(segments);
    } catch (e) {
      throw new Error(`publish ${ch.key}: ${(e as Error).message}`);
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(opts.headers ?? {})) headers[canonicalHeaderKey(k)] = v;
    headers[HeaderType] = typeof type === "string" ? type : type.typeName;
    headers[HeaderChannel] = ch.key;
    const msg: Message = { subject, id: opts.msgId || newID(), headers, data, delivery: 0 };
    if (!(this.serde instanceof ProtoSerde)) {
      try {
        await this.serde.encode(ch, this.#topic(ch), type, msg);
      } catch (e) {
        throw new Error(`publish ${ch.key}: encode: ${(e as Error).message}`, { cause: e });
      }
    }
    await this.backend.publish(ch, msg);
  }

  /**
   * Listens on ch as group. Messages whose subject does not match the
   * pattern are skipped (acked).
   */
  async subscribe(ch: Channel, group: string, h: SegmentHandler, opts?: SubscribeOptions): Promise<Stop> {
    if (!validGroup(group)) {
      throw new Error(`subscribe ${ch.key}: group "${group}": use letters, digits, '_' or '-'`);
    }
    const topic = this.#topic(ch);
    const plain = this.serde instanceof ProtoSerde;
    const stop = await this.backend.subscribe(ch, group, resolveSubscribeOptions(opts), async (raw) => {
      let m = raw;
      if (!plain) {
        m = { ...raw, headers: { ...raw.headers } };
        try {
          await this.serde.decode(ch, topic, m);
        } catch (e) {
          throw new Error(`decode ${ch.key}: ${(e as Error).message}`, { cause: e });
        }
      }
      const segs = ch.pattern.match(m.subject);
      if (!segs) {
        this.skip(ch, m, `subject ${m.subject} does not match ${ch.pattern.raw}`);
        return;
      }
      await h(segs, m);
    });
    if (!this.#subs.some((s) => s.channel === ch.key && s.group === group)) {
      this.#subs.push({ channel: ch.key, group });
    }
    return stop;
  }

  /** Logs, once per (channel, message type), that a message was acked without a handler. */
  skip(ch: Channel, m: Message, why: string): void {
    const k = `${ch.key}\u0000${messageType(m)}`;
    if (this.#warned.has(k)) return;
    this.#warned.add(k);
    this.#logf(`events: ${ch.key}: skipped message type "${messageType(m)}": ${why} (logged once)`);
  }

  /** Every (channel, group) subscribe was called with, in call order. Stopping does not remove one. */
  subscriptions(): Subscription[] {
    return [...this.#subs];
  }
}
