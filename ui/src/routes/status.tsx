import { createFileRoute } from '@tanstack/react-router';
import {
  Alert,
  Box,
  FormControl,
  InputLabel,
  MenuItem,
  Select,
  Tooltip,
  Typography,
  type Theme,
} from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { client } from '@/api/client';
import { DashboardQuery } from '@/api/operations';
import { DataTable, Page, Panel, StatusChip, useToast, type Column } from '@/ui';

export const Route = createFileRoute('/status')({
  component: Dashboard,
});

// Public status page: one row per (namespace, version) with calls +
// p50/p95/p99 + a horizontal dot-strip across the chosen window. The
// data is from the public /admin/services/history endpoint; each dot
// is one ring-bucket of the underlying stats ring (1s / 1m / 10m for
// 1m / 1h / 24h).
//
// Plan §2 — admin reads are public, so this works without an admin
// token; mutations stay gated.

type Window = '1m' | '1h' | '24h';

const windowOptions: { value: Window; label: string; sliceLabel: string }[] = [
  { value: '1m', label: 'Last minute', sliceLabel: '1s slices' },
  { value: '1h', label: 'Last hour', sliceLabel: '1m slices' },
  { value: '24h', label: 'Last 24 hours', sliceLabel: '10m slices' },
];

interface Row {
  id: string;
  namespace: string;
  version: string;
  buckets: ReadonlyArray<DashboardBucket | null>;
  count: number;
  errors: number;
  p50: number;
  p95: number;
  p99: number;
}

function Dashboard() {
  const toast = useToast();
  const [windowVal, setWindow] = useState<Window>('1h');
  const { data, isLoading, error } = useQuery({
    queryKey: ['dashboard', windowVal],
    queryFn: () => client.request(DashboardQuery, { window: windowVal }),
    refetchInterval: 5_000,
  });
  const errorMessage = error?.message;
  useEffect(() => {
    if (errorMessage) toast.error(errorMessage);
  }, [errorMessage, toast]);

  const opt = windowOptions.find((o) => o.value === windowVal)!;
  const rows: Row[] = (data?.admin?.servicesHistory?.services ?? []).flatMap((s, i) => {
    if (!s) return [];
    const buckets = s.buckets ?? [];
    let count = 0;
    let okCount = 0;
    let p50 = 0;
    let p95 = 0;
    let p99 = 0;
    for (const b of buckets) {
      if (!b) continue;
      count += Number(b.count);
      okCount += Number(b.okCount);
      if (Number(b.p50Millis) > p50) p50 = Number(b.p50Millis);
      if (Number(b.p95Millis) > p95) p95 = Number(b.p95Millis);
      if (Number(b.p99Millis) > p99) p99 = Number(b.p99Millis);
    }
    return [
      {
        id: `${s.namespace}@${s.version}@${i}`,
        namespace: s.namespace,
        version: s.version,
        buckets,
        count,
        errors: count - okCount,
        p50,
        p95,
        p99,
      },
    ];
  });

  const columns: Column<Row>[] = [
    {
      id: 'service',
      header: 'Service',
      sortValue: (r) => r.namespace,
      cell: (r) => (
        <>
          <Typography component="span" sx={{ fontWeight: 500 }}>
            {r.namespace}
          </Typography>{' '}
          <Typography component="span" color="text.secondary" variant="body2">
            {r.version}
          </Typography>
        </>
      ),
    },
    {
      id: 'health',
      header: 'Health',
      sortValue: (r) => r.errors,
      cell: (r) =>
        r.count === 0 ? (
          <StatusChip status="waiting" label="No traffic" size="small" />
        ) : r.errors > 0 ? (
          <StatusChip status="degraded" label="Errors" size="small" />
        ) : (
          <StatusChip status="ready" label="OK" size="small" />
        ),
    },
    {
      id: 'calls',
      header: 'Calls',
      align: 'right',
      sortValue: (r) => r.count,
      cell: (r) => formatCount(r.count),
    },
    {
      id: 'errors',
      header: 'Errors',
      align: 'right',
      sortValue: (r) => r.errors,
      cell: (r) =>
        r.errors > 0 ? (
          <Typography component="span" color="error" variant="inherit">
            {formatCount(r.errors)}
          </Typography>
        ) : (
          '—'
        ),
    },
    {
      id: 'p50',
      header: <>p50&nbsp;ms</>,
      align: 'right',
      sortValue: (r) => r.p50,
      cell: (r) => (r.count > 0 ? r.p50 : '—'),
    },
    {
      id: 'p95',
      header: <>p95&nbsp;ms</>,
      align: 'right',
      sortValue: (r) => r.p95,
      cell: (r) => (r.count > 0 ? r.p95 : '—'),
    },
    {
      id: 'p99',
      header: <>p99&nbsp;ms</>,
      align: 'right',
      sortValue: (r) => r.p99,
      cell: (r) => (r.count > 0 ? r.p99 : '—'),
    },
    {
      id: 'strip',
      header: opt.label,
      width: 340,
      cell: (r) => <DotStrip buckets={r.buckets} />,
    },
  ];

  return (
    <Page
      title="Status"
      subtitle="Per-service calls, errors and latency. Public: no admin token needed."
      actions={
        <FormControl size="small" sx={{ minWidth: 220 }}>
          <InputLabel id="window-label">Window</InputLabel>
          <Select
            labelId="window-label"
            label="Window"
            value={windowVal}
            onChange={(e) => setWindow(e.target.value)}
          >
            {windowOptions.map((o) => (
              <MenuItem key={o.value} value={o.value}>
                {o.label} ({o.sliceLabel})
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      }
    >
      {error && <Alert severity="error">{error.message}</Alert>}
      <Panel
        flush
        footer={
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 2, flexWrap: 'wrap' }}>
            <LegendDot color="none" label="no traffic" />
            <LegendDot color="ok" label="ok" />
            <LegendDot color="warn" label="< 5% errors" />
            <LegendDot color="bad" label="≥ 5% errors" />
          </Box>
        }
      >
        <DataTable
          columns={columns}
          rows={rows}
          getRowId={(r) => r.id}
          loading={isLoading}
          emptyTitle="No services registered"
          emptyDescription="The status table populates as services register against the gateway."
        />
      </Panel>
    </Page>
  );
}

// DotStrip renders one dot per bucket, oldest-on-the-left. Color rule:
// no traffic → grey; otherwise error_ratio thresholds (< 5% yellow,
// ≥ 5% red) — the standard "status page" eyeball.
function DotStrip({
  buckets,
}: {
  buckets: ReadonlyArray<DashboardBucket | null>;
}) {
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'center',
        gap: '2px',
        flexWrap: 'nowrap',
        minWidth: 120,
        overflow: 'hidden',
      }}
    >
      {buckets.map((b, i) => {
        if (!b) {
          return <Dot key={i} kind="none" title="no data" />;
        }
        const count = Number(b.count);
        const okCount = Number(b.okCount);
        const errors = count - okCount;
        const errorRatio = count > 0 ? errors / count : 0;
        const kind = bucketKind(count, errorRatio);
        const start = new Date(Number(b.startUnixSec) * 1000);
        const tip =
          count === 0
            ? `${start.toLocaleTimeString()} — no traffic`
            : `${start.toLocaleTimeString()} · ${count} calls · ${errors} errors${
                errorRatio > 0
                  ? ` (${(errorRatio * 100).toFixed(1)}%)`
                  : ''
              }`;
        return <Dot key={i} kind={kind} title={tip} />;
      })}
    </Box>
  );
}

