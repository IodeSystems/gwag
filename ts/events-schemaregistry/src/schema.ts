// Schemas as the registry holds them: what a generated file registers
// as, and what a fetched schema says about its messages. Mirrors
// events/schemaregistry/schema.go.
import { clone, fromBinary, toBinary, type DescFile } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema, type DescriptorProto, type FieldDescriptorProto, type FileDescriptorProto } from "@bufbuild/protobuf/wkt";
import { isNotFound, type Reference, type RegistryClient, type Schema } from "./client.ts";

/**
 * Whether a registry knows the import path without a reference:
 * google/protobuf/*, google/type/* and confluent/*.
 */
export function builtin(path: string): boolean {
  return path.startsWith("google/protobuf/") || path.startsWith("google/type/") || path.startsWith("confluent/");
}

/** Drops what differs between compilers of the same source (see canonical in schema.go). */
function canonical(fdp: FileDescriptorProto): void {
  fdp.sourceCodeInfo = undefined;
  const clear = (fs: FieldDescriptorProto[]) => {
    for (const f of fs) f.proto3Optional = false;
  };
  clear(fdp.extension);
  const walk = (ms: DescriptorProto[]) => {
    for (const m of ms) {
      clear(m.extension);
      walk(m.nestedType);
    }
  };
  walk(fdp.messageType);
}

/**
 * file as a registry schema: base64 of its FileDescriptorProto
 * (without source info), the format Confluent's own serializers
 * register. refs are its non-builtin imports.
 */
export function schemaOf(file: DescFile, refs: Reference[] = []): Schema {
  const fdp = clone(FileDescriptorProtoSchema, file.proto);
  canonical(fdp);
  const b = toBinary(FileDescriptorProtoSchema, fdp);
  return { schema: Buffer.from(b).toString("base64"), schemaType: "PROTOBUF", references: refs.length > 0 ? refs : undefined };
}

export interface RegisterOptions {
  /** Register the file and its references where missing. Without it every one must already be registered. */
  register?: boolean;
  /** Pass normalize=true. */
  normalize?: boolean;
  /** The subject of an import. Default: the import path, as Confluent's serializers do. */
  referenceSubject?: (path: string) => string;
}

/** A lookup that found no registered schema. */
export class NotRegisteredError extends Error {
  constructor(message: string, cause?: unknown) {
    super(message, { cause });
    this.name = "NotRegisteredError";
  }
}

const ensured = new WeakMap<RegistryClient, Map<string, Promise<Schema>>>();

/**
 * Finds (or, with register, registers) file under subject and each
 * non-builtin import under its reference subject, imports first.
 * Returns the registered root: subject, version, id.
 */
export function ensureFile(c: RegistryClient, subject: string, file: DescFile, o: RegisterOptions = {}): Promise<Schema> {
  let cache = ensured.get(c);
  if (!cache) ensured.set(c, (cache = new Map()));
  const k = `${subject}\0${file.proto.name}\0${o.register ? 1 : 0}\0${o.normalize ? 1 : 0}`;
  let p = cache.get(k);
  if (!p) {
    p = doEnsure(c, subject, file, o);
    const hit = cache;
    p.catch(() => hit.delete(k));
    cache.set(k, p);
  }
  return p;
}

async function doEnsure(c: RegistryClient, subject: string, file: DescFile, o: RegisterOptions): Promise<Schema> {
  const refSubject = o.referenceSubject ?? ((p: string) => p);
  const refs: Reference[] = [];
  for (const dep of file.dependencies) {
    const path = dep.proto.name;
    if (builtin(path)) continue;
    const refSubj = refSubject(path);
    const r = await ensureFile(c, refSubj, dep, o);
    // The subject asked for, not the one answered: a registry URL with
    // a context (/contexts/.corp) answers ":.corp:<subject>".
    refs.push({ name: path, subject: refSubj, version: r.version ?? 0 });
  }
  const s = schemaOf(file, refs);
  if (o.register) {
    try {
      await c.register(subject, s, o.normalize);
    } catch (e) {
      throw new Error(`register ${file.proto.name} under ${subject}: ${(e as Error).message}`, { cause: e });
    }
  }
  let found: Schema;
  try {
    found = await c.lookup(subject, s, o.normalize);
  } catch (e) {
    if (isNotFound(e)) throw new NotRegisteredError(`${file.proto.name} under subject ${subject}: schema not registered (${(e as Error).message})`, e);
    throw e;
  }
  if (!found.subject) found = { ...found, subject };
  return found;
}

