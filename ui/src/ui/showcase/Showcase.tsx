import { useRef, useState } from 'react';
import { flushSync } from 'react-dom';
import {
  Box,
  Button,
  Checkbox,
  Chip,
  Divider,
  FormControlLabel,
  MenuItem,
  Stack,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import RefreshIcon from '@mui/icons-material/Refresh';
import CloudOffIcon from '@mui/icons-material/CloudOff';
import {
  ConfirmDialog,
  CodeBlock,
  DataTable,
  DetailPanel,
  EmptyState,
  EnvBadge,
  FormDialog,
  FormRow,
  FormSelect,
  FormSwitch,
  FormTextField,
  KeyValue,
  Page,
  Panel,
  PresetPicker,
  ScopedTheme,
  StatusChip,
  ThemeToggle,
  ToastProvider,
  statusKeys,
  useThemeMode,
  useToast,
  type Column,
  type Mode,
  type PresetId,
  type StatusKey,
} from '@/ui';
import { PresetGallery } from './Gallery';
import { GraphQLRequestError } from '@/api/client';

interface Svc {
  id: string;
  ns: string;
  version: string;
  kind: 'proto' | 'openapi' | 'graphql' | 'mcp';
  status: StatusKey;
  replicas: number;
  addr: string;
}

const services: Svc[] = [
  { id: '1', ns: 'greeter', version: 'v1', kind: 'proto', status: 'ready', replicas: 3, addr: 'greeter-svc:50051' },
  { id: '2', ns: 'library', version: 'v2', kind: 'openapi', status: 'degraded', replicas: 2, addr: 'http://library:8081' },
  { id: '3', ns: 'billing', version: 'unstable', kind: 'graphql', status: 'waiting', replicas: 0, addr: 'http://billing:4000/graphql' },
  { id: '4', ns: 'greeter', version: 'v0', kind: 'proto', status: 'draining', replicas: 1, addr: 'greeter-old:50051' },
  { id: '5', ns: 'tools', version: 'v1', kind: 'mcp', status: 'sunsetable', replicas: 1, addr: 'stdio: ./tools-mcp' },
];

const columns: Column<Svc>[] = [
  { id: 'ns', header: 'Namespace', sortValue: (r) => r.ns, cell: (r) => <strong>{r.ns}</strong> },
  { id: 'version', header: 'Version', sortValue: (r) => r.version, mono: true },
  { id: 'kind', header: 'Kind', sortValue: (r) => r.kind, cell: (r) => <Chip size="small" variant="outlined" label={r.kind} /> },
  { id: 'status', header: 'Status', sortValue: (r) => r.status, cell: (r) => <StatusChip status={r.status} size="small" /> },
  { id: 'replicas', header: 'Replicas', align: 'right', sortValue: (r) => r.replicas },
  { id: 'addr', header: 'Address', mono: true, sortValue: (r) => r.addr },
];

const gql = `mutation CreateService($body: CreateServiceBody!) {
  admin {
    createService(body: $body) {
      namespace
      version
    }
  }
}`;

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Panel title={title}>
      <Stack spacing={2}>{children}</Stack>
    </Panel>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Box>
      <Typography variant="overline" color="text.secondary">
        {label}
      </Typography>
      <Stack direction="row" spacing={1.5} useFlexGap sx={{ flexWrap: 'wrap', alignItems: 'flex-start', mt: 0.5 }}>
        {children}
      </Stack>
    </Box>
  );
}

/**
 * Layout-stability demo. Three fields in a row; the middle one toggles an
 * error. "Measure" records offsetTop/offsetHeight of the neighbours and of
 * the block below before and after the toggle, and reports the largest move.
 */
