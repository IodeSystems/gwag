import { createFileRoute } from '@tanstack/react-router';
import {
  Alert,
  Box,
  Button,
  Chip,
  FormControlLabel,
  IconButton,
  Paper,
  Snackbar,
  Stack,
  Switch,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
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

export const Route = createFileRoute('/mcp')({
  component: McpConfig,
});

// MCP config page: operator curates which operations LLM agents see
// through /mcp. Default-deny — the allowlist is exactly Include
// unless auto_include flips it to "all public ops minus Exclude".
// Internal `_*` namespaces are filtered regardless. Live preview
// pulls from admin.mcpSchemaList so the operator sees the surface
// they're sculpting in real time.
function McpConfig() {
  const qc = useQueryClient();
  const [toast, setToast] = useState<string | null>(null);
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
      invalidate();
      setIncludeDraft('');
      setToast('include added');
    },
    onError: (e: Error) => setToast(e.message),
  });

  const includeRemove = useMutation({
    mutationFn: (path: string) =>
      client.request(McpIncludeRemoveMutation, { path }),
    onSuccess: () => {
      invalidate();
      setToast('include removed');
    },
    onError: (e: Error) => setToast(e.message),
  });

  const excludeAdd = useMutation({
    mutationFn: (path: string) => client.request(McpExcludeMutation, { path }),
    onSuccess: () => {
      invalidate();
      setExcludeDraft('');
      setToast('exclude added');
    },
    onError: (e: Error) => setToast(e.message),
  });

  const excludeRemove = useMutation({
    mutationFn: (path: string) =>
      client.request(McpExcludeRemoveMutation, { path }),
    onSuccess: () => {
      invalidate();
      setToast('exclude removed');
    },
    onError: (e: Error) => setToast(e.message),
  });

  const toggleAuto = useMutation({
    mutationFn: (autoInclude: boolean) =>
      client.request(McpSetAutoIncludeMutation, { autoInclude }),
    onSuccess: (_resp, v) => {
      invalidate();
      setToast(v ? 'auto-include on' : 'auto-include off');
    },
    onError: (e: Error) => setToast(e.message),
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error)
    return <Typography color="error">{(error as Error).message}</Typography>;

  const cfg = data?.admin?.mcpList;
  const entries = data?.admin?.mcpSchemaList?.entries ?? [];
  const autoInclude = cfg?.autoInclude ?? false;
  const include = (cfg?.include ?? []).filter((s): s is string => s !== null);
  const exclude = (cfg?.exclude ?? []).filter((s): s is string => s !== null);

  return (
    <Stack spacing={3}>
      <Box>
        <Typography variant="h5" gutterBottom>
          MCP allowlist
        </Typography>
        <Typography color="text.secondary" variant="body2">
          Pick which operations LLM agents see through{' '}
          <code>/mcp</code>. Globs are dot-segmented (<code>*</code> matches
          one segment, <code>**</code> matches any number). Internal{' '}
          <code>_*</code> namespaces are filtered regardless.
        </Typography>
      </Box>

      <Paper sx={{ p: 2 }}>
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
      </Paper>

      <Paper sx={{ p: 2 }}>
        <Typography variant="h6" gutterBottom>
          Include {!autoInclude && <Chip size="small" label="active" />}
        </Typography>
        <Typography variant="caption" color="text.secondary" sx={{ mb: 1, display: 'block' }}>
          {autoInclude
            ? 'Not consulted while auto-include is on.'
            : 'Operations matched here are exposed; everything else is hidden.'}
        </Typography>
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
      </Paper>

      <Paper sx={{ p: 2 }}>
        <Typography variant="h6" gutterBottom>
          Exclude {autoInclude && <Chip size="small" label="active" />}
        </Typography>
        <Typography variant="caption" color="text.secondary" sx={{ mb: 1, display: 'block' }}>
          {autoInclude
            ? 'Subtracted from the auto-included surface.'
            : 'Not consulted while auto-include is off.'}
        </Typography>
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
      </Paper>

      <Box>
        <Typography variant="h6" gutterBottom>
          Allowed operations ({entries.length})
        </Typography>
        <Typography variant="caption" color="text.secondary">
          Live preview of what an MCP agent sees from{' '}
          <code>schema_list</code>.
        </Typography>
        {entries.length === 0 ? (
          <Alert severity="info" sx={{ mt: 2 }}>
            No operations match the current allowlist. Add an include pattern
            (or enable auto-include).
          </Alert>
        ) : (
          <TableContainer component={Paper} sx={{ mt: 2 }}>
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Path</TableCell>
                  <TableCell>Kind</TableCell>
                  <TableCell>Namespace</TableCell>
                  <TableCell>Version</TableCell>
                  <TableCell>Description</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {entries.map((e, i) =>
                  e ? (
                    <TableRow key={i}>
                      <TableCell>
                        <code>{e.path}</code>
                      </TableCell>
                      <TableCell>{e.kind}</TableCell>
                      <TableCell>{e.namespace}</TableCell>
                      <TableCell>{e.version}</TableCell>
                      <TableCell sx={{ maxWidth: 360 }}>
                        <Typography
                          variant="body2"
                          color="text.secondary"
                          noWrap
                          title={e.description ?? ''}
                        >
                          {e.description}
                        </Typography>
                      </TableCell>
                    </TableRow>
                  ) : null,
                )}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </Box>

      <Snackbar
        open={toast !== null}
        autoHideDuration={3000}
        onClose={() => setToast(null)}
        message={toast ?? ''}
      />
    </Stack>
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
