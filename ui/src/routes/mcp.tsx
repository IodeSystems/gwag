import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  Button,
  FormControlLabel,
  IconButton,
  Stack,
  Switch,
  TextField,
  Tooltip,
  Typography,
} from '@mui/material';
import DeleteIcon from '@mui/icons-material/Delete';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { client } from '@/api/client';
import {
  McpConfigQuery,
  McpIncludeMutation,
  McpExcludeMutation,
  McpIncludeRemoveMutation,
  McpExcludeRemoveMutation,
  McpSetAutoIncludeMutation,
} from '@/api/operations';
import { DataTable, Page, Panel, StatusChip, useToast, type Column } from '@/ui';

export const Route = createFileRoute('/mcp')({
  component: McpConfig,
});

// MCP config page: operator curates which operations LLM agents see
// through /mcp. Default-deny — the allowlist is exactly Include
// unless auto_include flips it to "all public ops minus Exclude".
// Internal `_*` namespaces are filtered regardless. Live preview
// pulls from admin.mcpSchemaList so the operator sees the surface
// they're sculpting in real time.
interface Entry {
  path: string;
  kind: string;
  namespace: string;
  version: string;
  description?: string | null;
}

const columns: Column<Entry>[] = [
  { id: 'path', header: 'Path', cell: (e) => <code>{e.path}</code> },
  { id: 'kind', header: 'Kind', cell: (e) => e.kind },
  { id: 'namespace', header: 'Namespace', cell: (e) => e.namespace },
  { id: 'version', header: 'Version', cell: (e) => e.version },
  {
    id: 'description',
    header: 'Description',
    width: 360,
    cell: (e) => (
      <Typography variant="body2" color="text.secondary" noWrap title={e.description ?? ''} sx={{ maxWidth: 360 }}>
        {e.description}
      </Typography>
    ),
  },
];

