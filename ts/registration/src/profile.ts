// Service profile loader: the file that says what a service provides to
// gwag. Format: gw/profile/profile.go and gw/profile/schema.json.
// Mirrors controlclient.LoadProfile (Go).
import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import type { Dependency, Owner, Service, Subscription } from "./client.ts";

interface Provide {
  kind?: string;
  file?: string;
  imports?: string[];
  url?: string;
  endpoint?: string;
  health?: string;
  snapshot?: string;
}

interface Profile {
  service?: string;
  owner?: { team?: string; contact?: string };
  public?: boolean;
  address?: string;
  health?: string;
  provides?: Record<string, Provide>;
  depends?: Record<string, string[]>;
  subscribes?: { channel: string; group?: string }[];
}

const versionRe = /^(?:v([1-9][0-9]*)|unstable)$/;

function versionN(v: string): number {
  const m = versionRe.exec(v);
  return m && m[1] ? Number(m[1]) : Number.MAX_SAFE_INTEGER;
}

function versionCmp(a: string, b: string): number {
  const na = versionN(a);
  const nb = versionN(b);
  if (na !== nb) return na < nb ? -1 : 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

function walkProto(dir: string, out: string[]): void {
  let names: string[];
  try {
    names = readdirSync(dir).sort();
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return;
    throw err;
  }
  for (const n of names) {
    const p = join(dir, n);
    if (statSync(p).isDirectory()) walkProto(p, out);
    else if (n.endsWith(".proto")) out.push(p);
  }
}

/** A GraphQL endpoint given as a path, resolved against serviceAddr ("host:port" or a URL). */
function resolveEndpoint(addr: string, ep: string): string {
  const base = /^https?:\/\//.test(addr) ? addr : `http://${addr}`;
  return new URL(ep, base).toString();
}

/**
 * Read a service profile (gwag.json; `gwag service init -o gwag.json`
 * writes one) and return one Service per provided version, sorted (v1,
 * v2, ..., unstable last): the spec read from the file the profile names
 * (proto imports from its import directories), depends, owner, public,
 * subscribes, and healthUrl / specUrl so the gateway verifies each
 * instance. serviceAddr is Options.serviceAddr; a GraphQL endpoint given
 * as a path is resolved against it. JSON only.
 *
 *     const services = loadProfile("gwag.json", "billing:50051");
 *     await register({ gateway, serviceAddr: "billing:50051", services });
 */
export function loadProfile(path: string, serviceAddr: string): Service[] {
  let p: Profile;
  try {
    p = JSON.parse(readFileSync(path, "utf8")) as Profile;
  } catch (err) {
    if (err instanceof SyntaxError) {
      throw new Error(
        `registration: profile ${path}: not JSON (${err.message}); @gwag/registration reads JSON only: write gwag.json (gwag service init -o gwag.json)`,
      );
    }
    throw err;
  }
  const fail = (m: string): never => {
    throw new Error(`registration: profile ${path}: ${m}`);
  };
  if (p === null || typeof p !== "object" || Array.isArray(p)) fail("want a JSON object");
  if (!p.service) fail("service: required");
  const service = p.service!;
  const provides = p.provides ?? {};
  const versions = Object.keys(provides).sort(versionCmp);
  if (versions.length === 0) fail("provides: at least one version required");
  const dir = dirname(path);
  const abs = (f: string): string => (isAbsolute(f) ? f : join(dir, f));

  const depends: Dependency[] = Object.keys(p.depends ?? {})
    .sort()
    .map((s) => ({ service: s, lines: [...p.depends![s]!] }));
  const subscribes: Subscription[] = (p.subscribes ?? []).map((s) => ({ channel: s.channel, group: s.group ?? "" }));
  const owner: Owner | undefined = p.owner ? { team: p.owner.team ?? "", contact: p.owner.contact ?? "" } : undefined;

  return versions.map((v) => {
    const pv = provides[v]!;
    const where = `provides.${v}`;
    const svc: Service = {
      namespace: service,
      version: v,
      depends,
      owner,
      public: p.public ?? false,
      subscribes,
      healthUrl: pv.health || p.health || "",
      specUrl: pv.url ?? "",
    };
    switch (pv.kind) {
      case "graphql": {
        if (!pv.endpoint) fail(`${where}.endpoint: required for kind graphql`);
        const ep = pv.endpoint!;
        if (ep.startsWith("/")) {
          if (!serviceAddr) fail(`${where}.endpoint ${JSON.stringify(ep)} is a path; pass the service address`);
          svc.graphqlEndpoint = resolveEndpoint(serviceAddr, ep);
        } else {
          svc.graphqlEndpoint = ep;
        }
        svc.specUrl = "";
        break;
      }
      case "openapi":
      case "proto": {
        if (!pv.file) fail(`${where}.file: required for kind ${pv.kind}`);
        const file = abs(pv.file!);
        const src = readFileSync(file);
        if (pv.kind === "openapi") {
          svc.openapiSpec = src;
          break;
        }
        svc.protoSource = src;
        const imports: Record<string, Uint8Array> = {};
        const dirs = pv.imports && pv.imports.length > 0 ? pv.imports : [dirname(pv.file!)];
        for (const d of dirs) {
          const root = abs(d);
          const found: string[] = [];
          walkProto(root, found);
          for (const f of found) {
            if (resolve(f) === resolve(file)) continue;
            const key = relative(root, f).split(sep).join("/");
            if (!(key in imports)) imports[key] = readFileSync(f);
          }
        }
        if (Object.keys(imports).length > 0) svc.protoImports = imports;
        break;
      }
      case undefined:
      case "":
        fail(`${where}.kind: required: proto, openapi or graphql`);
        break;
      default:
        fail(`${where}.kind: ${JSON.stringify(pv.kind)}: want proto, openapi or graphql`);
    }
    return svc;
  });
}
