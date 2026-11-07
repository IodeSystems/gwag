import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { client } from '@/api/client';
import {
  ServicesQuery,
  ServiceStatsQuery,
  DeprecateMutation,
  UndeprecateMutation,
  RetractStableMutation,
} from '@/api/operations';
import {
  DataTable,
  DetailPanel,
  EmptyState,
  FormDialog,
  FormTextField,
  KeyValue,
  Page,
  Panel,
  StatusChip,
  useToast,
  type Column,
} from '@/ui';

export const Route = createFileRoute('/services')({
  component: Services,
});

// tierOf maps a §4 version string to its tier label.
//   "unstable" → "unstable"
//   "v<N>"     → "vN"  (the literal numbered cut tier)
// Unknown shapes — shouldn't appear given parseVersion canonicalisation —
// fall through to the raw string so operators see something rather than
// silently mislabelling.
function tierOf(version: string): string {
  if (version === 'unstable') return 'unstable';
  if (/^v[1-9][0-9]*$/.test(version)) return 'vN';
  return version;
}

// vNumber extracts N from "v<N>", or null when the version is unstable
// (or otherwise non-numbered). Used to match a row against the
// per-namespace stable target and to compute auto-deprecation.
function vNumber(version: string): number | null {
  const m = /^v([1-9][0-9]*)$/.exec(version);
  return m ? Number(m[1]) : null;
}

type DeprecateTarget = {
  namespace: string;
  version: string;
  currentReason: string;
};

type DrawerTarget = {
  namespace: string;
  version: string;
  hashHex: string;
};

type ServiceRow = {
  namespace: string;
  version: string;
  hashHex: string;
  replicaCount: number;
  tier: string;
  isStableTarget: boolean;
  stableTargetVN: number | undefined;
  isAutoDeprecated: boolean;
  isManualDeprecated: boolean;
  manualReason: string;
  stats:
    | { count: number; throughput: number; p50Millis: number; p95Millis: number }
    | undefined;
};