function McpConfig() {
  const qc = useQueryClient();
  const toast = useToast();
  const [includeDraft, setIncludeDraft] = useState('');
  const [excludeDraft, setExcludeDraft] = useState('');

  const { data, isLoading, error } = useQuery({
    queryKey: ['mcpConfig'],
    queryFn: () => client.request(McpConfigQuery),
    refetchInterval: 10_000,
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ['mcpConfig'] });

  const includeAdd = useMutation({
    mutationFn: (path: string) => client.request(McpIncludeMutation, { path }),
    onSuccess: () => {
      void invalidate();
      setIncludeDraft('');
      toast.success('include added');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const includeRemove = useMutation({
    mutationFn: (path: string) =>
      client.request(McpIncludeRemoveMutation, { path }),
    onSuccess: () => {
      void invalidate();
      toast.success('include removed');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const excludeAdd = useMutation({
    mutationFn: (path: string) => client.request(McpExcludeMutation, { path }),
    onSuccess: () => {
      void invalidate();
      setExcludeDraft('');
      toast.success('exclude added');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const excludeRemove = useMutation({
    mutationFn: (path: string) =>
      client.request(McpExcludeRemoveMutation, { path }),
    onSuccess: () => {
      void invalidate();
      toast.success('exclude removed');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const toggleAuto = useMutation({
    mutationFn: (autoInclude: boolean) =>
      client.request(McpSetAutoIncludeMutation, { autoInclude }),
    onSuccess: (_resp, v) => {
      void invalidate();
      toast.success(v ? 'auto-include on' : 'auto-include off');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error)
    return <Typography color="error">{error.message}</Typography>;

  const cfg = data?.admin?.mcpList;
  const entries = data?.admin?.mcpSchemaList?.entries ?? [];
  const autoInclude = cfg?.autoInclude ?? false;
  const include = (cfg?.include ?? []).filter((s): s is string => s !== null);
  const exclude = (cfg?.exclude ?? []).filter((s): s is string => s !== null);

  const rows: Entry[] = [];
  for (const e of entries) if (e) rows.push(e);

  const autoPanel = (
    <Panel>
      <FormControlLabel
        control={
          <Switch
            checked={autoInclude}
            onChange={(e) => toggleAuto.mutate(e.target.checked)}
            disabled={toggleAuto.isPending}
          />
        }
        label={
          <Stack>
            <Typography>Auto-include</Typography>
            <Typography variant="caption" color="text.secondary">
              {autoInclude
                ? 'Surface = every public operation minus the exclude list.'
                : 'Surface = exactly the include list (default-deny).'}
            </Typography>
          </Stack>
        }
      />
    </Panel>
  );

  const activeChip = <StatusChip status="ready" label="active" size="small" />;

  return (
    <Page
      title="MCP allowlist"
      subtitle={
        <>
          Pick which operations LLM agents see through <code>/mcp</code>. Globs
          are dot-segmented (<code>*</code> matches one segment,{' '}
          <code>**</code> matches any number). Internal <code>_*</code>{' '}
          namespaces are filtered regardless.
        </>
      }
    >
      {autoPanel}

      <Panel
        title={
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
            <span>Include</span>
            {!autoInclude && activeChip}
          </Stack>
        }
        subtitle={
          autoInclude
            ? 'Not consulted while auto-include is on.'
            : 'Operations matched here are exposed; everything else is hidden.'
        }
      >
        <Stack direction="row" spacing={1} sx={{ mb: 2 }}>
          <TextField
            size="small"
            placeholder="e.g. greeter.**"
            value={includeDraft}
            onChange={(e) => setIncludeDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && includeDraft.trim()) {
                includeAdd.mutate(includeDraft.trim());
              }
            }}
            fullWidth
          />
          <Button
            variant="contained"
            disabled={!includeDraft.trim() || includeAdd.isPending}
            onClick={() => includeAdd.mutate(includeDraft.trim())}
          >
            Add
          </Button>
        </Stack>
        <GlobList items={include} onRemove={(p) => includeRemove.mutate(p)} empty="No include patterns." />
      </Panel>

      <Panel
        title={
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
            <span>Exclude</span>
            {autoInclude && activeChip}
          </Stack>
        }
        subtitle={
          autoInclude
            ? 'Subtracted from the auto-included surface.'
            : 'Not consulted while auto-include is off.'
        }
      >
        <Stack direction="row" spacing={1} sx={{ mb: 2 }}>
          <TextField
            size="small"
            placeholder="e.g. admin.**"
            value={excludeDraft}
            onChange={(e) => setExcludeDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && excludeDraft.trim()) {
                excludeAdd.mutate(excludeDraft.trim());
              }
            }}
            fullWidth
          />
          <Button
            variant="contained"
            disabled={!excludeDraft.trim() || excludeAdd.isPending}
            onClick={() => excludeAdd.mutate(excludeDraft.trim())}
          >
            Add
          </Button>
        </Stack>
        <GlobList items={exclude} onRemove={(p) => excludeRemove.mutate(p)} empty="No exclude patterns." />
      </Panel>

      <Panel
        title={`Allowed operations (${entries.length})`}
        subtitle={
          <>
            Live preview of what an MCP agent sees from <code>schema_list</code>.
          </>
        }
        flush
      >
        <DataTable
          columns={columns}
          rows={rows}
          getRowId={(e) => `${e.path}`}
          emptyTitle="No operations match the current allowlist"
          emptyDescription="Add an include pattern (or enable auto-include)."
        />
      </Panel>
    </Page>
  );
}

function GlobList({
  items,
  onRemove,
  empty,
}: {
  items: string[];
  onRemove: (path: string) => void;
  empty: string;
}) {
  if (items.length === 0) {
    return (
      <Typography variant="body2" color="text.secondary">
        {empty}
      </Typography>
    );
  }
  return (
    <Stack spacing={0.5}>
      {items.map((p) => (
        <Box
          key={p}
          sx={{
            display: 'flex',
            flexDirection: 'row',
            alignItems: 'center',
            gap: 1,
            px: 1,
            py: 0.25,
            borderRadius: 1,
            '&:hover': { backgroundColor: 'action.hover' },
          }}
        >
          <code style={{ flex: 1 }}>{p}</code>
          <Tooltip title="Remove">
            <IconButton size="small" onClick={() => onRemove(p)}>
              <DeleteIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        </Box>
      ))}
    </Stack>
  );
}
