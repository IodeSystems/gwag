import { createFileRoute } from '@tanstack/react-router';
import { Box, Chip, Tooltip, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { client } from '@/api/client';
import { InjectorsQuery } from '@/api/operations';
import { DataTable, EmptyState, Page, Panel, StatusChip, type Column } from '@/ui';
import type { ResultOf } from '@graphql-typed-document-node/core';

export const Route = createFileRoute('/injectors')({
  component: Injectors,
});

// Strip null from the array before indexing it; see the note in deprecated.tsx.
type Injector = NonNullable<
  NonNullable<
    NonNullable<
      NonNullable<ResultOf<typeof InjectorsQuery>['admin']>['listInjectors']
    >['injectors']
  >[number]
>;

type Landing = NonNullable<NonNullable<Injector['landings']>[number]>;

function landingLabel(l: Landing): string {
  switch (l.kind) {
    case 'arg':
      return `${l.namespace}${l.version ? ':' + l.version : ''}.${l.op}.${l.argName}`;
    case 'field':
      return `${l.namespace}${l.version ? ':' + l.version : ''} · ${l.typeName}.${l.fieldName}`;
    case 'header':
      return l.headerName ?? '';
    default:
      return JSON.stringify(l);
  }
}

function injectorKey(inj: Injector): string {
  switch (inj.kind) {
    case 'type':
      return inj.typeName ?? '?';
    case 'path':
      return inj.path ?? '?';
    case 'header':
      return inj.headerName ?? '?';
    default:
      return '?';
  }
}

function modeLabel(inj: Injector): string {
  const parts: string[] = [];
  parts.push(inj.hide ? 'hide' : 'inspect');
  if (inj.nullable) parts.push('nullable');
  return parts.join(' + ');
}

function Injectors() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['injectors'],
    queryFn: () => client.request(InjectorsQuery),
    refetchInterval: 10_000,
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{error.message}</Typography>;

  const injectors = (data?.admin?.listInjectors?.injectors ?? []).filter(
    (x): x is Injector => x !== null,
  );

  if (injectors.length === 0) {
    return (
      <Page title="Injectors">
        <Panel>
          <EmptyState
            title="No injectors"
            description={
              <>
                No <code>InjectType</code>, <code>InjectPath</code>, or <code>InjectHeader</code>{' '}
                registrations on this gateway.
              </>
            }
          />
        </Panel>
      </Page>
    );
  }

  return (
    <Page
      title="Injectors"
      subtitle={
        <>
          Every InjectType / InjectPath / InjectHeader registration on this gateway, with the args /
          fields / headers it currently lands on. Path-keyed entries with no resolved landing are{' '}
          <em>dormant</em> — they activate on a future schema rebuild that brings the path into
          existence.
        </>
      }
    >
      <Panel flush>
        <DataTable columns={columns} rows={injectors} getRowId={(inj) => String(injectors.indexOf(inj))} />
      </Panel>
    </Page>
  );
}

const columns: Column<Injector>[] = [
  { id: 'kind', header: 'Kind', cell: (inj) => <Chip label={inj.kind} size="small" /> },
  { id: 'key', header: 'Key', mono: true, cell: (inj) => injectorKey(inj) },
  { id: 'mode', header: 'Mode', cell: (inj) => modeLabel(inj) },
  {
    id: 'state',
    header: 'State',
    cell: (inj) => (
      <StatusChip
        status={inj.state === 'active' ? 'ready' : 'waiting'}
        label={inj.state}
        size="small"
      />
    ),
  },
  {
    id: 'landings',
    header: 'Landings',
    cell: (inj) => {
      // `landings` is nullable in the schema; a null array renders the
      // same "—" placeholder as an empty one.
      const landings = (inj.landings ?? []).filter((l): l is Landing => l !== null);
      if (landings.length === 0) {
        return (
          <Typography variant="body2" color="text.secondary">
            —
          </Typography>
        );
      }
      return (
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
          {landings.map((l, j) => (
            <Chip key={j} label={landingLabel(l)} size="small" variant="outlined" />
          ))}
        </Box>
      );
    },
  },
  {
    id: 'registered',
    header: 'Registered',
    cell: (inj) => {
      const reg = inj.registeredAt;
      const regLabel =
        reg.file && reg.line !== null && reg.line !== undefined
          ? `${trimRepoPath(reg.file)}:${reg.line}`
          : '—';
      return (
        <Tooltip title={reg.function ? reg.function : ''}>
          <Box component="span" sx={{ fontFamily: (t) => t.gwag.mono, fontSize: '0.75rem' }}>
            {regLabel}
          </Box>
        </Tooltip>
      );
    },
  },
];

// trimRepoPath shortens an absolute file path to its last 3 segments
// so the table cell stays readable; full path lives in the tooltip.
function trimRepoPath(p: string): string {
  const parts = p.split('/');
  if (parts.length <= 3) return p;
  return '…/' + parts.slice(-3).join('/');
}
