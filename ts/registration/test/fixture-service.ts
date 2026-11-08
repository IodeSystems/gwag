// A service that registers itself and waits: the program
// e2e/registration/check.sh runs against a real gateway.
//
//   node test/fixture-service.ts --gateway HOST:PORT --namespace NS --proto FILE \
//        [--import PATH=FILE]... [--subscribe CHANNEL=GROUP]... [--key KEY] [--ttl SECONDS]
//
// Prints "REGISTERED <id>" once the gateway says ok, then runs until
// SIGTERM or SIGINT, which deregister it.
import { readFileSync } from "node:fs";
import { parseArgs } from "node:util";
import { register } from "../src/index.ts";

const { values } = parseArgs({
  options: {
    gateway: { type: "string" },
    namespace: { type: "string" },
    proto: { type: "string" },
    import: { type: "string", multiple: true },
    subscribe: { type: "string", multiple: true },
    key: { type: "string" },
    ttl: { type: "string", default: "6" },
  },
});

const imports: Record<string, string> = {};
for (const kv of values.import ?? []) {
  const [path, file] = kv.split("=", 2) as [string, string];
  imports[path] = readFileSync(file, "utf8");
}
const subscribes = (values.subscribe ?? []).map((kv) => {
  const [channel, group] = kv.split("=", 2) as [string, string];
  return { channel, group };
});

const reg = await register({
  gateway: values.gateway!,
  serviceAddr: "127.0.0.1:1", // nothing calls it
  instanceId: `${values.namespace}-ts`,
  ttlSeconds: Number(values.ttl),
  applicationKey: values.key,
  readyPollIntervalMs: 200,
  deregisterOnSignal: ["SIGTERM", "SIGINT"],
  services: [
    {
      namespace: values.namespace!,
      version: "v1",
      protoSource: readFileSync(values.proto!, "utf8"),
      protoImports: imports,
      subscribes,
    },
  ],
});
console.log(`REGISTERED ${reg.id}`);
setInterval(() => {}, 1 << 30); // run until a signal
