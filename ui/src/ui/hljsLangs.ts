// Small highlight.js grammars for GraphQL SDL and proto. The bundled
// grammars only colour a few tokens; these classify definitions, type
// references, field names, directives and docstrings separately.
import type { HLJSApi, Language, Mode } from 'highlight.js';

const IDENT = /[_A-Za-z]\w*/;
const punct: Mode = { scope: 'punctuation', match: /[{}()[\]!:=|&,<>;]/ };

export function graphql(hljs: HLJSApi): Language {
  const comment: Mode = hljs.HASH_COMMENT_MODE;
  const docstring: Mode = { scope: 'comment', begin: '"""', end: '"""' };
  const string: Mode = {
    scope: 'string',
    begin: '"',
    end: '"',
    contains: [hljs.BACKSLASH_ESCAPE],
  };
  const number: Mode = { scope: 'number', match: /-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/ };
  const directive: Mode = { scope: 'meta', match: /@[_A-Za-z]\w*/ };
  const typeRef: Mode = {
    scope: 'type',
    match: /[_A-Za-z]\w*/,
    keywords: { built_in: ['String', 'Int', 'Float', 'Boolean', 'ID'] },
  };
  // Built-in scalars get their own scope; everything else in a type position is a type.
  const scalar: Mode = { scope: 'built_in', match: /\b(?:String|Int|Float|Boolean|ID)\b/ };
  const typeTokens: Mode[] = [scalar, typeRef, punct];
  const literal: Mode = { scope: 'literal', match: /\b(?:true|false|null)\b/ };
  return {
    name: 'GraphQL SDL',
    aliases: ['gql'],
    keywords: {
      keyword: [
        'type', 'input', 'enum', 'interface', 'union', 'scalar', 'schema', 'extend',
        'directive', 'implements', 'repeatable', 'on', 'fragment', 'query', 'mutation',
        'subscription',
      ],
      literal: ['true', 'false', 'null'],
    },
    contains: [
      docstring,
      // Single-line description on its own line: treat like a docstring.
      { scope: 'comment', match: /^[ \t]*"[^"\n]*"[ \t]*$/m },
      string,
      comment,
      directive,
      // `union X = A | B`, possibly continued on following lines.
      {
        begin: [/\bunion/, /\s+/, IDENT, /\s*=/],
        beginScope: { 1: 'keyword', 3: 'title class_', 4: 'punctuation' },
        end: /\n\s*\n|(?=\n\s*(?:type|input|enum|interface|union|scalar|extend|directive|schema|"|#|@|\}))/,
        contains: [comment, directive, ...typeTokens],
      },
      // `type Foo implements A & B {`
      {
        begin: [/\b(?:type|interface)/, /\s+/, IDENT, /\s+implements\b/],
        beginScope: { 1: 'keyword', 3: 'title class_', 4: 'keyword' },
        end: /(?=[{@\n])/,
        contains: [...typeTokens],
      },
      {
        match: [/\b(?:type|input|enum|interface|scalar|directive|fragment|query|mutation|subscription)/, /\s+/, /@?[_A-Za-z]\w*/],
        scope: { 1: 'keyword', 3: 'title class_' },
      },
      // `fragment F on User`, `directive @d on FIELD | OBJECT`
      { match: [/\bon/, /\s+/, /[A-Z_]\w*/], scope: { 1: 'keyword', 3: 'type' } },
      // Type reference after a colon, e.g. `: [ID!]!` or `: String = "x"`.
      {
        begin: /:\s*(?=[[A-Za-z_])/,
        beginScope: 'punctuation',
        end: /(?=[\s,)=@#}])/,
        contains: [scalar, literal, typeRef, { scope: 'punctuation', match: /[[\]!]/ }],
      },
      number,
      literal,
      // Field and argument names.
      { scope: 'attr', match: /[_A-Za-z]\w*(?=\s*[:(])/ },
      punct,
    ],
  };
}

export function protobuf(hljs: HLJSApi): Language {
  const scalars =
    /\b(?:double|float|int32|int64|uint32|uint64|sint32|sint64|fixed32|fixed64|sfixed32|sfixed64|bool|string|bytes)\b/;
  const rpcTypes: Mode = {
    begin: /\(/,
    end: /\)/,
    beginScope: 'punctuation',
    endScope: 'punctuation',
    contains: [{ scope: 'keyword', match: /\bstream\b/ }, { scope: 'type', match: /[.\w]+/ }],
  };
  return {
    name: 'Protocol Buffers',
    aliases: ['proto'],
    keywords: {
      keyword: [
        'syntax', 'package', 'import', 'option', 'optional', 'required', 'repeated', 'oneof',
        'map', 'reserved', 'extensions', 'extend', 'returns', 'stream', 'public', 'weak', 'to', 'max',
      ],
      literal: ['true', 'false'],
    },
    contains: [
      hljs.QUOTE_STRING_MODE,
      hljs.C_LINE_COMMENT_MODE,
      hljs.C_BLOCK_COMMENT_MODE,
      { scope: 'number', match: /-?\b\d+(?:\.\d+)?\b/ },
      { match: [/\b(?:message|enum|service)/, /\s+/, IDENT], scope: { 1: 'keyword', 3: 'title class_' } },
      {
        begin: [/\brpc/, /\s+/, IDENT],
        beginScope: { 1: 'keyword', 3: 'title function_' },
        end: /(?=[{;])/,
        keywords: { keyword: ['returns', 'stream'] },
        contains: [rpcTypes],
      },
      { match: [/\boneof/, /\s+/, IDENT], scope: { 1: 'keyword', 3: 'title' } },
      {
        begin: /\bmap\s*</,
        beginScope: 'keyword',
        end: />/,
        endScope: 'punctuation',
        contains: [{ scope: 'type', match: /\.?[A-Za-z_][\w.]*/ }, { scope: 'punctuation', match: /,/ }],
      },
      // Extension / custom options: `(google.api.http)`.
      { scope: 'meta', match: /\([.\w]+\)/ },
      // Field type before `name = N`.
      { scope: 'type', match: /\.?[A-Za-z_][\w.]*(?=\s+[A-Za-z_]\w*\s*=\s*\d)/ },
      // Field name / enum value before `= N`.
      { scope: 'attr', match: /[A-Za-z_]\w*(?=\s*=\s*-?\d)/ },
      { scope: 'type', match: scalars },
      punct,
    ],
  };
}
