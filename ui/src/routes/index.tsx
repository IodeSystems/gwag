import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  FormControl,
  InputLabel,
  MenuItem,
  Paper,
  Select,
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
import { useState } from 'react';
import { client } from '@/api/client';
import { DashboardQuery } from '@/api/operations';

export const Route = createFileRoute('/')({
  component: Dashboard,
});

// The unauth landing acts as a public status page: one row per
// (namespace, version) with calls + p50/p95/p99 + a horizontal
// dot-strip across the chosen window. The data is from the public
// /admin/services/history endpoint; each dot is one ring-bucket of
// the underlying stats ring (1s / 1m / 10m for 1m / 1h / 24h).
//
// Plan §2 — admin reads are public, so this works without an admin
// token; mutations stay gated.

type Window = '1m' | '1h' | '24h';

const windowOptions: { value: Window; label: string; sliceLabel: string }[] = [
  { value: '1m', label: 'Last minute', sliceLabel: '1s slices' },
  { value: '1h', label: 'Last hour', sliceLabel: '1m slices' },
  { value: '24h', label: 'Last 24 hours', sliceLabel: '10m slices' },
];

function Dashboard() {
  const [windowVal, setWindow] = useState<Window>('1h');
  const { data, isLoading, error } = useQuery({
    queryKey: ['dashboard', windowVal],
    queryFn: () => client.request(DashboardQuery, { window: windowVal }),
    refetchInterval: 5_000,
  });

  const services = data?.admin?.servicesHistory?.services ?? [];
  const opt = windowOptions.find((o) => o.value === windowVal)!;

  return (
    <Box>
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'row',
          alignItems: 'center',
          gap: 2,
          mb: 2,
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="h5">Status</Typography>
        <FormControl size="small" sx={{ minWidth: 180 }}>
          <InputLabel id="window-label">Window</InputLabel>
          <Select
            labelId="window-label"
            label="Window"
            value={windowVal}
            onChange={(e) => setWindow(e.target.value as Window)}
          >
            {windowOptions.map((o) => (
              <MenuItem key={o.value} value={o.value}>
                {o.label} ({o.sliceLabel})
              </MenuItem>
            ))}
          </Select>
        </FormControl>
        <Box sx={{ flexGrow: 1 }} />
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'row',
            alignItems: 'center',
            gap: 2,
          }}
        >
          <LegendDot color="#cccccc" label="no traffic" />
          <LegendDot color="#4caf50" label="ok" />
          <LegendDot color="#ff9800" label="< 5% errors" />
          <LegendDot color="#f44336" label="≥ 5% errors" />
        </Box>
      </Box>

      {isLoading && <Typography>Loading…</Typography>}
      {error && (
        <Typography color="error">{(error as Error).message}</Typography>
      )}

      {!isLoading && !error && services.length === 0 && (
        <Typography color="text.secondary">
          No services registered. The status table populates as services
          register against the gateway.
        </Typography>
      )}

      {services.length > 0 && (
        <TableContainer component={Paper}>
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Service</TableCell>
                <TableCell align="right">Calls</TableCell>
                <TableCell align="right">Errors</TableCell>
                <TableCell align="right">p50&nbsp;ms</TableCell>
                <TableCell align="right">p95&nbsp;ms</TableCell>
                <TableCell align="right">p99&nbsp;ms</TableCell>
                <TableCell>{opt.label}</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {services.map((s, i) => {
                if (!s) return null;
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
                const errors = count - okCount;
                return (
                  <TableRow key={`${s.namespace}@${s.version}@${i}`}>
                    <TableCell>
                      <Typography component="span" sx={{ fontWeight: 500 }}>
                        {s.namespace}
                      </Typography>{' '}
                      <Typography
                        component="span"
                        color="text.secondary"
                        variant="body2"
                      >
                        {s.version}
                      </Typography>
                    </TableCell>
                    <TableCell align="right">{formatCount(count)}</TableCell>
                    <TableCell align="right">
                      {errors > 0 ? (
                        <Typography component="span" color="error">
                          {formatCount(errors)}
                        </Typography>
                      ) : (
                        '—'
                      )}
                    </TableCell>
                    <TableCell align="right">
                      {count > 0 ? p50 : '—'}
                    </TableCell>
                    <TableCell align="right">
                      {count > 0 ? p95 : '—'}
                    </TableCell>
                    <TableCell align="right">
                      {count > 0 ? p99 : '—'}
                    </TableCell>
                    <TableCell sx={{ minWidth: 320 }}>
                      <DotStrip buckets={buckets} />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableContainer>
      )}
    </Box>
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
        overflow: 'hidden',
      }}
    >
      {buckets.map((b, i) => {
        if (!b) {
          return <Dot key={i} color="#cccccc" title="no data" />;
        }
        const count = Number(b.count);
        const okCount = Number(b.okCount);
        const errors = count - okCount;
        const errorRatio = count > 0 ? errors / count : 0;
        const color = bucketColor(count, errorRatio);
        const start = new Date(Number(b.startUnixSec) * 1000);
        const tip =
          count === 0
            ? `${start.toLocaleTimeString()} — no traffic`
            : `${start.toLocaleTimeString()} · ${count} calls · ${errors} errors${
                errorRatio > 0
                  ? ` (${(errorRatio * 100).toFixed(1)}%)`
                  : ''
              }`;
        return <Dot key={i} color={color} title={tip} />;
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

function Dot({ color, title }: { color: string; title: string }) {
  return (
    <Tooltip title={title} placement="top" arrow disableInteractive>
      <Box
        sx={{
          flex: '0 0 auto',
          width: 10,
          height: 16,
          borderRadius: '2px',
          backgroundColor: color,
        }}
      />
    </Tooltip>
  );
}

function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'row',
        alignItems: 'center',
        gap: 0.5,
      }}
    >
      <Box
        sx={{
          width: 10,
          height: 10,
          borderRadius: '50%',
          backgroundColor: color,
        }}
      />
      <Typography variant="caption" color="text.secondary">
        {label}
      </Typography>
    </Box>
  );
}

function bucketColor(count: number, errorRatio: number): string {
  if (count === 0) return '#cccccc';
  if (errorRatio === 0) return '#4caf50';
  if (errorRatio < 0.05) return '#ff9800';
  return '#f44336';
}

function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}
