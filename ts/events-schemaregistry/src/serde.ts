import type { DescMessage } from "@bufbuild/protobuf";
import { HeaderType, type Channel, type Message, type Serde } from "@gwag/events";
import type { RegistryClient } from "./client.ts";
import { ensureFile, fileByID, findMessage, messageAt, type FileShape } from "./schema.ts";
import { frame, MagicByte, messageIndexes, unframe } from "./wire.ts";

/** Set on a decoded message (not on the wire): the schema id the record was framed with. */
export const HeaderSchemaID = "Gwag-Schema-Id";

/** Names the subject of a record's value schema. */
export type SubjectNameStrategy = (topic: string, md: DescMessage) => string;

/** "<topic>-value". Confluent's default; one schema per topic. */
export const topicNameStrategy: SubjectNameStrategy = (topic) => topic + "-value";
/** The message full name. A topic may carry several types. */
export const recordNameStrategy: SubjectNameStrategy = (_topic, md) => md.typeName;
/** "<topic>-<message full name>". */
export const topicRecordNameStrategy: SubjectNameStrategy = (topic, md) => `${topic}-${md.typeName}`;

/** "topic", "record" or "topic-record" (or Confluent's class names). */
export function strategyByName(name: string): SubjectNameStrategy {
  switch (name) {
    case "":
    case "topic":
    case "TopicNameStrategy":
      return topicNameStrategy;
    case "record":
    case "RecordNameStrategy":
      return recordNameStrategy;
    case "topic-record":
    case "TopicRecordNameStrategy":
      return topicRecordNameStrategy;
  }
  throw new Error(`subject name strategy "${name}": want topic, record or topic-record`);
}

export interface SerdeOptions {
  /** Default topicNameStrategy. */
  subjectNameStrategy?: SubjectNameStrategy;
  /**
   * Register the message's file (and its imports, as references) on
   * first publish when the subject does not hold it. Use it for
   * protos gwag owns. Without it (the default) publish only looks the
   * schema up and fails when it is not registered.
   */
  autoRegister?: boolean;
  /**
   * Frame with the subject's latest schema, without comparing it to
   * the local proto (Confluent's use.latest.version). The message must
   * exist in that schema under the same full name.
   */
  useLatest?: boolean;
  /** Frame every publish with this schema id. The message must exist in that schema under the same full name. */
  useSchemaId?: number;
  /** Pass normalize=true to lookups and registrations. */
  normalize?: boolean;
  /** The subject of an imported file. Default: the import path. */
  referenceSubject?: (path: string) => string;
  /**
   * Maps a registry message full name to the local one, for a shadow
   * copy in another package: a record framed as "corp.orders.v1.Order"
   * is delivered with Gwag-Type "shop.v1.OrderShadow". Applied on
   * decode only.
   */
  typeNames?: Record<string, string>;
  /**
   * Registry per channel key ("corp.orders.v1.Order"), used instead of
   * the serde's client on publish and on delivery. For a channel
   * another system produces: its schema ids are that system's, so its
   * records decode only against its registry, or against the gwag
   * context it was imported into
   * (http://gwag:8080/schema-registry/contexts/.corp after
   * `gwag ctl schema import --into .corp`).
   */
  channelClients?: Record<string, RegistryClient>;
}

interface Encoding {
  id: number;
  indexes: number[];
}

/**
 * A Serde with Confluent framing. Publish frames the body with the
 * schema id of the message's file; delivery accepts a framed record
 * from any producer and sets Gwag-Type from the schema id and message
 * indexes when the record has no Gwag-Type header. A record whose
 * first byte is not the magic byte is taken as plain proto (plain
 * proto never starts with 0), so a topic can move to framing without a
 * cut-over.
 */
export class SchemaRegistrySerde implements Serde {
  readonly client: RegistryClient;
  readonly #o: SerdeOptions;
  readonly #strategy: SubjectNameStrategy;
  readonly #ids = new Map<string, Promise<Encoding>>();

