// Confluent Schema Registry client over fetch. Mirrors
// events/schemaregistry/client.go.

/** An import of a schema, registered under its own subject. */
export interface Reference {
  /** The import path, e.g. "orders/v1/common.proto". */
  name: string;
  subject: string;
  version: number;
}

/** A registered or candidate schema. */
export interface Schema {
  /** The schema text: .proto source, or base64 of a serialized FileDescriptorProto. */
  schema: string;
  schemaType?: string;
  references?: Reference[];
  /** Set on responses. */
  subject?: string;
  version?: number;
  id?: number;
}

export interface RegistryConfig {
  /** The registry base URL, e.g. "https://sr.example.com:8081". User info in the URL is used for basic auth. */
  url: string;
  /** Basic auth. */
  username?: string;
  password?: string;
  /** Bearer token. Ignored when username is set. */
  token?: string;
  /**
   * Replaces the global fetch. Use it for a custom CA or client
   * certificate (an undici Agent through `dispatcher`) or to add
   * headers.
   */
  fetch?: typeof fetch;
}

/** A registry error response. */
export class RegistryError extends Error {
  readonly status: number;
  /** The registry's error_code. */
  readonly code: number;
  constructor(status: number, code: number, message: string) {
    super(`schema registry: ${status} ${message} (error_code ${code})`);
    this.name = "RegistryError";
    this.status = status;
    this.code = code;
  }
}

const codeSubjectNotFound = 40401;
const codeVersionNotFound = 40402;
const codeSchemaNotFound = 40403;

/** Whether err is a registry "not found" (subject, version or schema). */
export function isNotFound(err: unknown): boolean {
  if (!(err instanceof RegistryError)) return false;
  return err.status === 404 || err.code === codeSubjectNotFound || err.code === codeVersionNotFound || err.code === codeSchemaNotFound;
}

export interface Compatibility {
  isCompatible: boolean;
  messages: string[];
}

/**
 * A Schema Registry client. It caches schemas by id and subject
 * versions it has fetched by number; both are immutable in a registry.
 */
export class RegistryClient {
  readonly url: string;
  readonly #fetch: typeof fetch;
  readonly #auth: string | undefined;
  readonly #byID = new Map<string, Promise<Schema>>();
  readonly #byVer = new Map<string, Promise<Schema>>();

  constructor(cfg: RegistryConfig) {
    let u: URL;
    try {
      u = new URL(cfg.url);
    } catch (e) {
      throw new Error(`schema registry url: ${(e as Error).message}`);
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error(`schema registry url "${cfg.url}": want http or https`);
    let username = cfg.username ?? "";
    let password = cfg.password ?? "";
    if (u.username !== "" && username === "") {
      username = decodeURIComponent(u.username);
      password = decodeURIComponent(u.password);
    }
    u.username = "";
    u.password = "";
    this.url = u.toString().replace(/\/+$/, "");
    this.#fetch = cfg.fetch ?? globalThis.fetch.bind(globalThis);
    if (username !== "") this.#auth = "Basic " + Buffer.from(`${username}:${password}`).toString("base64");
    else if (cfg.token) this.#auth = "Bearer " + cfg.token;
  }

  async #do<T>(method: string, path: string, query: Record<string, string>, body?: unknown): Promise<T> {
    const q = new URLSearchParams(query).toString();
    const target = this.url + path + (q ? "?" + q : "");
    const headers: Record<string, string> = { Accept: "application/vnd.schemaregistry.v1+json, application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/vnd.schemaregistry.v1+json";
    if (this.#auth) headers["Authorization"] = this.#auth;
    let resp: Response;
    try {
      resp = await this.#fetch(target, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    } catch (e) {
      throw new Error(`schema registry: ${(e as Error).message}`, { cause: e });
    }
    const text = await resp.text();
    if (resp.status >= 300) {
      let code = 0;
      let message = text.trim();
      try {
        const j = JSON.parse(text) as { error_code?: number; message?: string };
        if (j.message) message = j.message;
        code = j.error_code ?? 0;
      } catch {
        // not JSON: keep the text
      }
      throw new RegistryError(resp.status, code, `${method} ${path}: ${message}`);
    }
    try {
      return JSON.parse(text) as T;
    } catch (e) {
      throw new Error(`schema registry: ${method} ${path}: ${(e as Error).message}`, { cause: e });
    }
  }

  /** The schema with id. format "" gives .proto text, "serialized" base64 of a FileDescriptorProto. */
  schemaByID(id: number, format = ""): Promise<Schema> {
    const k = `${id}\0${format}`;
    let p = this.#byID.get(k);
    if (!p) {
      p = this.#do<Schema>("GET", `/schemas/ids/${id}`, format ? { format } : {}).then((s) => ({ ...s, id }));
      p.catch(() => this.#byID.delete(k));
      this.#byID.set(k, p);
    }
    return p;
  }

  /** Version of subject: a number or "latest". Numbered versions are cached; "latest" is fetched every call. */
  subjectVersion(subject: string, version: string | number = "latest", format = ""): Promise<Schema> {
    const v = String(version);
    const get = () => this.#do<Schema>("GET", `/subjects/${encodeURIComponent(subject)}/versions/${encodeURIComponent(v)}`, format ? { format } : {});
    if (v === "latest") return get();
    const k = `${subject}\0${v}\0${format}`;
    let p = this.#byVer.get(k);
    if (!p) {
      p = get();
      p.catch(() => this.#byVer.delete(k));
      this.#byVer.set(k, p);
    }
    return p;
  }

  #body(s: Schema) {
    return { schema: s.schema, schemaType: s.schemaType || "PROTOBUF", references: s.references };
  }

  /** Registers s under subject and returns its id. Registering a schema the subject already holds returns the existing id. */
  async register(subject: string, s: Schema, normalize = false): Promise<number> {
    const out = await this.#do<{ id: number }>("POST", `/subjects/${encodeURIComponent(subject)}/versions`, normalize ? { normalize: "true" } : {}, this.#body(s));
    return out.id;
  }

  /** The registered version of s under subject (with its id and version), or an error isNotFound accepts. */
  lookup(subject: string, s: Schema, normalize = false): Promise<Schema> {
    return this.#do<Schema>("POST", `/subjects/${encodeURIComponent(subject)}`, normalize ? { normalize: "true" } : {}, this.#body(s));
  }

  /** Whether s is compatible with version ("latest" or a number) of subject, at the subject's level. messages explain a failure. */
  async checkCompatibility(subject: string, version: string | number, s: Schema): Promise<Compatibility> {
    const out = await this.#do<{ is_compatible: boolean; messages?: string[] }>(
      "POST",
      `/compatibility/subjects/${encodeURIComponent(subject)}/versions/${encodeURIComponent(String(version || "latest"))}`,
      { verbose: "true" },
      this.#body(s),
    );
    return { isCompatible: out.is_compatible, messages: out.messages ?? [] };
  }

  /** The subject's compatibility level, or the global one when the subject has none. */
  async compatibilityLevel(subject: string): Promise<string> {
    const out = await this.#do<{ compatibilityLevel: string }>("GET", `/config/${encodeURIComponent(subject)}`, { defaultToGlobal: "true" });
    return out.compatibilityLevel;
  }
}
