// Types shared by generated code and backends. Mirrors events/events.go.
import type { DescMessage } from "@bufbuild/protobuf";
import { parsePattern, type Pattern } from "./pattern.ts";

/** Header names on every message. */
export const HeaderType = "Gwag-Type";
export const HeaderChannel = "Gwag-Channel";
/** Message id on NATS (JetStream dedup). */
export const HeaderNatsMsgID = "Nats-Msg-Id";
/** Message id on backends other than NATS. */
export const HeaderMsgID = "Gwag-Msg-Id";
/** Added to a dead-lettered message: the last handler error. */
export const HeaderError = "Gwag-Error";

/** A dead letter goes to DLQPrefix + stream name. */
export const DLQPrefix = "gwag.dlq.";

export interface Channel {
  /** "<proto package>.<name>", e.g. "orders.v1.OrderEvents". */
  key: string;
  pattern: Pattern;
  /** Stream (or topic) name: streamName(key). */
  stream: string;
}

/** "EV_" + key with "." replaced by "_". */
export function streamName(key: string): string {
  return "EV_" + key.replaceAll(".", "_");
}

export function dlqSubject(stream: string): string {
  return DLQPrefix + stream;
}

/** Validates key and pattern. Generated code calls it. */
export function channel(key: string, pattern: string): Channel {
  if (key === "") throw new Error("channel key is empty");
  let p: Pattern;
  try {
    p = parsePattern(pattern);
  } catch (e) {
    throw new Error(`channel ${key}: ${(e as Error).message}`);
  }
  return { key, pattern: p, stream: streamName(key) };
}

/** One message on the wire. */
export interface Message {
  /** The concrete subject. */
  subject: string;
  /** Message id. The backend carries it in its own header. */
  id: string;
  /** HeaderType, HeaderChannel and any extra headers. Keys are canonical ("Gwag-Type"). */
  headers: Record<string, string>;
  /** Proto binary body. */
  data: Uint8Array;
  /** 1-based delivery attempt. Set on receive. */
  delivery: number;
  /** The backend's record key, where it has one (Kafka). Set on receive; undefined on NATS. */
  key?: Uint8Array;
}

/** The proto full name of the body: the HeaderType value. */
export function messageType(m: Message): string {
  return m.headers[HeaderType] ?? "";
}

/**
 * Handles one delivery. Resolving acks it; throwing (or rejecting)
 * asks for redelivery.
 */
export type Handler = (m: Message) => Promise<void> | void;

/** Ends one subscriber. The group's position stays. Does not wait for a running handler. */
export type Stop = () => Promise<void>;

/**
 * Moves messages. Implementations own delivery: ack on success,
 * redeliver after delayMs(opts, n) on error, and after maxDeliver
 * failed deliveries publish the message to dlqSubject(ch.stream) with
 * the original headers plus HeaderError, then drop it.
 */
export interface Backend {
  /** Sends m on m.subject, which matches ch.pattern. */
  publish(ch: Channel, m: Message): Promise<void>;
  /**
   * Delivers messages of ch to h. All subscribers with the same group
   * share one queue: each message goes to one of them. Each group gets
   * every message.
   */
  subscribe(ch: Channel, group: string, opts: ResolvedSubscribeOptions, h: Handler): Promise<Stop>;
  close(): Promise<void>;
}

/** One (channel, group) a bus listens on. Pass these to the control plane. */
export interface Subscription {
  channel: string;
  group: string;
}

/** What a caller of onX(...) may set. */
export interface SubscribeOptions {
  /** Delivery attempts before dead-lettering. Default 5. */
  maxDeliver?: number;
  /** Start a new group at the first stored message. Default: new messages only. */
  deliverAll?: boolean;
  /** Redelivery delay after failed attempt n (backoffMs[n-1], last repeats). Default 1s, 5s, 30s. */
  backoffMs?: number[];
  /** How long a delivery may run before the backend redelivers it. Default 30s. */
  ackWaitMs?: number;
  /** Backend-specific settings, keyed by the backend's own names (e.g. "kafka.retry"). Backends ignore keys they do not own. */
  backend?: Record<string, unknown>;
}

