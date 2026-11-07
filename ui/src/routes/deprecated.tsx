import { createFileRoute } from '@tanstack/react-router';
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Box,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import { useQuery } from '@tanstack/react-query';
import { client } from '@/api/client';
import { DeprecatedStatsQuery } from '@/api/operations';
import { DataTable, EmptyState, Page, Panel, StatusChip, type Column } from '@/ui';
import type { ResultOf } from '@graphql-typed-document-node/core';

export const Route = createFileRoute('/deprecated')({
  component: Deprecated,
});

// Plan §5: cross-service "should I retire this?" panel. Server
// returns one entry per deprecated (ns, ver) with manual + auto
// reasons (auto-deprecation = older vN), nested per-(method, caller)
// breakdown. Sorted by call volume desc — high-traffic surfaces
// first ("chase this"); zero-traffic services bubble up at the
// bottom as safe-to-retire candidates.
function Deprecated() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['deprecatedStats'],
    queryFn: () => client.request(DeprecatedStatsQuery),
    refetchInterval: 10_000,
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{error.message}</Typography>;

  const services = data?.admin?.deprecatedStats?.services ?? [];

  if (services.length === 0) {
    return (
      <Page title="Deprecated">
        <Panel>
          <EmptyState
            title="Nothing deprecated"
            description="No services flagged manually and no namespace has an older numbered cut still registered."
          />
        </Panel>
      </Page>
    );
  }

  // Split into "has traffic" and "safe to retire" so the operator
  // gets a visible separator between the two follow-up actions.
  // Server already sorts by totalCount desc; we just slice on the
  // first zero-count row.
  const withTraffic = services.filter((s): s is ServiceRow => s !== null && Number(s.totalCount) > 0);
  const safeToRetire = services.filter((s): s is ServiceRow => s !== null && Number(s.totalCount) === 0);

  return (
    <Page title="Deprecated">
      <Panel
        title="Deprecated services with recent traffic"
        subtitle={
          'Last 24h. The "should I chase this?" report — high call volume at the top means consumers haven\'t migrated. Each service expands to its per-method + per-caller breakdown.'
        }
      >
        {withTraffic.length === 0 ? (
          <EmptyState
            compact
            title="No recent traffic"
            description={'No deprecated service has been called in the last 24h. See the "Safe to retire" section below.'}
          />
        ) : (
          <Stack spacing={1}>
            {withTraffic.map((s, i) => (
              <ServiceCard key={i} service={s} />
            ))}
          </Stack>
        )}
      </Panel>

      <Panel
        title="Safe to retire"
        subtitle="Deprecated services with zero recorded traffic in the last 24h. No detected callers — likely safe to deregister."
        flush
      >
        <DataTable
          columns={retireColumns}
          rows={safeToRetire}
          getRowId={(s) => `${s.namespace}:${s.version}`}
          emptyTitle="Nothing safe to drop yet"
          emptyDescription="Every deprecated service still has traffic."
        />
      </Panel>
    </Page>
  );
}

// `services` is `T[] | null`, so null has to be stripped from the ARRAY before
// `[number]` can index it — `NonNullable<X['services']>[number]`, not
// `NonNullable<X['services'][number]>`. The outer wrap then drops null from the
// element itself.
type ServiceRow = NonNullable<
  NonNullable<
    NonNullable<
      NonNullable<ResultOf<typeof DeprecatedStatsQuery>['admin']>['deprecatedStats']
    >['services']
  >[number]
>;

const retireColumns: Column<ServiceRow>[] = [
  { id: 'namespace', header: 'Namespace', cell: (s) => s.namespace, sortValue: (s) => s.namespace },
  { id: 'version', header: 'Version', cell: (s) => s.version, sortValue: (s) => s.version },
  {
    id: 'reason',
    header: 'Reason',
    cell: (s) => (
      <DeprecationBadges manualReason={s.manualReason ?? ''} autoReason={s.autoReason ?? ''} />
    ),
  },
];

type MethodRow = { key: string; method: string; caller: string | null; count: number | null; p50: number | null; p95: number | null };

const methodColumns: Column<MethodRow>[] = [
  { id: 'method', header: 'Method', cell: (r) => r.method },
  {
    id: 'caller',
    header: 'Caller',
    cell: (r) =>
      r.caller === null ? (
        <Typography variant="body2" color="text.secondary">
          no callers recorded
        </Typography>
      ) : (
        r.caller
      ),
  },
  { id: 'count', header: 'Count', align: 'right', cell: (r) => (r.count === null ? '' : formatCount(r.count)) },
  { id: 'p50', header: <>p50&nbsp;ms</>, align: 'right', cell: (r) => r.p50 ?? '' },
  { id: 'p95', header: <>p95&nbsp;ms</>, align: 'right', cell: (r) => r.p95 ?? '' },
];

function methodRows(methods: NonNullable<ServiceRow['methods']>): MethodRow[] {
  const out: MethodRow[] = [];
  methods.forEach((m, mi) => {
    if (!m) return;
    const callers = m.callers ?? [];
    if (callers.length === 0) {
      out.push({ key: `${mi}`, method: m.method, caller: null, count: null, p50: null, p95: null });
      return;
    }
    callers.forEach((c, ci) => {
      if (!c) return;
      out.push({
        key: `${mi}-${ci}`,
        method: ci === 0 ? m.method : '',
        caller: c.caller,
        count: Number(c.count),
        p50: Number(c.p50Millis),
        p95: Number(c.p95Millis),
      });
    });
  });
  return out;
}

function ServiceCard({ service }: { service: ServiceRow }) {
  const methods = service.methods ?? [];
  return (
    <Accordion defaultExpanded={methods.length <= 3}>
      <AccordionSummary expandIcon={<ExpandMoreIcon />}>
        <Stack
          direction="row"
          spacing={1.5}
          sx={{ width: '100%', flexWrap: 'wrap', alignItems: 'center' }}
        >
          <Typography variant="subtitle1">
            {service.namespace} {service.version}
          </Typography>
          <DeprecationBadges
            manualReason={service.manualReason ?? ''}
            autoReason={service.autoReason ?? ''}
          />
          <Box sx={{ flexGrow: 1 }} />
          <Typography variant="body2" color="text.secondary">
            {formatCount(Number(service.totalCount))} calls /{' '}
            {service.totalThroughput.toFixed(2)} rps
          </Typography>
        </Stack>
      </AccordionSummary>
      <AccordionDetails>
        {methods.length === 0 ? (
          <Typography color="text.secondary" variant="body2">
            No per-method breakdown available.
          </Typography>
        ) : (
          <DataTable columns={methodColumns} rows={methodRows(methods)} getRowId={(r) => r.key} />
        )}
      </AccordionDetails>
    </Accordion>
  );
}

function DeprecationBadges({
  manualReason,
  autoReason,
}: {
  manualReason: string;
  autoReason: string;
}) {
  return (
    <Stack direction="row" spacing={0.5}>
      {manualReason && (
        <Tooltip title={`manual: ${manualReason}`}>
          <span>
            <StatusChip status="degraded" label="manual" size="small" />
          </span>
        </Tooltip>
      )}
      {autoReason && (
        <Tooltip title={`auto: ${autoReason}`}>
          <span>
            <StatusChip status="sunsetable" label="auto" size="small" />
          </span>
        </Tooltip>
      )}
    </Stack>
  );
}

function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}