  constructor(client: RegistryClient, opts: SerdeOptions = {}) {
    const modes = [opts.autoRegister === true, opts.useLatest === true, opts.useSchemaId !== undefined];
    if (modes.filter(Boolean).length > 1) {
      throw new Error("schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one");
    }
    for (const [key, c] of Object.entries(opts.channelClients ?? {})) {
      if (key === "" || !c) {
        throw new Error(`schema registry: channel client for channel ${JSON.stringify(key)}: want a channel key and a client`);
      }
    }
    this.client = client;
    this.#o = opts;
    this.#strategy = opts.subjectNameStrategy ?? topicNameStrategy;
  }

  /** The subject a publish of md to topic uses. */
  subject(topic: string, md: DescMessage): string {
    return this.#strategy(topic, md);
  }

  /** The registry of ch, and the cache prefix of its encodings. */
  #clientOf(ch: Channel): [RegistryClient, string] {
    const c = this.#o.channelClients?.[ch.key];
    return c ? [c, ch.key] : [this.client, ""];
  }

  async encode(ch: Channel, topic: string, type: DescMessage | string, out: Message): Promise<void> {
    if (typeof type === "string") {
      throw new Error(`message ${type}: the schema registry serde needs the message schema; publish through a generated bus`);
    }
    const enc = await this.#encoding(this.#clientOf(ch), this.#strategy(topic, type), type);
    out.data = frame(enc.id, enc.indexes, out.data);
  }

  #encoding([client, pfx]: [RegistryClient, string], subject: string, md: DescMessage): Promise<Encoding> {
    const o = this.#o;
    const k = `${pfx}\0${subject}\0${md.typeName}`;
    const cached = o.useLatest ? undefined : this.#ids.get(k);
    if (cached) return cached;
    const p = this.#resolve(client, subject, md);
    if (!o.useLatest) {
      p.catch(() => this.#ids.delete(k));
      this.#ids.set(k, p);
    }
    return p;
  }

  async #resolve(client: RegistryClient, subject: string, md: DescMessage): Promise<Encoding> {
    const o = this.#o;
    if (o.useLatest || o.useSchemaId !== undefined) {
      let id = o.useSchemaId ?? 0;
      if (o.useLatest) {
        let v;
        try {
          v = await client.subjectVersion(subject, "latest");
        } catch (e) {
          throw new Error(`latest of ${subject}: ${(e as Error).message}`, { cause: e });
        }
        id = v.id ?? 0;
      }
      const shape = await fileByID(client, id);
      const indexes = findMessage(shape, md.typeName);
      if (!indexes) throw new Error(`schema id ${id} (subject ${subject}) has no message ${md.typeName}`);
      return { id, indexes };
    }
    const r = await ensureFile(client, subject, md.file, {
      register: o.autoRegister === true,
      normalize: o.normalize,
      referenceSubject: o.referenceSubject,
    });
    return { id: r.id ?? 0, indexes: messageIndexes(md) };
  }

  async decode(ch: Channel, _topic: string, m: Message): Promise<void> {
    if (m.data.length === 0 || m.data[0] !== MagicByte) return; // plain proto
    const { id, indexes, body } = unframe(m.data);
    const shape: FileShape = await fileByID(this.#clientOf(ch)[0], id);
    let full: string;
    try {
      full = messageAt(shape, indexes);
    } catch (e) {
      throw new Error(`schema id ${id}: ${(e as Error).message}`, { cause: e });
    }
    const name = this.#o.typeNames?.[full] ?? full;
    const t = m.headers[HeaderType];
    if (t === undefined || t === "") m.headers[HeaderType] = name;
    else if (t !== name && t !== full) throw new Error(`${HeaderType} header is "${t}" but schema id ${id} frames ${full}`);
    m.headers[HeaderSchemaID] = String(id);
    m.data = body;
  }
}
