import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer, type Http2Server } from "node:http2";
import type { AddressInfo } from "node:net";
import { after, describe, test } from "node:test";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { buildRequest, loadProfile, register } from "../src/index.ts";
import {
  ControlPlane,
  InstanceState,
  VerificationState,
  type BindingStatus,
  type RegisterRequest,
} from "../src/gen/gw/proto/controlplane/v1/control_pb.ts";

// A control plane in-process: records calls, answers as the test says.
class Fake {
  registers: RegisterRequest[] = [];
  heartbeats: string[] = [];
  deregisters: string[] = [];
  auth: (string | null)[] = [];
  known = new Set<string>();
  seq = 0;
  ttl = 1;
  /** state returned for every binding */
  state: InstanceState = InstanceState.READY;
  verification: VerificationState = VerificationState.UNSPECIFIED;
  verifyReasons: string[] = [];
  failRegister: ConnectError | undefined;
  server: Http2Server;

  constructor() {
    this.server = createServer(
      connectNodeAdapter({
        routes: (router: ConnectRouter) => {
          router.service(ControlPlane, {
            register: (req, ctx) => {
              this.auth.push(ctx.requestHeader.get("authorization"));
              if (this.failRegister) throw this.failRegister;
              this.registers.push(req);
              const id = `r${++this.seq}`;
              this.known.add(id);
              return { registrationId: id, ttlSeconds: this.ttl, bindings: this.status(req) };
            },
            heartbeat: (req) => {
              this.heartbeats.push(req.registrationId);
              const ok = this.known.has(req.registrationId);
              const last = this.registers.at(-1);
              return { ok, bindings: ok && last ? this.status(last) : [] };
            },
            deregister: (req) => {
              this.deregisters.push(req.registrationId);
              this.known.delete(req.registrationId);
              return {};
            },
          });
        },
      }),
    );
  }

  status(req: RegisterRequest): BindingStatus[] {
    return req.services.map(
      (s) =>
        ({
          $typeName: "gateway.controlplane.v1.BindingStatus",
          namespace: s.namespace,
          version: s.version,
          state: this.state,
          verification: this.verification,
          verifyReasons: this.verifyReasons,
          depends:
            this.state === InstanceState.WAITING
              ? [
                  {
                    $typeName: "gateway.controlplane.v1.DependencyStatus",
                    service: "",
                    lines: ["v1"],
                    presentLines: [],
                    channel: "orders.v1.OrderEvents",
                    group: "billing",
                  },
                ]
              : [],
        }) as BindingStatus,
    );
  }

