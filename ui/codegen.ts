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
        scalars: { JSON: 'unknown' },
      },
    },
  },
  hooks: {
    afterAllFileWrite: ['echo "wrote src/gql/"'],
  },
};

export default config;
