import type { CodegenConfig } from '@graphql-codegen/cli';

const config: CodegenConfig = {
  // Schema is the on-disk cache populated by `pnpm run schema`.
  schema: './schema.graphql',
  // Inline operations are written as graphql(`...`) calls; the
  // client preset scans .ts/.tsx for them.
  documents: ['src/**/*.{ts,tsx}'],
  ignoreNoDocuments: true,
  generates: {
    'src/gql/': {
      preset: 'client',
      presetConfig: {
        // Keep fragment masking off — call sites read fields directly
        // off TypedDocumentNode results, no masking gymnastics needed.
        fragmentMasking: false,
      },
      config: {
        avoidOptionals: true,
        useTypeImports: true,
        // Long is a decimal string on output (64-bit safe); inputs
        // accept a number or a string.
        scalars: {
          JSON: 'unknown',
          Long: { input: 'string | number', output: 'string' },
        },
      },
    },
  },
  hooks: {
    afterAllFileWrite: ['echo "wrote src/gql/"'],
  },
};

export default config;
