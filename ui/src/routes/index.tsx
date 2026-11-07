import { createFileRoute } from '@tanstack/react-router';
import { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  FormControl,
  FormControlLabel,
  InputLabel,
  MenuItem,
  Select,
  Stack,
  Switch,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from '@mui/material';
import { useTheme } from '@mui/material/styles';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Background,
  Controls,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Node,
} from '@xyflow/react';
import AddIcon from '@mui/icons-material/Add';
import '@xyflow/react/dist/style.css';
import { client } from '@/api/client';
import { DependencyGraphQuery } from '@/api/operations';
import { EmptyState, EnvBadge, PageHeader } from '@/ui';
import { useEvents } from '@/providers/EventsProvider';
import { useAdminToken } from '@/components/SettingsDrawer';
import {
  buildFlow,
  externalId,
  withoutGatewayLines,
  type EdgeView,
  type ExternalData,
  type ExternalDependent,
  type Graph,
  type LineData,
} from '@/components/graph/model';
import { nodeTypes } from '@/components/graph/nodes';
import { NodeDetail } from '@/components/graph/NodeDetail';
import { ExternalDetail } from '@/components/graph/ExternalDetail';
import { ExternalDependentDialog } from '@/components/graph/ExternalDependentDialog';
import { notificationsQueryKey } from '@/components/Notifications';

// ?select=line:ns/ver or ?select=ext:id opens that node's detail panel
// (notification items link here).
export const Route = createFileRoute('/')({
  validateSearch: (search: Record<string, unknown>): { select?: string } =>
    typeof search.select === 'string' && search.select !== '' ? { select: search.select } : {},
  component: GraphPage,
});

// Main screen: the live dependency graph. One node per (service,
// line), grouped by service; desired edges (declared depends) solid,
// active edges (observed traffic) dashed with call counts. Declared
// external dependents (consumers the gateway cannot see) are nodes on
// the left with desired edges to the lines they pin. Data is the
// admin dependencyGraph op, refetched on every service change event
// (debounced) and polled every 5s for traffic.

type Window = '1m' | '1h' | '24h';
const POLL_MS = 5_000;
const EVENT_DEBOUNCE_MS = 300;

