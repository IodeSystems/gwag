import { useState } from 'react';
import { Button, Stack, Tooltip, Typography } from '@mui/material';
import { ConfirmDialog, DetailPanel, FormDialog, FormRow, FormTextField, KeyValue, Panel, StatusChip, useToast } from '@/ui';
import { client } from '@/api/client';
import { DeleteExternalDependentMutation, ReviewExternalDependentMutation } from '@/api/operations';
import type { ExternalDependent, Graph } from './model';
import { defaultReviewDate, toReviewAt } from './reviewDate';

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
  const [reviewing, setReviewing] = useState(false);
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
  const authTip = canEdit ? '' : 'Sign in with the admin token to edit';

  return (
    <>
      <DetailPanel
        open
        onClose={onClose}
        width={560}
        title={ext.name || ext.id}
        subtitle={`${ext.kind} · external dependent · id ${ext.id}`}
        meta={ext.reviewDue ? <StatusChip size="small" status="waiting" label="review due" /> : undefined}
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
                <Button disabled={!canEdit} onClick={() => onEdit(ext)}>
                  Edit
                </Button>
              </span>
            </Tooltip>
            <Tooltip title={authTip}>
              <span>
                <Button variant="contained" disabled={!canEdit} onClick={() => setReviewing(true)}>
                  Mark reviewed
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
                label: 'Review by',
                value: ext.reviewAt
                  ? `${ext.reviewAt}${ext.reviewDue ? ' (overdue: still counts as a dependent)' : ''}`
                  : 'not set (review due)',
              },
              {
                label: 'Last reviewed',
                value: ext.reviewedAt ? `${ext.reviewedAt}${ext.reviewedBy ? ` by ${ext.reviewedBy}` : ''}` : 'never',
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
            A pinned line is not sunset-able unless another pinned line of the same service has a ready instance, and
            the gateway blocks a drain that would leave no ready pinned line. This holds past the review date too: the
            gateway takes no action on it. To review, confirm the pins with the owner, then mark it reviewed with the
            next review date.
          </Typography>
        </Stack>
      </DetailPanel>
      <FormDialog<{ reviewAt: string; by: string }>
        open={reviewing}
        title={`Mark ${ext.name || ext.id} reviewed`}
        description="Sets the next review date and records who reviewed it and when. The pins and other fields stay as they are."
        submitLabel="Mark reviewed"
        defaultValues={{ reviewAt: defaultReviewDate(), by: '' }}
        onClose={() => setReviewing(false)}
        onSubmit={async (v) => {
          const res = await client.request(ReviewExternalDependentMutation, {
            id: ext.id,
            body: { reviewAt: toReviewAt(v.reviewAt), by: v.by.trim() || null },
          });
          const warnings = res.admin?.reviewExternalDependent?.warnings ?? [];
          if (warnings.length > 0) toast.warning(`Reviewed ${ext.id}. ${warnings.join('; ')}`);
          else toast.success(`Reviewed ${ext.id}`);
          setReviewing(false);
          onChanged();
        }}
      >
        <FormRow>
          <FormTextField
            name="reviewAt"
            label="Next review by"
            helperText="YYYY-MM-DD (midnight UTC) or an RFC 3339 time. Must be in the future."
            required
            autoFocus
            monospace
          />
          <FormTextField name="by" label="Reviewed by" placeholder="you@example.com" helperText="Recorded as given; not checked." />
        </FormRow>
      </FormDialog>
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