type DashboardBucket = {
  startUnixSec: unknown;
  durationSec: unknown;
  count: unknown;
  okCount: unknown;
  p50Millis: unknown;
  p95Millis: unknown;
  p99Millis: unknown;
};

type DotKind = 'none' | 'ok' | 'warn' | 'bad';

function dotColor(t: Theme, kind: DotKind): string {
  switch (kind) {
    case 'ok':
      return t.palette.success.main;
    case 'warn':
      return t.palette.warning.main;
    case 'bad':
      return t.palette.error.main;
    default:
      return t.palette.action.disabledBackground;
  }
}

function Dot({ kind, title }: { kind: DotKind; title: string }) {
  return (
    <Tooltip title={title} placement="top" arrow disableInteractive>
      <Box
        sx={{
          flex: '1 1 0',
          minWidth: 2,
          maxWidth: 10,
          height: 16,
          borderRadius: '2px',
          backgroundColor: (t) => dotColor(t, kind),
        }}
      />
    </Tooltip>
  );
}

function LegendDot({ color, label }: { color: DotKind; label: string }) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      <Box
        sx={{
          width: 10,
          height: 10,
          borderRadius: '50%',
          backgroundColor: (t) => dotColor(t, color),
        }}
      />
      <Typography variant="caption" color="text.secondary">
        {label}
      </Typography>
    </Box>
  );
}

function bucketKind(count: number, errorRatio: number): DotKind {
  if (count === 0) return 'none';
  if (errorRatio === 0) return 'ok';
  if (errorRatio < 0.05) return 'warn';
  return 'bad';
}

function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}
