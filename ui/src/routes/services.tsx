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
  Drawer,
  IconButton,
  Paper,
  Snackbar,
  Stack,
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
import CloseIcon from '@mui/icons-material/Close';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { sdk } from '@/api/client';

export const Route = createFileRoute('/services')({
  component: Services,
});

// tierOf maps a §4 version string to its tier label.
//   "unstable" → "unstable"
//   "v<N>"     → "vN"  (the literal numbered cut tier)
// Unknown shapes — shouldn't appear given parseVersion canonicalisation —
// fall through to the raw string so operators see something rather than
// silently mislabelling.
function tierOf(version: string): 'unstable' | 'vN' | string {
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

function Services() {
  const qc = useQueryClient();
  const [toast, setToast] = useState<string | null>(null);
  const [retractTarget, setRetractTarget] = useState<{
    namespace: string;
    currentVN: number;
    candidateVNs: number[];
  } | null>(null);
  const [deprecateTarget, setDeprecateTarget] =
    useState<DeprecateTarget | null>(null);
  const [drawerTarget, setDrawerTarget] = useState<DrawerTarget | null>(null);
  const [reasonDraft, setReasonDraft] = useState('');

  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => sdk.Services(),
    refetchInterval: 10_000,
  });

  const retract = useMutation({
    mutationFn: (vars: { namespace: string; targetVN: number }) =>
      sdk.RetractStable(vars),
    onSuccess: (resp) => {
      qc.invalidateQueries({ queryKey: ['services'] });
      const r = resp.admin.retractStable;
      setToast(`stable v${r?.priorVN} → v${r?.newVN}`);
      setRetractTarget(null);
    },
    onError: (e: Error) => setToast(e.message),
  });

  const deprecate = useMutation({
    mutationFn: (vars: {
      namespace: string;
      version: string;
      reason: string;
    }) => sdk.Deprecate(vars),
    onSuccess: (_resp, vars) => {
      qc.invalidateQueries({ queryKey: ['services'] });
      setToast(`deprecated ${vars.namespace} ${vars.version}`);
      setDeprecateTarget(null);
      setReasonDraft('');
    },
    onError: (e: Error) => setToast(e.message),
  });

  const undeprecate = useMutation({
    mutationFn: (vars: { namespace: string; version: string }) =>
      sdk.Undeprecate(vars),
    onSuccess: (_resp, vars) => {
      qc.invalidateQueries({ queryKey: ['services'] });
      setToast(`undeprecated ${vars.namespace} ${vars.version}`);
    },
    onError: (e: Error) => setToast(e.message),
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error)
    return <Typography color="error">{(error as Error).message}</Typography>;

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
  const statsByKey = new Map<
    string,
    {
      count: number;
      throughput: number;
      p50Millis: number;
      p95Millis: number;
    }
  >();
  for (const r of statsRows) {
    if (!r) continue;
    statsByKey.set(`${r.namespace}@${r.version}`, {
      count: Number(r.count),
      throughput: r.throughput,
      p50Millis: Number(r.p50Millis),
      p95Millis: Number(r.p95Millis),
    });
  }

  return (
    <>
      <TableContainer component={Paper}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Namespace</TableCell>
              <TableCell>Version</TableCell>
              <TableCell>Tier</TableCell>
              <TableCell>Status</TableCell>
              <TableCell align="right">Replicas</TableCell>
              <TableCell align="right">Calls&nbsp;/&nbsp;24h</TableCell>
              <TableCell align="right">p50&nbsp;ms</TableCell>
              <TableCell align="right">p95&nbsp;ms</TableCell>
              <TableCell align="right">Calls&nbsp;/&nbsp;s</TableCell>
              <TableCell align="right">Actions</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {services.map((s, i) => {
              if (!s) return null;
              const tier = tierOf(s.version);
              const stableTargetVN = stableByNamespace.get(s.namespace);
              const isStableTarget =
                stableTargetVN !== undefined &&
                vNumber(s.version) === stableTargetVN;
              const vN = vNumber(s.version);
              const maxVN = maxVNByNamespace.get(s.namespace);
              const isAutoDeprecated =
                vN !== null && maxVN !== undefined && vN < maxVN;
              const manualReason = s.manualDeprecationReason ?? '';
              const isManualDeprecated = manualReason.length > 0;
              const isDeprecated = isAutoDeprecated || isManualDeprecated;
              const stats = statsByKey.get(`${s.namespace}@${s.version}`);
              return (
                <TableRow
                  key={i}
                  hover
                  sx={{ cursor: 'pointer' }}
                  onClick={() =>
                    setDrawerTarget({
                      namespace: s.namespace,
                      version: s.version,
                      hashHex: s.hashHex,
                    })
                  }
                >
                  <TableCell>{s.namespace}</TableCell>
                  <TableCell>{s.version}</TableCell>
                  <TableCell>
                    <Stack direction="row" spacing={0.5}>
                      <Chip
                        label={tier}
                        size="small"
                        color={tier === 'unstable' ? 'warning' : 'default'}
                      />
                      {isStableTarget && (
                        <Chip label="stable" size="small" color="success" />
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>
                    {isDeprecated && (
                      <Tooltip
                        title={deprecationTooltip({
                          isAutoDeprecated,
                          isManualDeprecated,
                          manualReason,
                        })}
                      >
                        <Chip
                          label={deprecationLabel({
                            isAutoDeprecated,
                            isManualDeprecated,
                          })}
                          size="small"
                          color={isManualDeprecated ? 'error' : 'warning'}
                          variant="outlined"
                        />
                      </Tooltip>
                    )}
                  </TableCell>
                  <TableCell align="right">{s.replicaCount}</TableCell>
                  <TableCell align="right">
                    {stats ? formatCount(stats.count) : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {stats && stats.count > 0 ? stats.p50Millis : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {stats && stats.count > 0 ? stats.p95Millis : '—'}
                  </TableCell>
                  <TableCell align="right">
                    {stats && stats.count > 0
                      ? stats.throughput.toFixed(2)
                      : '—'}
                  </TableCell>
                  <TableCell
                    align="right"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <Box
                      sx={{
                        display: 'flex',
                        gap: 0.5,
                        justifyContent: 'flex-end',
                      }}
                    >
                      {isManualDeprecated ? (
                        <Button
                          size="small"
                          disabled={undeprecate.isPending}
                          onClick={() =>
                            undeprecate.mutate({
                              namespace: s.namespace,
                              version: s.version,
                            })
                          }
                        >
                          Undeprecate
                        </Button>
                      ) : (
                        <Button
                          size="small"
                          color="error"
                          onClick={() => {
                            setReasonDraft('');
                            setDeprecateTarget({
                              namespace: s.namespace,
                              version: s.version,
                              currentReason: manualReason,
                            });
                          }}
                        >
                          Deprecate
                        </Button>
                      )}
                      {isStableTarget && stableTargetVN !== undefined && (
                        <Button
                          size="small"
                          color="warning"
                          onClick={() => {
                            const candidates = (
                              vNsByNamespace.get(s.namespace) ?? []
                            )
                              .filter((n) => n < stableTargetVN)
                              .sort((a, b) => b - a);
                            setRetractTarget({
                              namespace: s.namespace,
                              currentVN: stableTargetVN,
                              candidateVNs: candidates,
                            });
                          }}
                        >
                          Retract Stable
                        </Button>
                      )}
                    </Box>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>

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
            onClick={() => setRetractTarget(null)}
            disabled={retract.isPending}
          >
            Cancel
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog
        open={deprecateTarget !== null}
        onClose={() => {
          if (!deprecate.isPending) setDeprecateTarget(null);
        }}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          Deprecate {deprecateTarget?.namespace} {deprecateTarget?.version}
        </DialogTitle>
        <DialogContent>
          <DialogContentText sx={{ mb: 2 }}>
            Marks this (namespace, version) with{' '}
            <code>@deprecated(reason: …)</code> in SDL. Auto-deprecation of
            older numbered cuts is unaffected; manual reason takes precedence
            in rendering.
          </DialogContentText>
          <TextField
            autoFocus
            fullWidth
            label="Reason"
            placeholder="e.g. moved to v2 (breaks Authn header on /list)"
            value={reasonDraft}
            onChange={(e) => setReasonDraft(e.target.value)}
            disabled={deprecate.isPending}
            multiline
            minRows={2}
          />
        </DialogContent>
        <DialogActions>
          <Button
            onClick={() => setDeprecateTarget(null)}
            disabled={deprecate.isPending}
          >
            Cancel
          </Button>
          <Button
            variant="contained"
            color="error"
            disabled={deprecate.isPending || reasonDraft.trim().length === 0}
            onClick={() => {
              if (!deprecateTarget) return;
              deprecate.mutate({
                namespace: deprecateTarget.namespace,
                version: deprecateTarget.version,
                reason: reasonDraft.trim(),
              });
            }}
          >
            Deprecate
          </Button>
        </DialogActions>
      </Dialog>

      <ServiceDrawer
        target={drawerTarget}
        onClose={() => setDrawerTarget(null)}
      />

      <Snackbar
        open={toast !== null}
        autoHideDuration={3000}
        onClose={() => setToast(null)}
        message={toast ?? ''}
      />
    </>
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
        ? sdk.ServiceStats({
            namespace: target.namespace,
            version: target.version,
          })
        : Promise.resolve(null),
    enabled: target !== null,
    refetchInterval: 5_000,
  });

  const methods = data?.admin?.serviceStats?.methods ?? [];

  return (
    <Drawer
      anchor="right"
      open={target !== null}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: { xs: '100%', sm: 540 } } } }}
    >
      <Box sx={{ p: 2 }}>
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
            mb: 1,
          }}
        >
          <Typography variant="h6">
            {target?.namespace} {target?.version}
          </Typography>
          <IconButton onClick={onClose} size="small">
            <CloseIcon />
          </IconButton>
        </Box>
        {target && (
          <Typography variant="caption" color="text.secondary">
            hash{' '}
            <code style={{ fontSize: '0.75rem' }}>{target.hashHex}</code>
          </Typography>
        )}
        <Typography variant="subtitle2" sx={{ mt: 2, mb: 1 }}>
          Per-method &amp; caller (24h)
        </Typography>
        {isLoading && <Typography>Loading…</Typography>}
        {error && (
          <Typography color="error">{(error as Error).message}</Typography>
        )}
        {!isLoading && !error && methods.length === 0 && (
          <Typography color="text.secondary" variant="body2">
            No traffic recorded for this service in the last 24h.
          </Typography>
        )}
        {methods.length > 0 && (
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
                {methods.map((m, i) => {
                  if (!m) return null;
                  return (
                    <TableRow key={i}>
                      <TableCell>{m.method}</TableCell>
                      <TableCell>{m.caller}</TableCell>
                      <TableCell align="right">
                        {formatCount(Number(m.count))}
                      </TableCell>
                      <TableCell align="right">
                        {Number(m.p50Millis)}
                      </TableCell>
                      <TableCell align="right">
                        {Number(m.p95Millis)}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </TableContainer>
        )}
      </Box>
    </Drawer>
  );
}