/** SubscribeOptions with defaults applied; what a backend receives. */
export interface ResolvedSubscribeOptions {
  maxDeliver: number;
  deliverAll: boolean;
  backoffMs: number[];
  ackWaitMs: number;
  backend: Record<string, unknown>;
}

export function resolveSubscribeOptions(o: SubscribeOptions = {}): ResolvedSubscribeOptions {
  return {
    maxDeliver: Math.max(1, o.maxDeliver ?? 5),
    deliverAll: o.deliverAll ?? false,
    backoffMs: o.backoffMs ? [...o.backoffMs] : [1000, 5000, 30000],
    ackWaitMs: o.ackWaitMs ?? 30000,
    backend: { ...o.backend },
  };
}

/** Redelivery delay after failed delivery n (1-based). */
export function delayMs(o: ResolvedSubscribeOptions, n: number): number {
  if (o.backoffMs.length === 0) return 0;
  const i = Math.min(Math.max(n, 1), o.backoffMs.length);
  return o.backoffMs[i - 1]!;
}

export interface PublishOptions {
  /** Message id. Default random. Republishing an id inside the backend's dedup window (JetStream: 2m) stores the message once. */
  msgId?: string;
  /** Extra headers. Gwag-Type and Gwag-Channel cannot be overridden. */
  headers?: Record<string, string>;
}

const groupRE = /^[A-Za-z0-9_-]+$/;

/** Letters, digits, "_" and "-". */
export function validGroup(group: string): boolean {
  return groupRE.test(group);
}

/** A random message id: 32 hex chars. */
export function newID(): string {
  const b = new Uint8Array(16);
  globalThis.crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

/** Canonical header key as Go's net/textproto writes it: "gwag-type" -> "Gwag-Type". */
export function canonicalHeaderKey(k: string): string {
  if (!/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(k)) return k;
  let up = true;
  let out = "";
  for (const c of k) {
    out += up ? c.toUpperCase() : c.toLowerCase();
    up = c === "-";
  }
  return out;
}

/**
 * Turns a message into its wire body and back. Core calls it on every
 * publish and every delivery, so any backend carries any wire format.
 * The default, ProtoSerde, leaves the body plain proto binary. Mirrors
 * events.Serde in Go; the one difference is that encode starts with
 * out.data holding the plain proto body.
 */
export interface Serde {
  /**
   * Rewrites out.data (plain proto binary on entry) to the wire body.
   * out already holds subject, id and the Gwag-Type / Gwag-Channel
   * headers; encode may add headers. topic is the backend's name for
   * the channel (Topics.topic, else ch.stream). type is the message
   * schema, or only its full name when the caller published raw bytes.
   */
  encode(ch: Channel, topic: string, type: DescMessage | string, out: Message): Promise<void> | void;
  /**
   * Rewrites m.data to plain proto binary of the body message. When m
   * has no Gwag-Type header (a record from a producer that is not
   * gwag) decode sets it if the wire format names the type. m is a
   * copy; the backend keeps the original for redelivery and dead
   * letters.
   */
  decode(ch: Channel, topic: string, m: Message): Promise<void> | void;
}

/** The default Serde: the body is plain proto binary, nothing else. */
export class ProtoSerde implements Serde {
  encode(): void {}
  decode(): void {}
}

/**
 * Implemented by a backend whose name for a channel is not ch.stream
 * (the Kafka backend's topic mapper). Core passes the name to the
 * Serde, which may derive a schema subject from it.
 */
export interface Topics {
  topic(ch: Channel): string;
}

export function hasTopics(b: object): b is Topics {
  return typeof (b as Partial<Topics>).topic === "function";
}
