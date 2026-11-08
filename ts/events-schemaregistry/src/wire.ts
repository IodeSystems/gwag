// Confluent's protobuf framing: magic byte 0, a 4-byte big-endian
// schema id, the message-index array, then the proto body. Mirrors
// events/schemaregistry/wire.go.
import type { DescMessage } from "@bufbuild/protobuf";

/** Starts every framed record. */
export const MagicByte = 0;

function appendVarint(out: number[], n: number): void {
  // Zigzag, then base-128 little endian. Numbers stay below 2^53.
  let u = n >= 0 ? n * 2 : -n * 2 - 1;
  while (u >= 0x80) {
    out.push((u % 0x80) | 0x80);
    u = Math.floor(u / 0x80);
  }
  out.push(u);
}

/**
 * Returns Confluent's protobuf framing of body: MagicByte, the schema
 * id (4 bytes, big endian), the message indexes, body.
 *
 * The indexes locate the message in its schema file: [i] is the i-th
 * top-level message, [i, j] its j-th nested message. They are written
 * as a zigzag varint count and zigzag varints; [0], the first
 * top-level message, is written as a single 0.
 */
export function frame(id: number, indexes: readonly number[], body: Uint8Array): Uint8Array {
  const head: number[] = [MagicByte, (id >>> 24) & 0xff, (id >>> 16) & 0xff, (id >>> 8) & 0xff, id & 0xff];
  if (indexes.length === 1 && indexes[0] === 0) {
    appendVarint(head, 0);
  } else {
    appendVarint(head, indexes.length);
    for (const i of indexes) appendVarint(head, i);
  }
  const out = new Uint8Array(head.length + body.length);
  out.set(head, 0);
  out.set(body, head.length);
  return out;
}

/** Thrown by unframe for data that does not start with MagicByte. */
export class NotFramedError extends Error {
  constructor() {
    super("schema registry: data does not start with the magic byte");
    this.name = "NotFramedError";
  }
}

function readVarint(data: Uint8Array, at: number): { v: number; next: number } | undefined {
  let u = 0;
  let mul = 1;
  for (let i = at; i < data.length && i < at + 8; i++) {
    const b = data[i]!;
    u += (b & 0x7f) * mul;
    if (b < 0x80) return { v: u % 2 === 0 ? u / 2 : -(u + 1) / 2, next: i + 1 };
    mul *= 0x80;
  }
  return undefined;
}

/** Splits framed data into schema id, message indexes and body. */
export function unframe(data: Uint8Array): { id: number; indexes: number[]; body: Uint8Array } {
  if (data.length < 5 || data[0] !== MagicByte) throw new NotFramedError();
  const id = ((data[1]! << 24) | (data[2]! << 16) | (data[3]! << 8) | data[4]!) >>> 0;
  const n = readVarint(data, 5);
  if (!n) throw new Error("schema registry: bad message index count");
  if (n.v === 0) return { id, indexes: [0], body: data.subarray(n.next) };
  if (n.v < 0 || n.v > 1024) throw new Error(`schema registry: message index count ${n.v}`);
  const indexes: number[] = [];
  let at = n.next;
  for (let i = 0; i < n.v; i++) {
    const r = readVarint(data, at);
    if (!r || r.v < 0) throw new Error("schema registry: bad message index");
    indexes.push(r.v);
    at = r.next;
  }
  return { id, indexes, body: data.subarray(at) };
}

/** md's indexes in its file: [i] top-level, [i, j] nested. */
export function messageIndexes(md: DescMessage): number[] {
  const out: number[] = [];
  for (let d: DescMessage | undefined = md; d; d = d.parent) {
    const siblings = d.parent ? d.parent.nestedMessages : d.file.messages;
    out.unshift(siblings.indexOf(d));
  }
  return out;
}
