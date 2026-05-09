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
  Paper,
  Snackbar,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
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
// per-namespace stable target.
function vNumber(version: string): number | null {
  const m = /^v([1-9][0-9]*)$/.exec(version);
  return m ? Number(m[1]) : null;
}

function Services() {
  const qc = useQueryClient();
  const [toast, setToast] = useState<string | null>(null);
  const [retractTarget, setRetractTarget] = useState<{
    namespace: string;
    currentVN: number;
    candidateVNs: number[];
  } | null>(null);

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

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  const services = data?.admin?.listServices?.services ?? [];
  const stableEntries = data?.admin?.listServices?.stableVN ?? [];
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

  // Group registered vNs per namespace so we can hand the retract dialog
  // a candidate list. Rows are ordered as the API returned them, but
  // we keep deterministic per-namespace sort below.
  const vNsByNamespace = new Map<string, number[]>();
  for (const s of services) {
    if (!s) continue;
    const n = vNumber(s.version);
    if (n === null) continue;
    const arr = vNsByNamespace.get(s.namespace) ?? [];
    arr.push(n);
    vNsByNamespace.set(s.namespace, arr);
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
              <TableCell>Hash</TableCell>
              <TableCell align="right">Replicas</TableCell>
              <TableCell align="right">Action</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {services.map((s, i) => {
              if (!s) return null;
              const tier = tierOf(s.version);
              const stableTargetVN = stableByNamespace.get(s.namespace);
              const isStableTarget =
                stableTargetVN !== undefined && vNumber(s.version) === stableTargetVN;
              return (
                <TableRow key={i}>
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
                    <code style={{ fontSize: '0.75rem' }}>
                      {s.hashHex?.slice(0, 16)}…
                    </code>
                  </TableCell>
                  <TableCell align="right">{s.replicaCount}</TableCell>
                  <TableCell align="right">
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
            Currently <code>{retractTarget?.namespace}.stable</code> aliases
            v{retractTarget?.currentVN}. Retracting moves the alias to a lower
            registered vN — operator-driven only; the gateway never rolls
            stable back automatically. Pick the target:
          </DialogContentText>
          {retractTarget?.candidateVNs.length === 0 ? (
            <Typography color="text.secondary">
              No earlier registered vN to retract to. Ensure at least one
              prior vN is currently registered before retracting.
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
          <Button onClick={() => setRetractTarget(null)} disabled={retract.isPending}>
            Cancel
          </Button>
        </DialogActions>
      </Dialog>

      <Snackbar
        open={toast !== null}
        autoHideDuration={3000}
        onClose={() => setToast(null)}
        message={toast ?? ''}
      />
    </>
  );
}