function GraphPage() {
  const [windowVal, setWindow] = useState<Window>('1h');
  const [view, setView] = useState<EdgeView>('both');
  // Flow node id of the selection: "line:ns/ver" or "ext:id".
  const { select } = Route.useSearch();
  const navigate = Route.useNavigate();
  // ?select wins while present; a local selection drops it, so
  // following the same link again re-opens it.
  const [localSelected, setLocalSelected] = useState<string | null>(null);
  const selected = select ?? localSelected;
  const setSelected = (id: string | null) => {
    setLocalSelected(id);
    if (select) void navigate({ search: {}, replace: true });
  };
  // Add / edit dialog: undefined = closed, null = add.
  const [editing, setEditing] = useState<ExternalDependent | null | undefined>(undefined);
  const [showGateway, setShowGateway] = useState(false);
  const token = useAdminToken();
  const queryClient = useQueryClient();
  const queryKey = ['dependencyGraph', windowVal];
  const { data, isLoading, error } = useQuery({
    queryKey,
    queryFn: () => client.request(DependencyGraphQuery, { window: windowVal }),
    refetchInterval: POLL_MS,
  });

  const { serviceChangeSeq } = useEvents();
  useEffect(() => {
    if (serviceChangeSeq === 0) return;
    const t = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: ['dependencyGraph'] });
    }, EVENT_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [serviceChangeSeq, queryClient]);

  const raw = data?.admin?.dependencyGraph ?? null;
  const graph = useMemo(() => (raw && !showGateway ? withoutGatewayLines(raw) : raw), [raw, showGateway]);
  const line = graph?.nodes?.find((n) => `line:${n.id}` === selected) ?? null;
  const ext = graph?.externalDependents?.find((x) => externalId(x.id) === selected) ?? null;
  const refetch = () => {
    void queryClient.invalidateQueries({ queryKey: ['dependencyGraph'] });
    void queryClient.invalidateQueries({ queryKey: notificationsQueryKey });
  };

  return (
    <Box>
      <PageHeader
        title="Dependency graph"
        meta={graph && <EnvBadge name={graph.environment || undefined} />}
        subtitle={
          <>
            Declared dependencies (solid) and observed calls (dashed). Traffic counts are from this gateway node
            only.
          </>
        }
        actions={
          <Stack direction="row" spacing={1.5} sx={{ alignItems: 'center' }}>
            <Button
              size="small"
              startIcon={<AddIcon />}
              disabled={token === null}
              title={token === null ? 'Sign in with the admin token to add one' : undefined}
              onClick={() => setEditing(null)}
            >
              External dependent
            </Button>
            <FormControlLabel
              control={<Switch size="small" checked={showGateway} onChange={(e) => setShowGateway(e.target.checked)} />}
              label="Gateway services"
            />
            <ToggleButtonGroup
              size="small"
              exclusive
              value={view}
              onChange={(_, v: EdgeView | null) => v && setView(v)}
              aria-label="edges"
            >
              <ToggleButton value="desired">Desired</ToggleButton>
              <ToggleButton value="active">Active</ToggleButton>
              <ToggleButton value="both">Both</ToggleButton>
            </ToggleButtonGroup>
            <FormControl size="small" sx={{ minWidth: 140 }}>
              <InputLabel id="graph-window">Window</InputLabel>
              <Select
                labelId="graph-window"
                label="Window"
                value={windowVal}
                onChange={(e) => setWindow(e.target.value)}
              >
                <MenuItem value="1m">Last minute</MenuItem>
                <MenuItem value="1h">Last hour</MenuItem>
                <MenuItem value="24h">Last 24 hours</MenuItem>
              </Select>
            </FormControl>
          </Stack>
        }
      />
      {graph && graph.callerSource !== 'extractor' && view !== 'desired' && (
        <Alert severity="info" sx={{ mb: 2 }}>
          {graph.callerSource === 'header'
            ? 'Callers on active edges come from a request header. Any client can set it, so treat caller names as claims.'
            : 'No caller-id source is configured, so every call is from "unknown". Configure WithCallerIDExtractor or WithCallerHeaders to see who calls what.'}
        </Alert>
      )}
      {isLoading && <Typography>Loading…</Typography>}
      {error && <Alert severity="error">{error.message}</Alert>}
      {graph && (graph.nodes ?? []).length === 0 && (graph.externalDependents ?? []).length === 0 && (
        <EmptyState
          title="No services registered"
          description="The graph fills in as services register against the gateway."
        />
      )}
      {graph && ((graph.nodes ?? []).length > 0 || (graph.externalDependents ?? []).length > 0) && (
        <ReactFlowProvider>
          <Flow graph={graph} view={view} selected={selected} onSelect={setSelected} />
        </ReactFlowProvider>
      )}
      {graph && (
        <NodeDetail
          graph={graph}
          line={line}
          canDrain={token !== null}
          onClose={() => setSelected(null)}
          onChanged={refetch}
        />
      )}
      {graph && (
        <ExternalDetail
          graph={graph}
          ext={ext}
          canEdit={token !== null}
          onEdit={(x) => setEditing(x)}
          onClose={() => setSelected(null)}
          onChanged={refetch}
        />
      )}
      {editing !== undefined && (
        <ExternalDependentDialog
          initial={editing}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined);
            refetch();
          }}
        />
      )}
    </Box>
  );
}

function Flow({
  graph,
  view,
  selected,
  onSelect,
}: {
  graph: Graph;
  view: EdgeView;
  selected: string | null;
  onSelect: (id: string | null) => void;
}) {
  const theme = useTheme();
  const tokens = theme.gwag.tokens;
  const colors = useMemo(
    () => ({
      met: theme.gwag.status.ready.fg,
      unmet: theme.palette.error.main,
      active: theme.palette.primary.main,
      muted: tokens.borderStrong,
      labelBg: tokens.surfaceRaised,
      labelText: tokens.text,
    }),
    [theme, tokens],
  );
  const { nodes, edges } = useMemo(() => {
    const flow = buildFlow(graph, view, colors);
    return { nodes: flow.nodes.map((n) => (n.id === selected ? { ...n, selected: true } : n)), edges: flow.edges };
  }, [graph, view, colors, selected]);

  // Refit when the set of nodes changes (a service joined or left),
  // not on every poll.
  const { fitView } = useReactFlow();
  const shape = nodes.map((n) => n.id).join('|');
  useEffect(() => {
    const t = setTimeout(() => void fitView({ padding: 0.15, duration: 200 }), 0);
    return () => clearTimeout(t);
  }, [shape, fitView]);

  return (
    <Box
      sx={{
        height: 'calc(100vh - 320px)',
        minHeight: 480,
        border: 1,
        borderColor: 'divider',
        borderRadius: `${theme.gwag.radius.md}px`,
        overflow: 'hidden',
        bgcolor: tokens.bg,
        '& .react-flow': { '--xy-background-color': tokens.bg },
      }}
    >
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        colorMode={theme.palette.mode}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        fitView
        minZoom={0.2}
        onNodeClick={(_, n: Node) => {
          if (n.type === 'line') {
            const l = (n.data as LineData).line;
            onSelect(`line:${l.id}`);
          } else if (n.type === 'external') {
            onSelect(externalId((n.data as ExternalData).ext.id));
          }
        }}
        onPaneClick={() => onSelect(null)}
      >
        <Background color={tokens.border} gap={24} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </Box>
  );
}