/** The message names of a schema file: enough to name the message a frame points at. */
export interface FileShape {
  package: string;
  messages: MessageShape[];
}

export interface MessageShape {
  name: string;
  messages: MessageShape[];
}

function shapeOf(ms: DescriptorProto[]): MessageShape[] {
  return ms.map((m) => ({ name: m.name, messages: shapeOf(m.nestedType) }));
}

/** The shape of a FileDescriptorProto. */
export function shapeOfFile(fdp: FileDescriptorProto): FileShape {
  return { package: fdp.package, messages: shapeOf(fdp.messageType) };
}

/**
 * The shape of .proto source text: package and the nesting of message
 * declarations. A fallback for a registry that cannot serve a schema
 * as format=serialized.
 */
export function shapeOfText(text: string): FileShape {
  const src = text.replace(/\/\*[\s\S]*?\*\//g, " ").replace(/\/\/[^\n]*/g, " ").replace(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'/g, '""');
  const re = /[A-Za-z_][\w.]*|[{};]/g;
  const out: FileShape = { package: "", messages: [] };
  // Each open block: its message list when it is a message, else undefined.
  const stack: (MessageShape | undefined)[] = [];
  let prev = "";
  let prev2 = "";
  for (let t = re.exec(src); t; t = re.exec(src)) {
    const tok = t[0];
    if (tok === "{") {
      const top = stack[stack.length - 1];
      const inMessageScope = stack.length === 0 || top !== undefined;
      if (prev2 === "message" && inMessageScope) {
        const m: MessageShape = { name: prev, messages: [] };
        (top ? top.messages : out.messages).push(m);
        stack.push(m);
      } else {
        stack.push(undefined);
      }
    } else if (tok === "}") {
      stack.pop();
    } else if (tok === ";" && prev2 === "package" && stack.length === 0) {
      out.package = prev;
    }
    prev2 = prev;
    prev = tok;
  }
  return out;
}

const files = new WeakMap<RegistryClient, Map<number, Promise<FileShape>>>();

/** The shape of schema id. Cached per client. */
export function fileByID(c: RegistryClient, id: number): Promise<FileShape> {
  let cache = files.get(c);
  if (!cache) files.set(c, (cache = new Map()));
  let p = cache.get(id);
  if (!p) {
    p = loadShape(c, id);
    const hit = cache;
    p.catch(() => hit.delete(id));
    cache.set(id, p);
  }
  return p;
}

async function loadShape(c: RegistryClient, id: number): Promise<FileShape> {
  const s = await c.schemaByID(id, "serialized");
  if (s.schemaType && s.schemaType !== "PROTOBUF") throw new Error(`schema id ${id} is ${s.schemaType}, not PROTOBUF`);
  // A serialized descriptor is base64; .proto text has ';' and '{'.
  if (/^[A-Za-z0-9+/=\s]+$/.test(s.schema)) {
    try {
      return shapeOfFile(fromBinary(FileDescriptorProtoSchema, Buffer.from(s.schema.trim(), "base64")));
    } catch {
      // not a descriptor: ask for the text
    }
    return shapeOfText((await c.schemaByID(id)).schema);
  }
  return shapeOfText(s.schema);
}

/** The full name of the message at indexes in shape. */
export function messageAt(shape: FileShape, indexes: readonly number[]): string {
  if (indexes.length === 0) throw new Error("schema registry: no message indexes");
  let ms = shape.messages;
  const names: string[] = [];
  for (const i of indexes) {
    const m = ms[i];
    if (!m) throw new Error(`schema registry: message index [${indexes}] not in schema`);
    names.push(m.name);
    ms = m.messages;
  }
  return (shape.package ? shape.package + "." : "") + names.join(".");
}

/** The indexes of the message named full in shape, top-level or nested, or undefined. */
export function findMessage(shape: FileShape, full: string): number[] | undefined {
  const prefix = shape.package ? shape.package + "." : "";
  const walk = (ms: MessageShape[], base: string): number[] | undefined => {
    for (let i = 0; i < ms.length; i++) {
      const name = base + ms[i]!.name;
      if (name === full) return [i];
      const sub = walk(ms[i]!.messages, name + ".");
      if (sub) return [i, ...sub];
    }
    return undefined;
  };
  return walk(shape.messages, prefix);
}