function FormLayoutDemo() {
  const [err, setErr] = useState(false);
  const [report, setReport] = useState<string | null>(null);
  const root = useRef<HTMLDivElement>(null);

  const snap = () =>
    Array.from(root.current?.querySelectorAll<HTMLElement>('[data-probe]') ?? []).map((el) => ({
      name: el.dataset.probe ?? '',
      top: el.getBoundingClientRect().top,
      height: el.getBoundingClientRect().height,
    }));

  const measure = () => {
    flushSync(() => setErr(false));
    const before = snap();
    flushSync(() => setErr(true));
    const after = snap();
    const rows = before.map((b, i) => ({
      name: b.name,
      dTop: after[i].top - b.top,
      dHeight: after[i].height - b.height,
    }));
    const max = Math.max(...rows.map((r) => Math.max(Math.abs(r.dTop), Math.abs(r.dHeight))));
    setReport(
      `max shift ${max.toFixed(2)}px (${rows
        .map((r) => `${r.name}: top ${r.dTop.toFixed(1)}, height ${r.dHeight.toFixed(1)}`)
        .join('; ')})`,
    );
  };

  return (
    <Box ref={root}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'flex-start' }}>
        <Box sx={{ flex: 1 }} data-probe="left">
          <TextField fullWidth label="Namespace" defaultValue="greeter" helperText="Lowercase letters and dashes." />
        </Box>
        <Box sx={{ flex: 1 }} data-probe="middle">
          <TextField
            fullWidth
            label="Address"
            defaultValue="greeter-svc:50051"
            error={err}
            helperText={err ? 'Unreachable: connection refused after 3 attempts.' : undefined}
          />
        </Box>
        <Box sx={{ flex: 1 }} data-probe="right">
          <TextField fullWidth select label="Kind" defaultValue="proto">
            <MenuItem value="proto">gRPC (.proto)</MenuItem>
            <MenuItem value="openapi">OpenAPI</MenuItem>
          </TextField>
        </Box>
      </Stack>
      <Typography variant="body2" data-probe="below" sx={{ mt: 1 }}>
        Block below the row. It must not move when the middle field shows an error.
      </Typography>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mt: 1.5 }}>
        <Button variant="outlined" size="small" onClick={() => setErr((v) => !v)}>
          Toggle error
        </Button>
        <Button variant="outlined" size="small" onClick={measure}>
          Measure
        </Button>
        {report && (
          <Typography variant="caption" color="text.secondary" sx={{ wordBreak: 'break-word' }}>
            {report}
          </Typography>
        )}
      </Stack>
    </Box>
  );
}

function DialogsDemo() {
  const toast = useToast();
  const [form, setForm] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [confirmFail, setConfirmFail] = useState(false);

  return (
    <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: 'wrap' }}>
      <Button variant="contained" startIcon={<AddIcon />} onClick={() => setForm(true)}>
        FormDialog (server field errors)
      </Button>
      <Button variant="outlined" onClick={() => setConfirm(true)}>
        ConfirmDialog
      </Button>
      <Button variant="outlined" color="error" onClick={() => setConfirmFail(true)}>
        Destructive, fails
      </Button>
      <FormDialog
        open={form}
        title="Register service"
        description="Submit as-is: the fake server rejects it with extensions.fields (body.name, body.inner.key) and one unmatched path."
        submitLabel="Register"
        defaultValues={{ name: 'Greeter', inner: { key: '' }, kind: 'proto', owner: '', tier: '', enabled: true }}
        onClose={() => setForm(false)}
        onSubmit={async (v) => {
          await new Promise((r) => setTimeout(r, 600));
          if (v.name !== v.name.toLowerCase() || v.inner.key === '') {
            throw new GraphQLRequestError([
              {
                message: 'validation failed',
                extensions: {
                  fields: [
                    { path: 'body.name', message: 'must be lowercase' },
                    { path: 'body.inner.key', message: 'is required' },
                    { path: 'body.replicas.2.host', message: 'no such field in this form' },
                  ],
                },
              },
            ]);
          }
          toast.success(`registered ${v.name}`);
        }}
      >
        <FormTextField name="name" label="Namespace" autoFocus helperText="Lowercase letters and dashes." />
        <FormTextField name="inner.key" label="Nested key" monospace />
        <FormSelect
          name="kind"
          label="Kind"
          options={[
            { value: 'proto', label: 'gRPC (.proto)' },
            { value: 'openapi', label: 'OpenAPI' },
            { value: 'graphql', label: 'GraphQL' },
          ]}
        />
        <FormRow>
          <FormTextField name="owner" label="Owner" helperText="Team that pages for this service." />
          <FormTextField name="tier" label="Tier" />
        </FormRow>
        <FormSwitch name="enabled" label="Enabled" />
      </FormDialog>
      <ConfirmDialog
        open={confirm}
        title="Forget peer?"
        message="Node n-4f2a is removed from the cluster view. It rejoins on its next heartbeat."
        confirmLabel="Forget"
        onClose={() => setConfirm(false)}
        onConfirm={async () => {
          await new Promise((r) => setTimeout(r, 400));
          toast.info('peer forgotten');
        }}
      />
      <ConfirmDialog
        open={confirmFail}
        destructive
        title="Delete slot greeter/v0?"
        message="This cannot be undone."
        confirmLabel="Delete"
        onClose={() => setConfirmFail(false)}
        onConfirm={async () => {
          await new Promise((r) => setTimeout(r, 400));
          throw new Error('slot is locked: version v0 is pinned');
        }}
      />
    </Stack>
  );
}