  async listen(): Promise<string> {
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    return `127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }

  async stop(): Promise<void> {
    (this.server as unknown as { closeAllConnections?: () => void }).closeAllConnections?.();
    await new Promise<void>((r) => this.server.close(() => r()));
  }
}

const quiet = () => {};
const svc = {
  namespace: "billing",
  version: "v1",
  protoSource: 'syntax = "proto3"; package billing.v1;',
  subscribes: [{ channel: "orders.v1.OrderEvents", group: "billing" }],
};
const waitFor = async (cond: () => boolean, ms = 5000) => {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timeout");
    await new Promise((r) => setTimeout(r, 20));
  }
};

describe("buildRequest", () => {
  test("carries the proto source, imports and subscribes", () => {
    const req = buildRequest({
      serviceAddr: "svc:1",
      instanceId: "i",
      ttlSeconds: 9,
      services: [{ ...svc, protoImports: { "a.proto": "x" }, owner: { team: "t" }, depends: [{ service: "library", lines: ["v1"] }] }],
    });
    assert.equal(req.addr, "svc:1");
    assert.equal(req.ttlSeconds, 9);
    const b = req.services[0]!;
    assert.equal(new TextDecoder().decode(b.protoSource), svc.protoSource);
    assert.equal(new TextDecoder().decode(b.protoImports["a.proto"]), "x");
    assert.deepEqual(b.subscribes.map((s) => [s.channel, s.group]), [["orders.v1.OrderEvents", "billing"]]);
    assert.equal(b.owner?.team, "t");
    assert.deepEqual(b.depends[0]?.lines, ["v1"]);
  });

  test("rejects zero or several sources", () => {
    assert.throws(() => buildRequest({ serviceAddr: "s:1", services: [{ namespace: "x" }] }), /exactly one/);
    assert.throws(
      () => buildRequest({ serviceAddr: "s:1", services: [{ namespace: "x", protoSource: "a", openapiSpec: "b" }] }),
      /exactly one/,
    );
    assert.throws(() => buildRequest({ serviceAddr: "s:1", services: [] }), /at least one/);
  });
});

describe("loadProfile", () => {
  const tmp = mkdtempSync(join(tmpdir(), "gwag-profile-"));
  const write = (rel: string, body: string) => {
    const p = join(tmp, rel);
    mkdirSync(join(p, ".."), { recursive: true });
    writeFileSync(p, body);
    return p;
  };
  const dec = (b: unknown) => new TextDecoder().decode(b as Uint8Array);

  test("openapi profile", () => {
    write("api/billing-v1.json", '{"openapi":"3.0.0"}');
    write("api/billing-v2.json", '{"openapi":"3.1.0"}');
    const path = write(
      "oa/gwag.json",
      JSON.stringify({
        service: "billing",
        owner: { team: "payments", contact: "#payments" },
        public: true,
        health: "/healthz",
        provides: {
          unstable: { kind: "openapi", file: "../api/billing-v2.json" },
          v2: { kind: "openapi", file: "../api/billing-v2.json", url: "/spec?v=2", health: "/h2" },
          v1: { kind: "openapi", file: "../api/billing-v1.json", url: "/spec?v=1" },
        },
        depends: { library: ["v1", "v2"], accounts: ["v1"] },
        subscribes: [{ channel: "orders.v1.OrderEvents", group: "billing" }],
      }),
    );
    const out = loadProfile(path, "billing:8080");
    assert.deepEqual(out.map((s) => s.version), ["v1", "v2", "unstable"]);
    const v1 = out[0]!;
    assert.equal(v1.namespace, "billing");
    assert.equal(dec(v1.openapiSpec), '{"openapi":"3.0.0"}');
    assert.equal(v1.healthUrl, "/healthz");
    assert.equal(v1.specUrl, "/spec?v=1");
    assert.equal(out[1]!.healthUrl, "/h2");
    assert.equal(out[2]!.specUrl, "");
    assert.deepEqual(v1.depends, [
      { service: "accounts", lines: ["v1"] },
      { service: "library", lines: ["v1", "v2"] },
    ]);
    assert.deepEqual(v1.owner, { team: "payments", contact: "#payments" });
    assert.equal(v1.public, true);
    assert.deepEqual(v1.subscribes, [{ channel: "orders.v1.OrderEvents", group: "billing" }]);
    assert.doesNotThrow(() => buildRequest({ serviceAddr: "billing:8080", services: out }));
  });

  test("proto profile reads imports from the entry's directory", () => {
    write("pr/proto/a.proto", 'syntax = "proto3"; import "common/c.proto";');
    write("pr/proto/common/c.proto", 'syntax = "proto3";');
    const path = write("pr/gwag.json", JSON.stringify({ service: "a", provides: { v1: { kind: "proto", file: "proto/a.proto" } } }));
    const [s] = loadProfile(path, "a:1");
    assert.equal(dec(s!.protoSource), 'syntax = "proto3"; import "common/c.proto";');
    assert.deepEqual(Object.keys(s!.protoImports ?? {}), ["common/c.proto"]);
    assert.equal(dec(s!.protoImports!["common/c.proto"]), 'syntax = "proto3";');
  });

  test("graphql endpoint path resolves against the service address", () => {
    const path = write("gq/gwag.json", JSON.stringify({ service: "g", provides: { v1: { kind: "graphql", endpoint: "/graphql" } } }));
    assert.equal(loadProfile(path, "g:9000")[0]!.graphqlEndpoint, "http://g:9000/graphql");
  });

  test("rejects an invalid profile with a clear message", () => {
    assert.throws(() => loadProfile(write("bad1.json", "{}"), "x:1"), /service: required/);
    assert.throws(() => loadProfile(write("bad2.json", '{"service":"x"}'), "x:1"), /at least one version/);
    assert.throws(
      () => loadProfile(write("bad3.json", '{"service":"x","provides":{"v1":{"kind":"soap"}}}'), "x:1"),
      /want proto, openapi or graphql/,
    );
    assert.throws(() => loadProfile(write("bad4.json", '{"service":"x","provides":{"v1":{"kind":"proto"}}}'), "x:1"), /file: required/);
    assert.throws(() => loadProfile(write("bad5.yaml", "service: x\n"), "x:1"), /gwag service init -o gwag.json/);
  });
});

describe("register", () => {
  const fakes: Fake[] = [];
  after(async () => {
    for (const f of fakes) await f.stop();
  });
  const start = async () => {
    const f = new Fake();
    fakes.push(f);
    return { f, addr: await f.listen() };
  };

  test("announces, sends the application key, heartbeats, deregisters on close", async () => {
    const { f, addr } = await start();
    const reg = await register({
      gateway: addr,
      serviceAddr: "billing:50051",
      services: [svc],
      applicationKey: "app-key-1",
      heartbeatIntervalMs: 50,
      logger: quiet,
    });
    assert.equal(reg.id, "r1");
    assert.equal(f.registers.length, 1);
    assert.equal(f.registers[0]!.services[0]!.subscribes[0]!.channel, "orders.v1.OrderEvents");
    assert.deepEqual(f.auth, ["Bearer app-key-1"]);
    await waitFor(() => f.heartbeats.length >= 2);
    await reg.close();
    assert.deepEqual(f.deregisters, ["r1"]);
    await reg.close(); // second close is a no-op
    assert.equal(f.deregisters.length, 1);
  });

  test("sends no authorization header without a key", async () => {
    const { f, addr } = await start();
    const reg = await register({ gateway: addr, serviceAddr: "b:1", services: [svc], logger: quiet });
    assert.deepEqual(f.auth, [null]);
    await reg.close();
  });

  test("heartbeat period defaults to ttl/3", async () => {
    const { f, addr } = await start();
    f.ttl = 1; // 333ms period
    const reg = await register({ gateway: addr, serviceAddr: "b:1", services: [svc], logger: quiet });
    await waitFor(() => f.heartbeats.length >= 2, 3000);
    await reg.close();
  });

  test("registers again when the gateway forgot the registration", async () => {
    const { f, addr } = await start();
    const reg = await register({ gateway: addr, serviceAddr: "b:1", services: [svc], heartbeatIntervalMs: 50, logger: quiet });
    f.known.clear(); // gateway restarted
    await waitFor(() => reg.id === "r2"); // the fake counts the call before the client reads the reply
    assert.equal(reg.id, "r2");
    await reg.close();
    assert.deepEqual(f.deregisters, ["r2"]);
  });

  test("waits for ok while a binding is WAITING", async () => {
    const { f, addr } = await start();
    f.state = InstanceState.WAITING;
    const logs: string[] = [];
    let resolved = false;
    const p = register({
      gateway: addr,
      serviceAddr: "b:1",
      services: [svc],
      readyPollIntervalMs: 30,
      heartbeatIntervalMs: 1000,
      logger: (m) => logs.push(m),
    }).then((r) => {
      resolved = true;
      return r;
    });
    await waitFor(() => f.heartbeats.length >= 2);
    assert.equal(resolved, false);
    assert.ok(logs.some((l) => l.includes("channel orders.v1.OrderEvents")), logs.join("\n"));
    f.state = InstanceState.READY;
    const reg = await p;
    assert.equal(reg.bindings[0]!.state, "ready");
    await reg.close();
  });

  test("verify fields reach the gateway only when set", async () => {
    const { f, addr } = await start();
    const reg = await register({
      gateway: addr,
      serviceAddr: "b:1",
      services: [{ ...svc, healthUrl: "/healthz", specUrl: "/spec" }, { ...svc, version: "v2" }],
      logger: quiet,
    });
    const [a, b] = f.registers[0]!.services;
    assert.equal(a!.verify?.healthUrl, "/healthz");
    assert.equal(a!.verify?.specUrl, "/spec");
    assert.equal(b!.verify, undefined);
    await reg.close();
  });

  test("waits while a binding is RED, then starts when GREEN", async () => {
    const { f, addr } = await start();
    f.state = InstanceState.WAITING;
    f.verification = VerificationState.RED;
    f.verifyReasons = ["health /healthz: status 500", "spec differs"];
    const logs: string[] = [];
    let resolved = false;
    const p = register({
      gateway: addr,
      serviceAddr: "b:1",
      services: [{ ...svc, subscribes: [], healthUrl: "/healthz" }],
      readyPollIntervalMs: 30,
      heartbeatIntervalMs: 1000,
      logger: (m) => logs.push(m),
    }).then((r) => {
      resolved = true;
      return r;
    });
    await waitFor(() => f.heartbeats.length >= 2);
    assert.equal(resolved, false);
    assert.ok(logs.some((l) => l.includes("billing/v1 red: health /healthz: status 500; spec differs")), logs.join("\n"));
    f.state = InstanceState.READY;
    f.verification = VerificationState.GREEN;
    f.verifyReasons = [];
    const reg = await p;
    assert.equal(reg.bindings[0]!.verification, "green");
    assert.ok(logs.includes("registration: ready, starting"), logs.join("\n"));
    await reg.close();
  });

  test("a refused register rejects", async () => {
    const { f, addr } = await start();
    f.failRegister = new ConnectError("bad key", Code.Unauthenticated);
    await assert.rejects(register({ gateway: addr, serviceAddr: "b:1", services: [svc], logger: quiet }), /bad key/);
  });

  test("retries an unreachable gateway until it is up", async () => {
    const f = new Fake();
    fakes.push(f);
    // Reserve a port, release it, register against it, then listen.
    await new Promise<void>((r) => f.server.listen(0, "127.0.0.1", r));
    const port = (f.server.address() as AddressInfo).port;
    await new Promise<void>((r) => f.server.close(() => r()));
    const logs: string[] = [];
    const p = register({ gateway: `127.0.0.1:${port}`, serviceAddr: "b:1", services: [svc], logger: (m) => logs.push(m) });
    await waitFor(() => logs.length >= 1);
    f.server.listen(port, "127.0.0.1");
    const reg = await p;
    assert.ok(logs[0]!.includes("unreachable"));
    await reg.close();
  });

  test("the abort signal ends the wait", async () => {
    const ac = new AbortController();
    const p = register({ gateway: "127.0.0.1:1", serviceAddr: "b:1", services: [svc], signal: ac.signal, logger: quiet });
    setTimeout(() => ac.abort(new Error("stop")), 100);
    await assert.rejects(p, /stop/);
  });

  test("moves to the next gateway when the first goes away", async () => {
    const a = await start();
    const b = await start();
    const reg = await register({
      gateway: [a.addr, b.addr],
      serviceAddr: "b:1",
      services: [svc],
      heartbeatIntervalMs: 50,
      logger: quiet,
    });
    assert.equal(reg.gatewayAddr, a.addr);
    await a.f.stop();
    await waitFor(() => b.f.registers.length >= 1);
    assert.equal(reg.gatewayAddr, b.addr);
    await reg.close();
  });
});
