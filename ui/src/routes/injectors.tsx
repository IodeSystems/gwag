import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  Chip,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { client } from '@/api/client';
import { InjectorsQuery } from '@/api/operations';
import type { ResultOf } from '@graphql-typed-document-node/core';

export const Route = createFileRoute('/injectors')({
  component: Injectors,
});

type Injector = NonNullable<
  NonNullable<
    NonNullable<ResultOf<typeof InjectorsQuery>['admin']>['listInjectors']
  >['injectors'][number]
>;

type Landing = NonNullable<Injector['landings'][number]>;

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
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  const injectors = (data?.admin?.listInjectors?.injectors ?? []).filter(
    (x): x is Injector => x !== null,
  );

  if (injectors.length === 0) {
    return (
      <Stack spacing={2}>
        <Typography variant="h5">Injectors</Typography>
        <Paper sx={{ p: 3 }}>
          <Typography color="text.secondary">
            No <code>InjectType</code>, <code>InjectPath</code>, or{' '}
            <code>InjectHeader</code> registrations on this gateway.
          </Typography>
        </Paper>
      </Stack>
    );
  }

  return (
    <Stack spacing={2}>
      <Typography variant="h5">Injectors</Typography>
      <Typography variant="body2" color="text.secondary">
        Every InjectType / InjectPath / InjectHeader registration on this
        gateway, with the args / fields / headers it currently lands on.
        Path-keyed entries with no resolved landing are <em>dormant</em> —
        they activate on a future schema rebuild that brings the path
        into existence.
      </Typography>
      <TableContainer component={Paper}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Kind</TableCell>
              <TableCell>Key</TableCell>
              <TableCell>Mode</TableCell>
              <TableCell>State</TableCell>
              <TableCell>Landings</TableCell>
              <TableCell>Registered</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {injectors.map((inj, i) => {
              const reg = inj.registeredAt;
              const regTitle = reg.function ? reg.function : '';
              const regLabel =
                reg.file && reg.line !== null && reg.line !== undefined
                  ? `${trimRepoPath(reg.file)}:${reg.line}`
                  : '—';
              return (
                <TableRow key={i}>
                  <TableCell>
                    <Chip label={inj.kind} size="small" />
                  </TableCell>
                  <TableCell>
                    <code style={{ fontSize: '0.85rem' }}>{injectorKey(inj)}</code>
                  </TableCell>
                  <TableCell>{modeLabel(inj)}</TableCell>
                  <TableCell>
                    <Chip
                      label={inj.state}
                      size="small"
                      color={inj.state === 'active' ? 'success' : 'warning'}
                    />
                  </TableCell>
                  <TableCell>
                    {inj.landings.length === 0 ? (
                      <Typography variant="body2" color="text.secondary">
                        —
                      </Typography>
                    ) : (
                      <Box
                        sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}
                      >
                        {inj.landings
                          .filter((l): l is Landing => l !== null)
                          .map((l, j) => (
                            <Chip
                              key={j}
                              label={landingLabel(l)}
                              size="small"
                              variant="outlined"
                            />
                          ))}
                      </Box>
                    )}
                  </TableCell>
                  <TableCell>
                    <Tooltip title={regTitle}>
                      <code style={{ fontSize: '0.75rem' }}>{regLabel}</code>
                    </Tooltip>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>
    </Stack>
  );
}

// trimRepoPath shortens an absolute file path to its last 3 segments
// so the table cell stays readable; full path lives in the tooltip.
function trimRepoPath(p: string): string {
  const parts = p.split('/');
  if (parts.length <= 3) return p;
  return '…/' + parts.slice(-3).join('/');
}