function Body() {
  const toast = useToast();
  const { environment } = useThemeMode();
  const [sel, setSel] = useState<Svc | null>(null);

  return (
    <Page
      breadcrumbs={['Admin', 'Kit']}
      title="Services"
      subtitle="Page header with breadcrumbs, meta, subtitle and actions."
      meta={<EnvBadge name={environment} />}
      actions={
        <>
          <Button variant="outlined" startIcon={<RefreshIcon />}>
            Refresh
          </Button>
          <Button variant="contained" startIcon={<AddIcon />}>
            Add service
          </Button>
        </>
      }
    >
      <Section title="Status chips">
        <Row label="medium">
          {statusKeys.map((s) => (
            <StatusChip key={s} status={s} />
          ))}
        </Row>
        <Row label="small">
          {statusKeys.map((s) => (
            <StatusChip key={s} status={s} size="small" />
          ))}
        </Row>
        <Row label="custom label">
          <StatusChip status="ready" label="3/3 replicas" />
          <StatusChip status="degraded" label="1 of 3 down" />
        </Row>
      </Section>

      <Panel
        title="DataTable"
        subtitle="Click headers to sort; click a row to open the DetailPanel."
        actions={<Button size="small">Action</Button>}
        flush
      >
        <DataTable
          columns={columns}
          rows={services}
          getRowId={(r) => r.id}
          selectedId={sel?.id}
          onRowClick={setSel}
          defaultSort={{ columnId: 'ns', direction: 'asc' }}
        />
      </Panel>

      <Stack direction={{ xs: 'column', md: 'row' }} spacing={3} useFlexGap>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <Panel title="DataTable: empty" flush>
            <DataTable
              columns={columns}
              rows={[]}
              getRowId={(r) => r.id}
              emptyTitle="No services registered"
              emptyDescription="Register a proto, OpenAPI or GraphQL service to see it here."
            />
          </Panel>
        </Box>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <Panel title="DataTable: loading" flush>
            <DataTable columns={columns} rows={[]} getRowId={(r) => r.id} loading />
          </Panel>
        </Box>
      </Stack>

      <Stack direction={{ xs: 'column', md: 'row' }} spacing={3} useFlexGap>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <Panel title="EmptyState">
            <EmptyState
              icon={<CloudOffIcon />}
              title="No peers connected"
              description="This gateway runs standalone. Start another node with the same cluster name to see it."
              action={<Button variant="outlined">Read the cluster docs</Button>}
            />
          </Panel>
        </Box>
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <Panel title="KeyValue">
            <KeyValue
              items={[
                { label: 'Namespace', value: 'greeter' },
                { label: 'Node ID', value: 'n-4f2a91c0be', mono: true, copyable: true },
                { label: 'Address', value: 'greeter-svc:50051', mono: true, copyable: true },
                { label: 'Status', value: <StatusChip status="ready" size="small" /> },
                { label: 'Joined', value: new Date(1_700_000_000_000).toLocaleString() },
              ]}
            />
          </Panel>
        </Box>
      </Stack>

      <Panel title="CodeBlock" subtitle="Monospace with copy button.">
        <Stack spacing={2}>
          <CodeBlock language="graphql" code={gql} />
          <CodeBlock
            language="bash"
            wrap
            code={
              "curl -sS -H 'Authorization: Bearer $GWAG_TOKEN' http://localhost:18080/api/graphql " +
              '-d \'{"query":"{ admin { listPeers { peers { nodeId } } } }"}\''
            }
          />
        </Stack>
      </Panel>

      <Section title="Form layout stability">
        <FormLayoutDemo />
      </Section>

      <Section title="Dialogs">
        <DialogsDemo />
      </Section>

      <Section title="Toasts">
        <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: 'wrap' }}>
          <Button variant="outlined" onClick={() => toast.success('Service registered')}>
            Success
          </Button>
          <Button variant="outlined" onClick={() => toast.info('Reconciling 3 slots')}>
            Info
          </Button>
          <Button variant="outlined" onClick={() => toast.warning('Peer n-4f2a is lagging')}>
            Warning
          </Button>
          <Button variant="outlined" onClick={() => toast.error('Dispatch failed: upstream unavailable')}>
            Error
          </Button>
        </Stack>
      </Section>

      <Section title="Plain MUI, themed">
        <Row label="buttons">
          <Button variant="contained">Contained</Button>
          <Button variant="outlined">Outlined</Button>
          <Button>Text</Button>
          <Button variant="contained" color="error">
            Danger
          </Button>
          <Button variant="contained" disabled>
            Disabled
          </Button>
        </Row>
        <Row label="inputs">
          <TextField label="Text" defaultValue="value" />
          <TextField label="Error" defaultValue="bad" error helperText="Something is wrong" />
          <TextField select label="Select" defaultValue="a" sx={{ minWidth: 140 }}>
            <MenuItem value="a">Option A</MenuItem>
            <MenuItem value="b">Option B</MenuItem>
          </TextField>
          <FormControlLabel control={<Checkbox defaultChecked />} label="Checkbox" />
        </Row>
        <Row label="chips">
          <Chip label="default" />
          <Chip label="outlined" variant="outlined" />
          <Chip label="primary" color="primary" />
        </Row>
        <Divider />
        <Box>
          <Typography variant="h1">Heading 1</Typography>
          <Typography variant="h2">Heading 2</Typography>
          <Typography variant="h3">Heading 3</Typography>
          <Typography variant="h4">Heading 4</Typography>
          <Typography variant="body1">Body 1: the quick brown fox jumps over the lazy dog.</Typography>
          <Typography variant="body2" color="text.secondary">
            Body 2 secondary: the quick brown fox jumps over the lazy dog.
          </Typography>
          <Typography variant="overline">Overline label</Typography>
        </Box>
      </Section>

      <DetailPanel
        open={sel !== null}
        onClose={() => setSel(null)}
        title={sel ? `${sel.ns}/${sel.version}` : ''}
        subtitle={sel?.kind}
        meta={sel && <StatusChip status={sel.status} size="small" />}
        actions={
          <>
            <Button color="inherit" onClick={() => setSel(null)}>
              Close
            </Button>
            <Button variant="outlined" color="error">
              Drain
            </Button>
          </>
        }
      >
        {sel && (
          <Stack spacing={3}>
            <KeyValue
              labelWidth={110}
              items={[
                { label: 'Namespace', value: sel.ns },
                { label: 'Version', value: sel.version, mono: true },
                { label: 'Replicas', value: String(sel.replicas) },
                { label: 'Address', value: sel.addr, mono: true, copyable: true },
              ]}
            />
            <CodeBlock language="json" code={JSON.stringify(sel, null, 2)} />
          </Stack>
        )}
      </DetailPanel>
    </Page>
  );
}

