import type { CodegenConfig } from '@graphql-codegen/cli'

// Reads the schema straight from the running gat server. Run
// `pnpm dev` (or boot the server some other way) first, then
// `pnpm gen`. Output lands in src/gql/ and is gitignored.
const config: CodegenConfig = {
  overwrite: true,
  schema: 'http://localhost:8080/api/schema/graphql',
  documents: ['src/**/*.{ts,tsx}'],
  generates: {
    './src/gql/': {
      preset: 'client',
      config: {
        useTypeImports: true,
      },
    },
  },
  ignoreNoDocuments: true,
}

export default config
