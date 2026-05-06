import type { CodegenConfig } from '@graphql-codegen/cli';

const config: CodegenConfig = {
  // Schema is the on-disk cache populated by `pnpm run schema`.
  schema: './schema.graphql',
  // *.graphql files alongside React components hold typed operations.
  documents: ['src/**/*.graphql', 'src/**/*.tsx'],
  generates: {
    'src/api/gateway.ts': {
      plugins: ['typescript', 'typescript-operations', 'typescript-graphql-request'],
      config: {
        avoidOptionals: true,
        useTypeImports: true,
        scalars: { JSON: 'unknown' },
      },
    },
  },
  hooks: {
    afterAllFileWrite: ['echo "wrote src/api/gateway.ts"'],
  },
};

export default config;
