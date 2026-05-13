import { createFileRoute } from '@tanstack/react-router';
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
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
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import { useQuery } from '@tanstack/react-query';
import { client } from '@/api/client';
import { DeprecatedStatsQuery } from '@/api/operations';
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
  if (error)
    return <Typography color="error">{(error as Error).message}</Typography>;

  const services = data?.admin?.deprecatedStats?.services ?? [];

  if (services.length === 0) {
    return (
      <Alert severity="success">
        Nothing deprecated. No services flagged manually and no namespace has
        an older numbered cut still registered.
      </Alert>
    );
  }

  // Split into "has traffic" and "safe to retire" so the operator
  // gets a visible separator between the two follow-up actions.
  // Server already sorts by totalCount desc; we just slice on the
  // first zero-count row.
  const withTraffic = services.filter((s) => s !== null && Number(s.totalCount) > 0);
  const safeToRetire = services.filter((s) => s !== null && Number(s.totalCount) === 0);

  return (
    <Stack spacing={3}>
      <Box>
        <Typography variant="h6" gutterBottom>
          Deprecated services with recent traffic
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          Last 24h. The "should I chase this?" report — high call volume at
          the top means consumers haven't migrated. Each service expands to
          its per-method + per-caller breakdown.
        </Typography>
        {withTraffic.length === 0 ? (
          <Alert severity="info">
            No deprecated service has been called in the last 24h. See the
            "Safe to retire" section below.
          </Alert>
        ) : (
          withTraffic.map(
            (s, i) => s && <ServiceCard key={i} service={s} />,
          )
        )}
      </Box>

      <Box>
        <Typography variant="h6" gutterBottom>
          Safe to retire
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          Deprecated services with zero recorded traffic in the last 24h. No
          detected callers — likely safe to deregister.
        </Typography>
        {safeToRetire.length === 0 ? (
          <Alert severity="info">
            Every deprecated service still has traffic. Nothing safe to drop
            yet.
          </Alert>
        ) : (
          <TableContainer component={Paper} variant="outlined">
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Namespace</TableCell>
                  <TableCell>Version</TableCell>
                  <TableCell>Reason</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {safeToRetire.map((s, i) => {
                  if (!s) return null;
                  return (
                    <TableRow key={i}>
                      <TableCell>{s.namespace}</TableCell>
                      <TableCell>{s.version}</TableCell>
                      <TableCell>
                        <DeprecationBadges
                          manualReason={s.manualReason ?? ''}
                          autoReason={s.autoReason ?? ''}
                        />
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </Box>
    </Stack>
  );
}

type ServiceRow = NonNullable<
  NonNullable<
    NonNullable<ResultOf<typeof DeprecatedStatsQuery>['admin']>['deprecatedStats']
  >['services'][number]
>;

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
          <TableContainer component={Paper} variant="outlined">
            <Table size="small">
              <TableHead>
                <TableRow>
                  <TableCell>Method</TableCell>
                  <TableCell>Caller</TableCell>
                  <TableCell align="right">Count</TableCell>
                  <TableCell align="right">p50&nbsp;ms</TableCell>
                  <TableCell align="right">p95&nbsp;ms</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {methods.map((m, mi) => {
                  if (!m) return null;
                  const callers = m.callers ?? [];
                  if (callers.length === 0) {
                    return (
                      <TableRow key={mi}>
                        <TableCell>{m.method}</TableCell>
                        <TableCell colSpan={4}>
                          <Typography variant="body2" color="text.secondary">
                            no callers recorded
                          </Typography>
                        </TableCell>
                      </TableRow>
                    );
                  }
                  return callers.map((c, ci) => {
                    if (!c) return null;
                    return (
                      <TableRow key={`${mi}-${ci}`}>
                        <TableCell>{ci === 0 ? m.method : ''}</TableCell>
                        <TableCell>{c.caller}</TableCell>
                        <TableCell align="right">
                          {formatCount(Number(c.count))}
                        </TableCell>
                        <TableCell align="right">
                          {Number(c.p50Millis)}
                        </TableCell>
                        <TableCell align="right">
                          {Number(c.p95Millis)}
                        </TableCell>
                      </TableRow>
                    );
                  });
                })}
              </TableBody>
            </Table>
          </TableContainer>
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
          <Chip
            label="manual"
            size="small"
            color="error"
            variant="outlined"
          />
        </Tooltip>
      )}
      {autoReason && (
        <Tooltip title={`auto: ${autoReason}`}>
          <Chip
            label="auto"
            size="small"
            color="warning"
            variant="outlined"
          />
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