export type View = 'gallery' | 'current' | 'dark' | 'light' | 'both';

function Framed({
  mode,
  preset,
  environment,
}: {
  mode: Mode;
  preset: PresetId;
  environment: string;
}) {
  return (
    <ScopedTheme mode={mode} preset={preset} environment={environment}>
      <ToastProvider>
        <Box
          sx={(t) => ({
            bgcolor: 'background.default',
            color: 'text.primary',
            p: 3,
            borderRadius: `${t.gwag.radius.lg}px`,
            border: `1px solid ${t.gwag.tokens.border}`,
            ...t.gwag.accentFrame,
            minWidth: 0,
          })}
        >
          <Typography variant="overline" color="text.secondary" sx={{ display: 'block', mb: 2 }}>
            {preset} / {mode}
          </Typography>
          <Body />
        </Box>
      </ToastProvider>
    </ScopedTheme>
  );
}

export function Showcase({
  view = 'gallery',
  onViewChange,
}: {
  view?: View;
  onViewChange?: (v: View) => void;
}) {
  const { mode, preset, setPreset, environment: appEnv } = useThemeMode();
  const [localView, setLocalView] = useState<View>(view);
  const cur = onViewChange ? view : localView;
  const setView = onViewChange ?? setLocalView;
  const [env, setEnv] = useState(appEnv);

  return (
    <Box>
      <Stack
        direction="row"
        spacing={2}
        useFlexGap
        sx={{ mb: 3, flexWrap: 'wrap', alignItems: 'flex-start' }}
      >
        <Typography variant="h3" sx={{ pt: 0.75 }}>
          UI kit
        </Typography>
        <Box sx={{ minWidth: 220 }}>
          <PresetPicker />
        </Box>
        <ToggleButtonGroup
          exclusive
          size="small"
          value={cur}
          onChange={(_e, v: View | null) => v && setView(v)}
          aria-label="view"
          sx={{ mt: '3px' }}
        >
          <ToggleButton value="gallery">Gallery</ToggleButton>
          <ToggleButton value="current">Full ({mode})</ToggleButton>
          <ToggleButton value="dark">Dark</ToggleButton>
          <ToggleButton value="light">Light</ToggleButton>
          <ToggleButton value="both">Side by side</ToggleButton>
        </ToggleButtonGroup>
        <TextField
          select
          label="Environment"
          value={env}
          onChange={(e) => setEnv(e.target.value)}
          sx={{ minWidth: 150 }}
        >
          {['dev', 'staging', 'prod', 'qa', 'eu-west-1'].map((e) => (
            <MenuItem key={e} value={e}>
              {e}
            </MenuItem>
          ))}
        </TextField>
        <Box sx={{ pt: '1px' }}>
          <ThemeToggle />
        </Box>
      </Stack>
      {cur === 'gallery' ? (
        <PresetGallery mode={mode} environment={env} selected={preset} onSelect={setPreset} />
      ) : cur === 'both' ? (
        <Stack direction={{ xs: 'column', xl: 'row' }} spacing={3} useFlexGap>
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Framed mode="dark" preset={preset} environment={env} />
          </Box>
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Framed mode="light" preset={preset} environment={env} />
          </Box>
        </Stack>
      ) : (
        <Framed mode={cur === 'current' ? mode : cur} preset={preset} environment={env} />
      )}
    </Box>
  );
}
