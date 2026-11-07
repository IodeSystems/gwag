import { useState } from 'react';
import { Button, Stack, Tooltip, Typography } from '@mui/material';
import {
  ConfirmDialog,
  DataTable,
  DetailPanel,
  KeyValue,
  Panel,
  StatusChip,
  useToast,
  type Column,
} from '@/ui';
import { client } from '@/api/client';
import { DrainInstanceMutation } from '@/api/operations';
import { formatCount, type Graph, type GraphLine } from './model';

type Instance = NonNullable<GraphLine['instances']>[number];

const drainLabel: Record<string, string> = {
  draining: 'Draining',
  blocked: 'Blocked',
  safe_to_stop: 'Safe to stop',
};

export interface NodeDetailProps {
  graph: Graph;
  line: GraphLine | null;
  canDrain: boolean;
  onClose: () => void;
  /** Called after a drain request, to refetch. */
  onChanged: () => void;
}

export function NodeDetail({ graph, line, canDrain, onClose, onChanged }: NodeDetailProps) {
  const toast = useToast();
  const [target, setTarget] = useState<Instance | null>(null);
  if (!line) return null;

  const instances = line.instances ?? [];
  const callers = (graph.activeEdges ?? [])
    .filter((e) => e.namespace === line.namespace && e.version === line.version)
    .flatMap((e) =>
      (e.methods ?? []).map((m) => ({
        key: `${e.caller}|${m.method}`,
        caller: e.caller,
        mapped: e.consumerNamespace,
        method: m.method,
        count: Number(m.count),
        errors: Number(m.count) - Number(m.okCount),
      })),
    );
  const dependents = (graph.desiredEdges ?? []).filter(
    (e) => e.provider === line.namespace && (e.lines ?? []).includes(line.version),
  );
  const depends = (graph.desiredEdges ?? []).filter(
    (e) => e.consumerNamespace === line.namespace && e.consumerVersion === line.version,
  );

  const instanceCols: Column<Instance>[] = [
    { id: 'state', header: 'State', cell: (i) => <StatusChip size="small" status={i.state} />, sortValue: (i) => i.state },
    { id: 'addr', header: 'Address', mono: true, sortValue: (i) => i.addr },
    { id: 'id', header: 'Instance', mono: true, sortValue: (i) => i.instanceId || i.registrationId },
    {
      id: 'drain',
      header: 'Drain',
      sortValue: (i) => i.drainState,
      cell: (i) =>
        i.drainState === 'serving' ? (
          <Typography variant="caption" color="text.secondary">
            serving
          </Typography>
        ) : (
          <Tooltip title={(i.drainReasons ?? []).join('; ')}>
            <span>
              <StatusChip size="small" status="draining" label={drainLabel[i.drainState] ?? i.drainState} />
            </span>
          </Tooltip>
        ),
    },
    {
      id: 'action',
      header: '',
      align: 'right',
      cell: (i) => (
        <Tooltip title={canDrain ? '' : 'Sign in with the admin token to drain'}>
          <span>
            <Button
              size="small"
              disabled={!canDrain || i.drainState !== 'serving'}
              onClick={() => setTarget(i)}
            >
              Drain
            </Button>
          </span>
        </Tooltip>
      ),
    },
  ];

  const drain = async (i: Instance) => {
    const res = await client.request(DrainInstanceMutation, {
      instance: i.registrationId,
      cancel: null,
    });
    const st = res.admin?.drainInstance;
    const reasons = st?.reasons ?? [];
    if (st?.state === 'blocked') toast.warning(`Drain blocked: ${reasons.join('; ')}`);
    else toast.success(`Drain requested: ${i.instanceId || i.registrationId} (${st?.state ?? 'unknown'})`);
    onChanged();
  };

  return (
    <>
      <DetailPanel
        open
        onClose={onClose}
        width={640}
        title={`${line.namespace} ${line.version}`}
        subtitle={line.kind || 'not in pool yet'}
        meta={line.sunsetable ? <StatusChip size="small" status="sunsetable" /> : undefined}
      >
        <Stack spacing={2}>
          <KeyValue
            labelWidth={120}
            items={[
              { label: 'Owner', value: line.owner?.team || '—' },
              { label: 'Contact', value: line.owner?.contact || '—' },
              { label: 'Replicas', value: String(line.replicaCount) },
              {
                label: 'Traffic',
                value: `${formatCount(Number(line.trafficCount))} calls in ${graph.window} (this node)`,
              },
              { label: 'Sunset', value: line.sunsetReason },
            ]}
          />
          <Panel title="Instances" flush>
            <DataTable
              columns={instanceCols}
              rows={instances}
              getRowId={(i) => i.registrationId}
              emptyTitle="No registered instances"
              emptyDescription="Boot-time registrations (AddProto etc.) are not listed."
            />
          </Panel>
          <Panel title="Active callers" subtitle={`by method, ${graph.window}, this node`} flush>
            <DataTable
              columns={[
                {
                  id: 'caller',
                  header: 'Caller',
                  mono: true,
                  sortValue: (r) => r.caller,
                  cell: (r) => (r.mapped ? r.caller : `${r.caller} (external)`),
                },
                { id: 'method', header: 'Method', mono: true, sortValue: (r) => r.method },
                { id: 'count', header: 'Calls', align: 'right', sortValue: (r) => r.count, cell: (r) => formatCount(r.count) },
                { id: 'errors', header: 'Errors', align: 'right', sortValue: (r) => r.errors },
              ]}
              rows={callers}
              getRowId={(r) => r.key}
              defaultSort={{ columnId: 'count', direction: 'desc' }}
              emptyTitle="No traffic in the window"
            />
          </Panel>
          <Panel title="Declared dependencies">
            <EdgeList
              empty="None declared."
              items={depends.map((e) => ({
                key: `${e.provider}|${(e.lines ?? []).join(',')}`,
                text: `${e.provider} [${(e.lines ?? []).join(', ')}]`,
                met: e.met,
                note: e.met ? `present: ${(e.presentLines ?? []).join(', ')}` : 'no accepted line is ready',
              }))}
            />
          </Panel>
          <Panel title="Dependents">
            <EdgeList
              empty="No dependent accepts this line."
              items={dependents.map((e) => ({
                key: `${e.consumerNamespace}/${e.consumerVersion}|${(e.lines ?? []).join(',')}`,
                text: `${e.consumerNamespace} ${e.consumerVersion} accepts [${(e.lines ?? []).join(', ')}]`,
                met: e.met,
                note: '',
              }))}
            />
          </Panel>
        </Stack>
      </DetailPanel>
      <ConfirmDialog
        open={target !== null}
        title="Drain instance?"
        message={
          target && (
            <>
              Stop routing new requests to <b>{target.instanceId || target.registrationId}</b> ({target.addr}) on
              every node. The gateway holds the drain as blocked while a dependent would have no other ready line.
            </>
          )
        }
        confirmLabel="Drain"
        destructive
        onConfirm={() => (target ? drain(target) : undefined)}
        onClose={() => setTarget(null)}
      />
    </>
  );
}

function EdgeList({
  items,
  empty,
}: {
  items: { key: string; text: string; met: boolean; note: string }[];
  empty: string;
}) {
  if (items.length === 0) {
    return (
      <Typography variant="body2" color="text.secondary">
        {empty}
      </Typography>
    );
  }
  return (
    <Stack spacing={1}>
      {items.map((i) => (
        <Stack key={i.key} direction="row" spacing={1} sx={{ alignItems: 'center' }}>
          <StatusChip size="small" status={i.met ? 'ready' : 'waiting'} label={i.met ? 'met' : 'unmet'} />
          <Typography variant="body2" sx={{ fontFamily: (t) => t.gwag.mono }}>
            {i.text}
          </Typography>
          {i.note && (
            <Typography variant="caption" color="text.secondary">
              {i.note}
            </Typography>
          )}
        </Stack>
      ))}
    </Stack>
  );
}