function Services() {
  const qc = useQueryClient();
  const toast = useToast();
  const [retractTarget, setRetractTarget] = useState<{
    namespace: string;
    currentVN: number;
    candidateVNs: number[];
  } | null>(null);
  const [deprecateTarget, setDeprecateTarget] =
    useState<DeprecateTarget | null>(null);
  const [drawerTarget, setDrawerTarget] = useState<DrawerTarget | null>(null);

  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => client.request(ServicesQuery),
    refetchInterval: 10_000,
  });

  const retract = useMutation({
    mutationFn: (vars: { namespace: string; targetVN: number }) =>
      client.request(RetractStableMutation, vars),
    onSuccess: (resp) => {
      void qc.invalidateQueries({ queryKey: ['services'] });
      const r = resp.admin.retractStable;
      toast.success(`stable v${r?.priorVN} → v${r?.newVN}`);
      setRetractTarget(null);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  // Errors propagate to FormDialog (mutateAsync), which shows them on the form.
  const deprecate = useMutation({
    mutationFn: (vars: {
      namespace: string;
      version: string;
      reason: string;
    }) => client.request(DeprecateMutation, vars),
    onSuccess: (_resp, vars) => {
      void qc.invalidateQueries({ queryKey: ['services'] });
      toast.success(`deprecated ${vars.namespace} ${vars.version}`);
    },
  });

  const undeprecate = useMutation({
    mutationFn: (vars: { namespace: string; version: string }) =>
      client.request(UndeprecateMutation, vars),
    onSuccess: (_resp, vars) => {
      void qc.invalidateQueries({ queryKey: ['services'] });
      toast.success(`undeprecated ${vars.namespace} ${vars.version}`);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  if (isLoading)
    return (
      <Page title="Services">
        <Typography>Loading…</Typography>
      </Page>
    );
  if (error)
    return (
      <Page title="Services">
        <Typography color="error">{error.message}</Typography>
      </Page>
    );

  const services = data?.admin?.listServices?.services ?? [];
  const stableEntries = data?.admin?.listServices?.stableVN ?? [];
  const statsRows = data?.admin?.servicesStats?.services ?? [];
  // Map<ns, currentStableVN> — the set of {namespace → vN} the renderer
  // is currently aliasing. RetractStable refuses to drop below an
  // unregistered vN, so the candidate set is "registered numbered cuts
  // for this namespace strictly below the current stable."
  const stableByNamespace = new Map<string, number>();
  for (const e of stableEntries) {
    if (e?.namespace && typeof e.vN === 'number') {
      stableByNamespace.set(e.namespace, e.vN);
    }
  }

  // Auto-deprecation matches the gateway's renderer: a numbered vN row
  // whose namespace has a strictly-greater vN registered is auto
  // deprecated (with the latest's reason as the fallback). Manual
  // deprecation is operator-set; the badge shows both routes.
  const maxVNByNamespace = new Map<string, number>();
  for (const s of services) {
    if (!s) continue;
    const n = vNumber(s.version);
    if (n === null) continue;
    const cur = maxVNByNamespace.get(s.namespace);
    if (cur === undefined || n > cur) maxVNByNamespace.set(s.namespace, n);
  }

  // Group registered vNs per namespace so we can hand the retract dialog
  // a candidate list.
  const vNsByNamespace = new Map<string, number[]>();
  for (const s of services) {
    if (!s) continue;
    const n = vNumber(s.version);
    if (n === null) continue;
    const arr = vNsByNamespace.get(s.namespace) ?? [];
    arr.push(n);
    vNsByNamespace.set(s.namespace, arr);
  }

  // Stats keyed by (ns, ver) so the row render is a single Map lookup.
  // unknown widths come back as bigint-shaped (`unknown` in codegen);
  // coerce to Number for display — counts top out well below 2^53 for
  // the 24h windows surfaced here.
  const statsByKey = new Map<string, NonNullable<ServiceRow['stats']>>();
  for (const r of statsRows) {
    if (!r) continue;
    statsByKey.set(`${r.namespace}@${r.version}`, {
      count: Number(r.count),
      throughput: r.throughput,
      p50Millis: Number(r.p50Millis),
      p95Millis: Number(r.p95Millis),
    });
  }

  const rows: ServiceRow[] = [];
  for (const s of services) {
    if (!s) continue;
    const stableTargetVN = stableByNamespace.get(s.namespace);
    const vN = vNumber(s.version);
    const maxVN = maxVNByNamespace.get(s.namespace);
    const manualReason = s.manualDeprecationReason ?? '';
    rows.push({
      namespace: s.namespace,
      version: s.version,
      hashHex: s.hashHex,
      replicaCount: s.replicaCount,
      tier: tierOf(s.version),
      isStableTarget: stableTargetVN !== undefined && vN === stableTargetVN,
      stableTargetVN,
      isAutoDeprecated: vN !== null && maxVN !== undefined && vN < maxVN,
      isManualDeprecated: manualReason.length > 0,
      manualReason,
      stats: statsByKey.get(`${s.namespace}@${s.version}`),
    });
  }

  const columns: Column<ServiceRow>[] = [
    { id: 'namespace', header: 'Namespace', cell: (r) => r.namespace },
    { id: 'version', header: 'Version', cell: (r) => r.version },
    {
      id: 'tier',
      header: 'Tier',
      cell: (r) => (
        <Stack direction="row" spacing={0.5}>
          <Chip
            label={r.tier}
            size="small"
            color={r.tier === 'unstable' ? 'warning' : 'default'}
          />
          {r.isStableTarget && (
            <StatusChip status="ready" size="small" label="stable" />
          )}
        </Stack>
      ),
    },
    {
      id: 'status',
      header: 'Status',
      cell: (r) =>
        r.isAutoDeprecated || r.isManualDeprecated ? (
          <Tooltip title={deprecationTooltip(r)}>
            <span>
              <StatusChip
                status={r.isManualDeprecated ? 'degraded' : 'sunsetable'}
                size="small"
                label={deprecationLabel(r)}
              />
            </span>
          </Tooltip>
        ) : null,
    },
    {
      id: 'replicas',
      header: 'Replicas',
      align: 'right',
      cell: (r) => r.replicaCount,
    },
    {
      id: 'calls',
      header: <>Calls&nbsp;/&nbsp;24h</>,
      align: 'right',
      cell: (r) => (r.stats ? formatCount(r.stats.count) : '—'),
    },
    {
      id: 'p50',
      header: <>p50&nbsp;ms</>,
      align: 'right',
      cell: (r) => (r.stats && r.stats.count > 0 ? r.stats.p50Millis : '—'),
    },
    {
      id: 'p95',
      header: <>p95&nbsp;ms</>,
      align: 'right',
      cell: (r) => (r.stats && r.stats.count > 0 ? r.stats.p95Millis : '—'),
    },
    {
      id: 'rate',
      header: <>Calls&nbsp;/&nbsp;s</>,
      align: 'right',
      cell: (r) =>
        r.stats && r.stats.count > 0 ? r.stats.throughput.toFixed(2) : '—',
    },
    {
      id: 'actions',
      header: 'Actions',
      align: 'right',
      cell: (r) => (
        <Box
          sx={{ display: 'flex', gap: 0.5, justifyContent: 'flex-end' }}
          onClick={(e) => e.stopPropagation()}
          onKeyDown={(e) => e.stopPropagation()}
        >
          {r.isManualDeprecated ? (
            <Button
              size="small"
              disabled={undeprecate.isPending}
              onClick={() =>
                undeprecate.mutate({
                  namespace: r.namespace,
                  version: r.version,
                })
              }
            >
              Undeprecate
            </Button>
          ) : (
            <Button
              size="small"
              color="error"
              onClick={() =>
                setDeprecateTarget({
                  namespace: r.namespace,
                  version: r.version,
                  currentReason: r.manualReason,
                })
              }
            >
              Deprecate
            </Button>
          )}
          {r.isStableTarget && r.stableTargetVN !== undefined && (
            <Button
              size="small"
              color="warning"
              onClick={() => {
                const current = r.stableTargetVN as number;
                const candidates = (vNsByNamespace.get(r.namespace) ?? [])
                  .filter((n) => n < current)
                  .sort((a, b) => b - a);
                setRetractTarget({
                  namespace: r.namespace,
                  currentVN: current,
                  candidateVNs: candidates,
                });
              }}
            >
              Retract Stable
            </Button>
          )}
        </Box>
      ),
    },
  ];

  return (
    <Page title="Services">
      <Panel flush>
        <DataTable
          columns={columns}
          rows={rows}
          getRowId={(r) => `${r.namespace}@${r.version}`}
          emptyTitle="No services registered"
          onRowClick={(r) =>
            setDrawerTarget({
              namespace: r.namespace,
              version: r.version,
              hashHex: r.hashHex,
            })
          }
        />
      </Panel>

      <Dialog
        open={retractTarget !== null}
        onClose={() => setRetractTarget(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>Retract stable for {retractTarget?.namespace}</DialogTitle>
        <DialogContent>
          <DialogContentText sx={{ mb: 2 }}>
            Currently <code>{retractTarget?.namespace}.stable</code> aliases v
            {retractTarget?.currentVN}. Retracting moves the alias to a lower
            registered vN — operator-driven only; the gateway never rolls
            stable back automatically. Pick the target:
          </DialogContentText>
          {retractTarget?.candidateVNs.length === 0 ? (
            <Typography color="text.secondary">
              No earlier registered vN to retract to. Ensure at least one prior
              vN is currently registered before retracting.
            </Typography>
          ) : (
            <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1 }}>
              {retractTarget?.candidateVNs.map((n) => (
                <Button
                  key={n}
                  variant="outlined"
                  size="small"
                  disabled={retract.isPending}
                  onClick={() =>
                    retract.mutate({
                      namespace: retractTarget.namespace,
                      targetVN: n,
                    })
                  }
                >
                  v{n}
                </Button>
              ))}
            </Box>
          )}
        </DialogContent>
        <DialogActions>
          <Button
            color="inherit"
            onClick={() => setRetractTarget(null)}
            disabled={retract.isPending}
          >
            Cancel
          </Button>
        </DialogActions>
      </Dialog>

      <FormDialog
        open={deprecateTarget !== null}
        title={`Deprecate ${deprecateTarget?.namespace ?? ''} ${deprecateTarget?.version ?? ''}`}
        description={
          <>
            Marks this (namespace, version) with{' '}
            <code>@deprecated(reason: …)</code> in SDL. Auto-deprecation of
            older numbered cuts is unaffected; manual reason takes precedence
            in rendering.
          </>
        }
        submitLabel="Deprecate"
        defaultValues={{ reason: '' }}
        onClose={() => setDeprecateTarget(null)}
        onSubmit={async (values) => {
          if (!deprecateTarget) return;
          await deprecate.mutateAsync({
            namespace: deprecateTarget.namespace,
            version: deprecateTarget.version,
            reason: values.reason.trim(),
          });
        }}
      >
        <FormTextField
          name="reason"
          label="Reason"
          placeholder="e.g. moved to v2 (breaks Authn header on /list)"
          autoFocus
          multiline
          rows={2}
        />
      </FormDialog>

      <ServiceDrawer
        target={drawerTarget}
        onClose={() => setDrawerTarget(null)}
      />
    </Page>
  );
}

function deprecationLabel({
  isAutoDeprecated,
  isManualDeprecated,
}: {
  isAutoDeprecated: boolean;
  isManualDeprecated: boolean;
}): string {
  if (isAutoDeprecated && isManualDeprecated) return 'deprecated · auto+manual';
  if (isManualDeprecated) return 'deprecated · manual';
  return 'deprecated · auto';
}

function deprecationTooltip({
  isAutoDeprecated,
  isManualDeprecated,
  manualReason,
}: {
  isAutoDeprecated: boolean;
  isManualDeprecated: boolean;
  manualReason: string;
}): string {
  const parts: string[] = [];
  if (isManualDeprecated) parts.push(`manual: ${manualReason}`);
  if (isAutoDeprecated)
    parts.push('auto: a higher vN exists in this namespace');
  return parts.join('  ·  ');
}

function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

function ServiceDrawer({
  target,
  onClose,
}: {
  target: DrawerTarget | null;
  onClose: () => void;
}) {
  // Per-method+caller breakdown for the selected (ns, ver). Per-replica
  // dimension is plan-§5 followup work; today's `g.Snapshot` keys on
  // (ns, ver, method, caller). The drawer shows whichever caller
  // dimension the gateway captured.
  const { data, isLoading, error } = useQuery({
    queryKey: ['serviceStats', target?.namespace, target?.version],
    queryFn: () =>
      target
        ? client.request(ServiceStatsQuery, {
            namespace: target.namespace,
            version: target.version,
          })
        : Promise.resolve(null),
    enabled: target !== null,
    refetchInterval: 5_000,
  });

  const methods = (data?.admin?.serviceStats?.methods ?? []).flatMap((m, i) =>
    m ? [{ ...m, id: `${i}:${m.method}:${m.caller}` }] : [],
  );
  type MethodRow = (typeof methods)[number];
  const columns: Column<MethodRow>[] = [
    { id: 'method', header: 'Method', cell: (m) => m.method },
    { id: 'caller', header: 'Caller', cell: (m) => m.caller },
    {
      id: 'count',
      header: 'Count',
      align: 'right',
      cell: (m) => formatCount(Number(m.count)),
    },
    {
      id: 'p50',
      header: <>p50&nbsp;ms</>,
      align: 'right',
      cell: (m) => Number(m.p50Millis),
    },
    {
      id: 'p95',
      header: <>p95&nbsp;ms</>,
      align: 'right',
      cell: (m) => Number(m.p95Millis),
    },
  ];

  return (
    <DetailPanel
      open={target !== null}
      onClose={onClose}
      title={`${target?.namespace ?? ''} ${target?.version ?? ''}`}
      width={540}
    >
      <Stack spacing={2}>
        {target && (
          <KeyValue
            labelWidth={60}
            items={[{ label: 'hash', value: target.hashHex, mono: true }]}
          />
        )}
        <Panel title="Per-method & caller (24h)" flush={methods.length > 0}>
          {isLoading && <Typography>Loading…</Typography>}
          {error && <Typography color="error">{error.message}</Typography>}
          {!isLoading && !error && methods.length === 0 && (
            <EmptyState
              compact
              title="No traffic recorded"
              description="No traffic recorded for this service in the last 24h."
            />
          )}
          {methods.length > 0 && (
            <DataTable
              columns={columns}
              rows={methods}
              getRowId={(m) => m.id}
            />
          )}
        </Panel>
      </Stack>
    </DetailPanel>
  );
}
