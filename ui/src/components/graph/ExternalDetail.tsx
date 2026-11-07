import { useState } from 'react';
import { Button, Stack, Tooltip, Typography } from '@mui/material';
import { ConfirmDialog, DetailPanel, KeyValue, Panel, StatusChip, useToast } from '@/ui';
import { client } from '@/api/client';
import { DeleteExternalDependentMutation } from '@/api/operations';
import type { ExternalDependent, Graph } from './model';

export interface ExternalDetailProps {
  graph: Graph;
  ext: ExternalDependent | null;
  canEdit: boolean;
  onEdit: (x: ExternalDependent) => void;
  onClose: () => void;
  onChanged: () => void;
}

/** Detail panel for a declared external dependent. */
export function ExternalDetail({ graph, ext, canEdit, onEdit, onClose, onChanged }: ExternalDetailProps) {
  const toast = useToast();
  const [confirm, setConfirm] = useState(false);
  if (!ext) return null;

  const edges = (graph.desiredEdges ?? []).filter(
    (e) => e.consumerKind === 'external' && e.consumerNamespace === ext.id,
  );
  const remove = async () => {
    await client.request(DeleteExternalDependentMutation, { id: ext.id });
    toast.success(`Deleted ${ext.id}`);
    onClose();
    onChanged();
  };
  const authTip = canEdit ? '' : 'Sign in with the dev or admin token to edit';

  return (
    <>
      <DetailPanel
        open
        onClose={onClose}
        width={560}
        title={ext.name || ext.id}
        subtitle={`${ext.kind} · external dependent · id ${ext.id}`}
        meta={ext.expired ? <StatusChip size="small" status="degraded" label="expired" /> : undefined}
        actions={
          <>
            <Tooltip title={authTip}>
              <span>
                <Button color="error" disabled={!canEdit} onClick={() => setConfirm(true)}>
                  Delete
                </Button>
              </span>
            </Tooltip>
            <Tooltip title={authTip}>
              <span>
                <Button variant="contained" disabled={!canEdit} onClick={() => onEdit(ext)}>
                  Edit
                </Button>
              </span>
            </Tooltip>
          </>
        }
      >
        <Stack spacing={2}>
          <KeyValue
            labelWidth={120}
            items={[
              { label: 'Owner', value: ext.owner?.team || '—' },
              { label: 'Contact', value: ext.owner?.contact || '—' },
              {
                label: 'Expires',
                value: ext.expiresAt
                  ? `${ext.expiresAt}${ext.expired ? ' (expired: no longer counts as a dependent)' : ''}`
                  : 'never',
              },
              { label: 'Updated', value: new Date(Number(ext.updatedUnixMs)).toISOString() },
              { label: 'Note', value: ext.note || '—' },
            ]}
          />
          <Panel title="Pinned lines">
            <Stack spacing={1}>
              {(ext.depends ?? []).map((d) => {
                const e = edges.find((x) => x.provider === d.service);
                return (
                  <Stack key={d.service} direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                    {e ? (
                      <StatusChip size="small" status={e.met ? 'ready' : 'waiting'} label={e.met ? 'met' : 'unmet'} />
                    ) : (
                      <StatusChip size="small" status="degraded" label="not counted" />
                    )}
                    <Typography variant="body2" sx={{ fontFamily: (t) => t.gwag.mono }}>
                      {d.service} [{(d.lines ?? []).join(', ')}]
                    </Typography>
                    {e && (
                      <Typography variant="caption" color="text.secondary">
                        {e.met ? `present: ${(e.presentLines ?? []).join(', ')}` : 'no pinned line is ready'}
                      </Typography>
                    )}
                  </Stack>
                );
              })}
            </Stack>
          </Panel>
          <Typography variant="body2" color="text.secondary">
            While it has not expired, a pinned line is not sunset-able unless another pinned line of the same service
            has a ready instance, and the gateway blocks a drain that would leave no ready pinned line.
          </Typography>
        </Stack>
      </DetailPanel>
      <ConfirmDialog
        open={confirm}
        title={`Delete ${ext.name || ext.id}?`}
        message="Its pins stop counting at once: lines only it pins can become sunset-able, and drains it blocks proceed."
        confirmLabel="Delete"
        destructive
        onConfirm={remove}
        onClose={() => setConfirm(false)}
      />
    </>
  );
}
