// Sanity check for the local GraphQL grammar in src/ui/hljsLangs.ts.
// Run: node ui/scripts/hljs-graphql.ts   (Node >= 22.6 strips the TS types)
import hljs from 'highlight.js/lib/core';
import { graphql } from '../src/ui/hljsLangs.ts';

hljs.registerLanguage('graphql', graphql);
const out = (code: string) =>
  hljs
    .highlight(code, { language: 'graphql', ignoreIllegals: true })
    .value.replace(/<span class="hljs-([^"]+)">/g, '[$1:')
    .replace(/<\/span>/g, ']');

const sdl = `type Mutation {
  add(body: admin_In!): admin_Out
  del(
    id: ID!
  ): admin_LimitDeleteOut
  list(xs: [my_Type!]!): [other_Type!]!
}
type admin_X implements a_A & b_B @k {
  f: g_H
}`;
const o = out(sdl);
for (const t of [
  'admin_In', 'admin_Out', 'admin_LimitDeleteOut', 'my_Type', 'other_Type', 'a_A', 'b_B', 'g_H',
]) {
  if (!o.includes(`[type:${t}]`)) throw new Error(`${t} not highlighted as type:\n${o}`);
}
if (!o.includes('[built_in:ID]')) throw new Error('ID not built_in');
console.log('ok');
