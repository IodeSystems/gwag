import { Box, Stack, Typography } from '@mui/material';
import PublicIcon from '@mui/icons-material/Public';
import { Handle, Position, type Node, type NodeProps, type NodeTypes } from '@xyflow/react';
import { StatusChip } from '@/ui';
import { formatCount, type CallerData, type GroupData, type LineData } from './model';

// Handles are invisible: edges attach left (in) and right (out).
const hidden = { opacity: 0, width: 1, height: 1, minWidth: 0, minHeight: 0, border: 0 };

function Handles() {
  return (
    <>
      <Handle type="target" position={Position.Left} style={hidden} isConnectable={false} />
      <Handle type="source" position={Position.Right} style={hidden} isConnectable={false} />
    </>
  );
}

function GroupNode({ data }: NodeProps<Node<GroupData>>) {
  return (
    <Box
      sx={(t) => ({
        width: '100%',
        height: '100%',
        borderRadius: `${t.gwag.radius.md}px`,
        border: `1px ${data.missing ? 'dashed' : 'solid'} ${t.gwag.tokens.borderStrong}`,
        bgcolor: t.gwag.tokens.surfaceSunken,
      })}
    >
      <Handles />
      <Stack direction="row" sx={{ px: 1.5, height: 44, alignItems: 'center', gap: 1 }}>
        <Typography variant="h4" noWrap sx={{ flexGrow: 1, minWidth: 0 }}>
          {data.namespace}
        </Typography>
        {data.team && (
          <Typography variant="caption" color="text.secondary" noWrap>
            {data.team}
          </Typography>
        )}
      </Stack>
      {data.missing && (
        <Typography variant="caption" color="error" sx={{ px: 1.5 }}>
          not registered
        </Typography>
      )}
    </Box>
  );
}

function LineNode({ data, selected }: NodeProps<Node<LineData>>) {
  const l = data.line;
  const ready = Number(l.readyCount);
  const waiting = Number(l.waitingCount);
  const degraded = Number(l.degradedCount);
  const draining = Number(l.drainingCount);
  const traffic = Number(l.trafficCount);
  // Boot-time registrations have replicas but no listed instances.
  const bootReady = ready + waiting + degraded === 0 ? l.replicaCount : 0;
  return (
    <Box
      sx={(t) => ({
        width: '100%',
        height: '100%',
        px: 1.25,
        py: 1,
        cursor: 'pointer',
        borderRadius: `${t.gwag.radius.sm}px`,
        border: `1px solid ${selected ? t.palette.primary.main : t.gwag.tokens.border}`,
        boxShadow: selected ? `0 0 0 1px ${t.palette.primary.main}` : t.gwag.tokens.shadow.sm,
        bgcolor: t.gwag.tokens.surface,
        display: 'flex',
        flexDirection: 'column',
        gap: 0.75,
        overflow: 'hidden',
      })}
    >
      <Handles />
      <Stack direction="row" sx={{ alignItems: 'baseline', gap: 1 }}>
        <Typography variant="h5" sx={{ fontFamily: (t) => t.gwag.mono }}>
          {l.version}
        </Typography>
        <Typography variant="caption" color="text.secondary" noWrap sx={{ flexGrow: 1 }}>
          {l.kind || 'not in pool'}
        </Typography>
        <Typography variant="caption" color="text.secondary" noWrap>
          {traffic > 0 ? `${formatCount(traffic)} calls` : 'no traffic'}
        </Typography>
      </Stack>
      <Stack direction="row" sx={{ gap: 0.5, flexWrap: 'wrap' }}>
        {ready + bootReady > 0 && <StatusChip size="small" status="ready" label={`${ready + bootReady} ready`} />}
        {waiting > 0 && <StatusChip size="small" status="waiting" label={`${waiting} waiting`} />}
        {degraded > 0 && <StatusChip size="small" status="degraded" label={`${degraded} degraded`} />}
        {draining > 0 && <StatusChip size="small" status="draining" label={`${draining} draining`} />}
        {l.sunsetable && <StatusChip size="small" status="sunsetable" />}
      </Stack>
      <Typography variant="caption" color="text.secondary" noWrap>
        {l.owner?.team ? `owner ${l.owner.team}` : 'no owner declared'}
      </Typography>
    </Box>
  );
}

function CallerNode({ data }: NodeProps<Node<CallerData>>) {
  return (
    <Box
      sx={(t) => ({
        width: '100%',
        height: '100%',
        px: 1.25,
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        borderRadius: `${t.gwag.radius.pill}px`,
        border: `1px dashed ${t.gwag.tokens.borderStrong}`,
        bgcolor: t.gwag.tokens.surface,
      })}
    >
      <Handles />
      <PublicIcon fontSize="small" color="disabled" />
      <Box sx={{ minWidth: 0 }}>
        <Typography variant="body2" noWrap sx={{ fontFamily: (t) => t.gwag.mono }}>
          {data.caller}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          external · {formatCount(data.count)} calls
        </Typography>
      </Box>
    </Box>
  );
}

export const nodeTypes: NodeTypes = {
  nsGroup: GroupNode,
  line: LineNode,
  caller: CallerNode,
};
