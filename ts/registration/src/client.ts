// Light registration client for gwag's control plane. Announce the
// service, wait for the gateway's ok, keep the registration alive,
// deregister on close. Mirrors gw/controlclient (Go); see
// docs/registration-clients.md.
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, type Client, type Interceptor } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";
import {
  ControlPlane,
  DependencySchema,
  InstanceState,
  OwnerSchema,
  RegisterRequestSchema,
  ServiceBindingSchema,
  SubscriptionSchema,
  VerificationState,
  VerifySchema,
  type BindingStatus as WireBindingStatus,
  type RegisterRequest,
} from "./gen/gw/proto/controlplane/v1/control_pb.ts";

/** One event channel a binding listens on and the consumer group it listens as. */
export interface Subscription {
  /** "<proto package>.<channel name>", e.g. "orders.v1.OrderEvents". */
  channel: string;
  group: string;
}

/** A provider service the binding calls through the gateway, with the lines it accepts ("v1", "unstable"). */
export interface Dependency {
  service: string;
  lines: string[];
}

export interface Owner {
  team: string;
  contact?: string;
}

/**
 * One binding to register. Set exactly one of protoSource, openapiSpec,
 * graphqlEndpoint. Same fields as controlclient.Service in Go.
 */
export interface Service {
  /** GraphQL namespace. Empty: the proto's file stem / the spec's title. */
  namespace?: string;
  /** "unstable" or "vN". Empty: "v1". */
  version?: string;
  /** Raw .proto source of the entrypoint, as written on disk. */
  protoSource?: string | Uint8Array;
  /** Transitive imports keyed by import path. */
  protoImports?: Record<string, string | Uint8Array>;
  /** OpenAPI 3.x document (JSON or YAML). serviceAddr is then the HTTP base URL. */
  openapiSpec?: string | Uint8Array;
  /** Upstream GraphQL endpoint URL. */
  graphqlEndpoint?: string;
  maxConcurrency?: number;
  maxConcurrencyPerInstance?: number;
  depends?: Dependency[];
  owner?: Owner;
  /** Show this binding to admin readers that present no token. */
  public?: boolean;
  /** Event channels this binding listens on. */
  subscribes?: Subscription[];
  /**
   * Where each instance serves its health check: a path or an absolute
   * URL on the service's host. When this or specUrl is set the gateway
   * verifies each instance before routing to it (see
   * ServiceBinding.verify in control.proto).
   */
  healthUrl?: string;
  /** Where each instance serves this binding's spec; the gateway compares it with the registered one. */
  specUrl?: string;
}

export type State = "ready" | "waiting" | "degraded";

export interface DependencyStatus {
  service: string;
  lines: string[];
  presentLines: string[];
  channel: string;
  group: string;
}

/** The gateway's verification of an instance: "" (not verified), "pending", "green" or "red". */
export type Verification = "" | "pending" | "green" | "red";

export interface BindingStatus {
  namespace: string;
  version: string;
  state: State;
  /** "" when the binding declares no healthUrl / specUrl. A binding that is not green is waiting. */
  verification: Verification;
  /** The failed checks when verification is "red". */
  verifyReasons: string[];
  depends: DependencyStatus[];
}

export interface Options {
  /**
   * Control-plane address of the gateway: "host:port" (plain HTTP/2),
   * or a full "http://..." / "https://..." URL. Several addresses are
   * tried in order; the registration lives on one gateway at a time and
   * moves to the next when a heartbeat cannot reach it.
   */
  gateway: string | string[];
  /** host:port where this service's gRPC server is reachable (the HTTP base URL for OpenAPI bindings). */
  serviceAddr: string;
  services: Service[];
  /** Application key, sent as gRPC metadata `authorization: Bearer <key>`. */
  applicationKey?: string;
  /** Echoed in ListRegistrations. */
  instanceId?: string;
  /** Registration lifetime without a heartbeat. 0 or unset: the gateway default (30s). */
  ttlSeconds?: number;
  /** Heartbeat period in ms. Default: ttl/3, or 10s when the gateway's ttl is unknown. */
  heartbeatIntervalMs?: number;
  /** Poll period in ms while a binding is WAITING on its dependencies. Default 1000. */
  readyPollIntervalMs?: number;
  /** Aborts the initial register-and-wait. */
  signal?: AbortSignal;
  /**
   * Deregister and exit on these signals (default none). The process
   * re-raises the signal after the deregister, so it exits as it would
   * have. Pass ["SIGTERM", "SIGINT"] for a service with no handler of
   * its own; otherwise call close() from yours.
   */
  deregisterOnSignal?: NodeJS.Signals[];
  /** Non-fatal events. Default: console.error. */
  logger?: (message: string) => void;
}

