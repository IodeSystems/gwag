import {
  canonicalHeaderKey,
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
  type Stop,
  type SubscribeOptions,
  type Subscription,
} from "./events.ts";

export interface CoreOptions {
  /** Logger for skipped messages. Default console.warn. */
  logf?: (msg: string) => void;
}

/** Gets the wildcard segment values (pattern order) of the delivered subject. */
export type SegmentHandler = (segments: string[], m: Message) => Promise<void> | void;

/**
 * What a generated bus wraps: fills subjects, records subscriptions
 * and calls the Backend. Encoding stays in the generated code.
 */
export class Core {
  readonly backend: Backend;
  readonly #logf: (msg: string) => void;
  readonly #subs: Subscription[] = [];
  readonly #warned = new Set<string>();

  constructor(backend: Backend, opts: CoreOptions = {}) {
    this.backend = backend;
    this.#logf = opts.logf ?? ((m) => console.warn(m));
  }

  /**
   * Publishes data (the proto body of message type typeName) on the
   * subject built from segments (in pattern order).
   */
  async publish(ch: Channel, segments: readonly string[], typeName: string, data: Uint8Array, opts: PublishOptions = {}): Promise<void> {
    let subject: string;
    try {
      subject = ch.pattern.subject(segments);
    } catch (e) {
      throw new Error(`publish ${ch.key}: ${(e as Error).message}`);
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(opts.headers ?? {})) headers[canonicalHeaderKey(k)] = v;
    headers[HeaderType] = typeName;
    headers[HeaderChannel] = ch.key;
    await this.backend.publish(ch, { subject, id: opts.msgId || newID(), headers, data, delivery: 0 });
  }

  /**
   * Listens on ch as group. Messages whose subject does not match the
   * pattern are skipped (acked).
   */
  async subscribe(ch: Channel, group: string, h: SegmentHandler, opts?: SubscribeOptions): Promise<Stop> {
    if (!validGroup(group)) {
      throw new Error(`subscribe ${ch.key}: group "${group}": use letters, digits, '_' or '-'`);
    }
    const stop = await this.backend.subscribe(ch, group, resolveSubscribeOptions(opts), async (m) => {
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
