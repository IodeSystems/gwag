import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControl,
  FormControlLabel,
  Radio,
  RadioGroup,
  Stack,
  Typography,
} from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import FilterListIcon from '@mui/icons-material/FilterList';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { sdk } from '@/api/client';

export const Route = createFileRoute('/schema')({
  component: Schema,
});

type Format = 'graphql' | 'openapi' | 'proto';

interface FormatSpec {
  label: string;
  basePath: string;
  contentType: 'text' | 'json' | 'proto-sdl';
  filename: string;
}

const formats: Record<Format, FormatSpec> = {
  graphql: {
    label: 'GraphQL SDL',
    basePath: '/api/schema/graphql',
    contentType: 'text',
    filename: 'schema.graphql',
  },
  openapi: {
    label: 'OpenAPI',
    basePath: '/api/schema/openapi',
    contentType: 'json',
    filename: 'schema.openapi.json',
  },
  proto: {
    // ?format=sdl asks the gateway to render each FileDescriptor as
    // proto SDL via jhump/protoreflect's protoprint and return a
    // JSON array of {name, sdl}. Decoding is done server-side; the
    // browser just concatenates the entries with `// --- name ---`
    // headers so it reads as one logical document.
    label: 'Protobuf SDL',
    basePath: '/api/schema/proto?format=sdl',
    contentType: 'proto-sdl',
    filename: 'schema.proto.txt',
  },
};

interface ServiceKey {
  namespace: string;
  version: string;
}

function Schema() {
  const [format, setFormat] = useState<Format>('graphql');
  const [filterOpen, setFilterOpen] = useState(false);
  const [picked, setPicked] = useState<Set<string>>(new Set()); // "ns:v"

  const spec = formats[format];
  const selector = useMemo(() => {
    if (picked.size === 0) return '';
    return Array.from(picked).join(',');
  }, [picked]);

  const url = useMemo(() => {
    if (!selector) return spec.basePath;
    const sep = spec.basePath.includes('?') ? '&' : '?';
    return `${spec.basePath}${sep}service=${encodeURIComponent(selector)}`;
  }, [spec.basePath, selector]);

  const { data, isLoading, error } = useQuery({
    queryKey: ['schema', format, selector],
    queryFn: async (): Promise<{ text: string; bytes: Blob }> => {
      const r = await fetch(url);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const bytes = await r.blob();
      let text: string;
      switch (spec.contentType) {
        case 'text':
          text = await bytes.text();
          break;
        case 'json':
          try {
            text = JSON.stringify(JSON.parse(await bytes.text()), null, 2);
          } catch {
            text = await bytes.text();
          }
          break;
        case 'proto-sdl': {
          // Server returns [{name, sdl}, ...].
          const entries = JSON.parse(await bytes.text()) as Array<{
            name: string;
            sdl: string;
          }>;
          text = entries
            .map((e) => `// === ${e.name} ===\n${e.sdl}`)
            .join('\n');
          break;
        }
      }
      return { text, bytes };
    },
  });

  const downloadCurrent = () => {
    if (!data) return;
    // For proto-sdl we re-package the rendered text rather than
    // dumping the raw JSON envelope.
    const payload =
      spec.contentType === 'proto-sdl' ? new Blob([data.text]) : data.bytes;
    const blobUrl = URL.createObjectURL(payload);
    const a = document.createElement('a');
    a.href = blobUrl;
    a.download = spec.filename;
    a.click();
    URL.revokeObjectURL(blobUrl);
  };

  return (
    <Box
      sx={{
        m: -3,
        height: 'calc(100vh - 64px)',
        display: 'flex',
        flexDirection: 'column',
      }}
    >
      <Box
        sx={{
          px: 2,
          py: 1,
          borderBottom: 1,
          borderColor: 'divider',
          display: 'flex',
          alignItems: 'center',
          gap: 2,
        }}
      >
        <FormControl>
          <RadioGroup
            row
            value={format}
            onChange={(e) => setFormat(e.target.value as Format)}
          >
            {(Object.keys(formats) as Format[]).map((k) => (
              <FormControlLabel
                key={k}
                value={k}
                control={<Radio size="small" />}
                label={formats[k].label}
              />
            ))}
          </RadioGroup>
        </FormControl>
        <Box sx={{ flex: 1 }} />
        <Button
          size="small"
          startIcon={<FilterListIcon />}
          onClick={() => setFilterOpen(true)}
        >
          {picked.size > 0 ? `Filter (${picked.size})` : 'Filter'}
        </Button>
        <Button
          size="small"
          startIcon={<DownloadIcon />}
          disabled={!data}
          onClick={downloadCurrent}
        >
          Download
        </Button>
      </Box>
      <Box
        component="pre"
        sx={{
          flex: 1,
          m: 0,
          p: 2,
          overflow: 'auto',
          fontFamily: 'monospace',
          fontSize: 12,
          whiteSpace: 'pre',
        }}
      >
        {isLoading && <Typography>Loading…</Typography>}
        {error && (
          <Typography color="error">{(error as Error).message}</Typography>
        )}
        {!isLoading && !error && data?.text}
      </Box>

      <FilterDialog
        open={filterOpen}
        onClose={() => setFilterOpen(false)}
        picked={picked}
        onApply={(next) => {
          setPicked(next);
          setFilterOpen(false);
        }}
      />
    </Box>
  );
}

function FilterDialog({
  open,
  onClose,
  picked,
  onApply,
}: {
  open: boolean;
  onClose: () => void;
  picked: Set<string>;
  onApply: (next: Set<string>) => void;
}) {
  // Local draft so the parent's URL doesn't refetch on every check.
  const [draft, setDraft] = useState<Set<string>>(picked);

  // Refetch service list each time the dialog opens — operators run
  // dynamic registrations all the time, and a stale checkbox set
  // would silently drop "new" services from the selector.
  const { data, isLoading, error } = useQuery({
    queryKey: ['filter-services'],
    queryFn: () => sdk.Services(),
    enabled: open,
  });

  const all = useMemo(() => {
    const out: ServiceKey[] = [];
    for (const s of data?.admin?.listServices?.services ?? []) {
      if (s) out.push({ namespace: s.namespace, version: s.version });
    }
    out.sort((a, b) =>
      a.namespace === b.namespace
        ? a.version.localeCompare(b.version)
        : a.namespace.localeCompare(b.namespace),
    );
    return out;
  }, [data]);

  const toggle = (key: string) => {
    setDraft((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>Filter schema by service</DialogTitle>
      <DialogContent dividers>
        {isLoading && <Typography>Loading…</Typography>}
        {error && (
          <Typography color="error">{(error as Error).message}</Typography>
        )}
        {!isLoading && !error && all.length === 0 && (
          <Typography color="text.secondary">
            No services registered. Register a service first
            (`bin/bench service add ...`).
          </Typography>
        )}
        <Stack>
          {all.map((s) => {
            const key = `${s.namespace}:${s.version}`;
            return (
              <FormControlLabel
                key={key}
                control={
                  <Checkbox
                    size="small"
                    checked={draft.has(key)}
                    onChange={() => toggle(key)}
                  />
                }
                label={
                  <Typography sx={{ fontFamily: 'monospace' }}>
                    {key}
                  </Typography>
                }
              />
            );
          })}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={() => setDraft(new Set())} disabled={draft.size === 0}>
          Clear
        </Button>
        <Box sx={{ flex: 1 }} />
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="contained" onClick={() => onApply(draft)}>
          Apply
        </Button>
      </DialogActions>
    </Dialog>
  );
}