const retryMin = 250;
const retryMax = 5000;
const heartbeatTimeoutMax = 5000;
const registerTimeout = 10000;
const waitLogEvery = 30000;

function retryable(err: unknown): boolean {
  const code = ConnectError.from(err).code;
  return (
    code === Code.Unavailable ||
    code === Code.DeadlineExceeded ||
    code === Code.ResourceExhausted ||
    code === Code.Aborted
  );
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal!.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function bytes(v: string | Uint8Array | undefined): Uint8Array {
  if (v === undefined) return new Uint8Array();
  return typeof v === "string" ? new TextEncoder().encode(v) : v;
}

function stateOf(s: InstanceState): State {
  switch (s) {
    case InstanceState.WAITING:
      return "waiting";
    case InstanceState.DEGRADED:
      return "degraded";
    default:
      return "ready";
  }
}

function verificationOf(v: VerificationState): Verification {
  switch (v) {
    case VerificationState.PENDING:
      return "pending";
    case VerificationState.GREEN:
      return "green";
    case VerificationState.RED:
      return "red";
    default:
      return "";
  }
}

function bindingsOf(list: WireBindingStatus[]): BindingStatus[] {
  return list.map((b) => ({
    namespace: b.namespace,
    version: b.version,
    state: stateOf(b.state),
    verification: verificationOf(b.verification),
    verifyReasons: [...b.verifyReasons],
    depends: b.depends.map((d) => ({
      service: d.service,
      lines: [...d.lines],
      presentLines: [...d.presentLines],
      channel: d.channel,
      group: d.group,
    })),
  }));
}

/** The register request for opts: one binding per Service. Exported for tests. */
export function buildRequest(opts: Pick<Options, "serviceAddr" | "services" | "instanceId" | "ttlSeconds">): RegisterRequest {
  if (opts.services.length === 0) throw new Error("registration: at least one service required");
  return create(RegisterRequestSchema, {
    addr: opts.serviceAddr,
    instanceId: opts.instanceId ?? "",
    ttlSeconds: opts.ttlSeconds ?? 0,
    services: opts.services.map((s) => {
      const name = s.namespace ?? "";
      const set = [s.protoSource, s.openapiSpec, s.graphqlEndpoint].filter((x) => x !== undefined && x.length !== 0).length;
      if (set !== 1) {
        throw new Error(`service ${JSON.stringify(name)}: set exactly one of protoSource, openapiSpec, graphqlEndpoint`);
      }
      return create(ServiceBindingSchema, {
        namespace: name,
        version: s.version ?? "",
        protoSource: bytes(s.protoSource),
        protoImports: Object.fromEntries(Object.entries(s.protoImports ?? {}).map(([k, v]) => [k, bytes(v)])),
        openapiSpec: bytes(s.openapiSpec),
        graphqlEndpoint: s.graphqlEndpoint ?? "",
        maxConcurrency: s.maxConcurrency ?? 0,
        maxConcurrencyPerInstance: s.maxConcurrencyPerInstance ?? 0,
        depends: (s.depends ?? []).map((d) => create(DependencySchema, { service: d.service, lines: d.lines })),
        owner: s.owner ? create(OwnerSchema, { team: s.owner.team, contact: s.owner.contact ?? "" }) : undefined,
        public: s.public ?? false,
        subscribes: (s.subscribes ?? []).map((x) => create(SubscriptionSchema, { channel: x.channel, group: x.group })),
        verify:
          s.healthUrl || s.specUrl
            ? create(VerifySchema, { healthUrl: s.healthUrl ?? "", specUrl: s.specUrl ?? "" })
            : undefined,
      });
    }),
  });
}

interface Gateway {
  addr: string;
  client: Client<typeof ControlPlane>;
  close(): void;
}

function dial(addr: string, key: string | undefined): Gateway {
  const baseUrl = /^https?:\/\//.test(addr) ? addr : `http://${addr}`;
  const interceptors: Interceptor[] = key
    ? [
        (next) => (req) => {
          req.header.set("authorization", `Bearer ${key}`);
          return next(req);
        },
      ]
    : [];
  const transport = createGrpcTransport({ baseUrl, interceptors });
  return { addr, client: createClient(ControlPlane, transport), close: () => {} };
}

/**
 * A live registration. close() stops the heartbeat and deregisters.
 * Created by register().
 */
export class Registration {
  #gws: Gateway[];
  #cur = 0;
  #id = "";
  #ttlMs = 0;
  #bindings: BindingStatus[] = [];
  #opts: Options;
  #log: (m: string) => void;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #closed = false;
  #closing: Promise<void> | undefined;
  #signalHandlers: [NodeJS.Signals, () => void][] = [];
  #beating: Promise<void> = Promise.resolve();

  /** @internal use register() */
  constructor(opts: Options) {
    this.#opts = opts;
    this.#log = opts.logger ?? ((m) => console.error(m));
    const addrs = [...new Set((Array.isArray(opts.gateway) ? opts.gateway : [opts.gateway]).filter((a) => a !== ""))];
    if (addrs.length === 0) throw new Error("registration: gateway address required");
    if (opts.serviceAddr === "") throw new Error("registration: serviceAddr required");
    buildRequest(opts); // validate now
    this.#gws = addrs.map((a) => dial(a, opts.applicationKey));
  }

  /** Registration id. Changes when the registration is made again (gateway restart, failover). */
  get id(): string {
    return this.#id;
  }

  /** The control-plane address that holds the registration now. */
  get gatewayAddr(): string {
    return this.#gw().addr;
  }

  /** Start-gate state of each binding from the latest register or heartbeat. */
  get bindings(): BindingStatus[] {
    return this.#bindings;
  }

  /** Lines of service with a ready instance, from the bindings that depend on it. */
  presentLines(service: string): string[] {
    const out = new Set<string>();
    for (const b of this.#bindings) {
      for (const d of b.depends) {
        if (d.service === service && d.channel === "") d.presentLines.forEach((l) => out.add(l));
      }
    }
    return [...out];
  }

  #gw(): Gateway {
    return this.#gws[this.#cur]!;
  }

  async #registerOnce(timeoutMs?: number, signal?: AbortSignal): Promise<void> {
    const gw = this.#gw();
    const resp = await gw.client.register(buildRequest(this.#opts), {
      timeoutMs,
      signal,
    });
    this.#id = resp.registrationId;
    this.#ttlMs = resp.ttlSeconds * 1000;
    this.#bindings = bindingsOf(resp.bindings);
  }

  /** Register, retrying an unreachable gateway with backoff, forever (or until signal aborts). */
  async #registerRetry(signal?: AbortSignal): Promise<void> {
    let backoff = retryMin;
    const n = this.#gws.length;
    for (let attempt = 1; ; attempt++) {
      const gw = this.#gw();
      try {
        await this.#registerOnce(n > 1 ? registerTimeout : undefined, signal);
        return;
      } catch (err) {
        if (signal?.aborted) throw err;
        if (!retryable(err)) throw err;
        if (n > 1) {
          this.#cur = (this.#cur + 1) % n;
          if (attempt % n !== 0) {
            this.#log(`registration: gateway ${gw.addr} unreachable, trying ${this.#gw().addr}: ${ConnectError.from(err).rawMessage}`);
            continue;
          }
        }
        this.#log(`registration: gateway ${this.#gws.map((g) => g.addr).join(",")} unreachable, retrying in ${backoff}ms: ${ConnectError.from(err).rawMessage}`);
        await sleep(backoff, signal);
        backoff = Math.min(backoff * 2, retryMax);
      }
    }
  }

  #waiting(): string {
    const parts: string[] = [];
    for (const b of this.#bindings) {
      if (b.state !== "waiting") continue;
      const miss: string[] = [];
      for (const d of b.depends) {
        if (d.presentLines.length > 0) continue;
        if (d.channel !== "" && d.service === "") miss.push(`channel ${d.channel} (not declared yet)`);
        else if (d.channel !== "") miss.push(`channel ${d.channel} (${d.service}/${d.lines.join("|")})`);
        else miss.push(`${d.service}(${d.lines.join("|")})`);
      }
      if (miss.length > 0) parts.push(`${b.namespace}/${b.version} needs ${miss.join(", ")}`);
      if (b.verification === "red") parts.push(`${b.namespace}/${b.version} red: ${b.verifyReasons.join("; ")}`);
      else if (b.verification === "pending") parts.push(`${b.namespace}/${b.version} verification pending`);
      else if (miss.length === 0) parts.push(`${b.namespace}/${b.version} waiting`);
    }
    return parts.sort().join("; ");
  }

  /** Poll Heartbeat until no binding is WAITING. */
  async #waitReady(signal?: AbortSignal): Promise<void> {
    const poll = this.#opts.readyPollIntervalMs ?? 1000;
    let lastLog = "";
    let lastLogAt = 0;
    let backoff = retryMin;
    for (;;) {
      const w = this.#waiting();
      if (w === "") {
        if (lastLog !== "") this.#log("registration: ready, starting");
        return;
      }
      if (w !== lastLog || Date.now() - lastLogAt >= waitLogEvery) {
        this.#log(`registration: waiting: ${w}`);
        lastLog = w;
        lastLogAt = Date.now();
      }
      await sleep(poll, signal);
      const gw = this.#gw();
      try {
        const resp = await gw.client.heartbeat({ registrationId: this.#id }, { signal });
        backoff = retryMin;
        if (!resp.ok) {
          this.#log("registration: gateway evicted us while waiting, re-registering");
          await this.#registerRetry(signal);
          continue;
        }
        this.#bindings = bindingsOf(resp.bindings);
      } catch (err) {
        if (signal?.aborted) throw err;
        this.#log(`registration: heartbeat to ${gw.addr} while waiting: ${ConnectError.from(err).rawMessage}`);
        if (this.#gws.length > 1) {
          this.#cur = (this.#cur + 1) % this.#gws.length;
          await this.#registerRetry(signal);
          continue;
        }
        await sleep(backoff, signal);
        backoff = Math.min(backoff * 2, retryMax);
      }
    }
  }

  /** @internal use register() */
  async start(): Promise<void> {
    const signal = this.#opts.signal;
    try {
      await this.#registerRetry(signal);
      await this.#waitReady(signal);
    } catch (err) {
      await this.#deregister();
      this.#closeConns();
      this.#closed = true;
      throw err;
    }
    const hi = Math.max(1, Math.round(this.#opts.heartbeatIntervalMs ?? (this.#ttlMs > 0 ? this.#ttlMs / 3 : 10000)));
    const tick = (): void => {
      this.#timer = setTimeout(() => {
        this.#beating = this.#beat(Math.min(hi, heartbeatTimeoutMax)).finally(() => {
          if (!this.#closed) tick();
        });
      }, hi);
      this.#timer.unref?.();
    };
    tick();
    for (const sig of this.#opts.deregisterOnSignal ?? []) {
      const h = (): void => {
        void this.close().finally(() => {
          for (const [s, f] of this.#signalHandlers) process.off(s, f);
          process.kill(process.pid, sig);
        });
      };
      this.#signalHandlers.push([sig, h]);
      process.on(sig, h);
    }
  }

  /** One heartbeat: not ok re-registers on the same gateway; unreachable moves to the next address. */
  async #beat(timeoutMs: number): Promise<void> {
    if (this.#id === "" || this.#closed) return;
    const gw = this.#gw();
    try {
      const resp = await gw.client.heartbeat({ registrationId: this.#id }, { timeoutMs });
      if (resp.ok) {
        this.#bindings = bindingsOf(resp.bindings);
        return;
      }
      this.#log(`registration: gateway ${gw.addr} does not know registration ${this.#id}, re-registering`);
      try {
        await this.#registerOnce(registerTimeout);
      } catch (err) {
        this.#log(`registration: re-register on ${gw.addr} failed: ${ConnectError.from(err).rawMessage}`);
        await this.#failover(gw);
      }
    } catch (err) {
      this.#log(`registration: heartbeat to ${gw.addr}: ${ConnectError.from(err).rawMessage}`);
      await this.#failover(gw);
    }
  }

  /** Register on the addresses after failed, in order, until one accepts. No-op with one address. */
  async #failover(failed: Gateway): Promise<void> {
    const n = this.#gws.length;
    if (n < 2) return;
    const start = this.#gws.indexOf(failed);
    for (let i = 1; i < n && !this.#closed; i++) {
      this.#cur = (start + i) % n;
      try {
        await this.#registerOnce(registerTimeout);
        this.#log(`registration: registered on gateway ${this.#gw().addr} instead of ${failed.addr} (registration ${this.#id})`);
        return;
      } catch (err) {
        this.#log(`registration: register on ${this.#gw().addr} failed: ${ConnectError.from(err).rawMessage}`);
      }
    }
  }

  async #deregister(): Promise<void> {
    if (this.#id === "") return;
    try {
      await this.#gw().client.deregister({ registrationId: this.#id }, { timeoutMs: 5000 });
    } catch {
      // best effort
    }
  }

  #closeConns(): void {
    for (const g of this.#gws) g.close();
  }

  /** Stop the heartbeat and deregister. Safe to call more than once. */
  close(): Promise<void> {
    this.#closing ??= (async () => {
      this.#closed = true;
      if (this.#timer) clearTimeout(this.#timer);
      for (const [s, f] of this.#signalHandlers) process.off(s, f);
      this.#signalHandlers = [];
      await this.#beating;
      await this.#deregister();
      this.#closeConns();
    })();
    return this.#closing;
  }
}

/**
 * Announce the service to the gateway and wait for its ok.
 *
 * - While the gateway is unreachable the register is retried with
 *   backoff, forever, and each failure is logged: the service does not
 *   start without the gateway. A refused register (bad request,
 *   dependency cycle, bad key) rejects.
 * - A binding whose depends (or subscribes) have no ready provider is
 *   WAITING; register() polls Heartbeat and resolves when the gateway
 *   reports every binding ready.
 * - After that a timer heartbeats at ttl/3, registers again when the
 *   gateway no longer knows the registration, and moves to the next
 *   gateway address when one is unreachable.
 */
export async function register(opts: Options): Promise<Registration> {
  const r = new Registration(opts);
  await r.start();
  return r;
}
