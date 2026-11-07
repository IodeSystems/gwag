// Typed REST client for services behind a gwag gateway.
// Types come from the gateway's exported OpenAPI (see gen.sh).
//
//   pnpm gen && pnpm start
//
// Env: GWAG_INGRESS (default http://localhost:8080/api/ingress).
import createClient from "openapi-fetch";
import type { paths as GreeterPaths } from "./gen/greeter.ts";
import type { paths as LibraryPaths } from "./gen/library.ts";

const baseUrl = process.env.GWAG_INGRESS ?? "http://localhost:8080/api/ingress";

const greeter = createClient<GreeterPaths>({ baseUrl });
const library = createClient<LibraryPaths>({ baseUrl });

const hello = await greeter.POST("/greeter.v1.GreeterService/Hello", {
  body: { name: "openapi" },
});
if (!hello.data) throw new Error(`Hello: HTTP ${hello.response.status}`);
// hello.data is { greeting?: string }
console.log(`greeter.Hello -> ${hello.data.greeting}`);

const list = await library.POST("/library.v1.LibraryService/ListBooks", {
  body: {},
});
if (!list.data) throw new Error(`ListBooks: HTTP ${list.response.status}`);
const books = list.data.books ?? [];
console.log(`library.ListBooks -> ${books.length} books`);
for (const b of books) console.log(`  ${b.id}  ${b.title} (${b.year})`);

const one = await library.POST("/library.v1.LibraryService/GetBook", {
  body: { id: "1" },
});
if (!one.data) throw new Error(`GetBook: HTTP ${one.response.status}`);
console.log(`library.GetBook(1) -> ${one.data.title} by ${one.data.author}`);
