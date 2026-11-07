import { Box, Button, Stack, Table, TableBody, TableCell, TableHead, TableRow, TextField, Typography } from '@mui/material';
import CheckIcon from '@mui/icons-material/Check';
import { EnvBadge } from '../Chrome';
import { StatusChip } from '../StatusChip';
import {
  ScopedTheme,
  presetList,
  type Mode,
  type Preset,
  type PresetId,
} from '../theme';

function SwatchRow({ preset, mode }: { preset: Preset; mode: Mode }) {
  const t = preset.modes[mode];
  const s = preset.status[mode];
  const colors = [
    t.bg,
    t.surface,
    t.surfaceRaised,
    t.border,
    t.textMuted,
    t.text,
    t.primary,
    s.ready.fg,
    s.waiting.fg,
    s.degraded.fg,
    s.draining.fg,
    s.sunsetable.fg,
  ];
  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
      <Typography variant="caption" color="text.secondary" sx={{ width: 36 }}>
        {mode}
      </Typography>
      <Box
        sx={{
          display: 'flex',
          flex: 1,
          height: 22,
          overflow: 'hidden',
          borderRadius: `${preset.radius.sm}px`,
          border: `1px solid ${t.borderStrong}`,
        }}
      >
        {colors.map((c, i) => (
          <Box key={i} sx={{ flex: 1, bgcolor: c }} />
        ))}
      </Box>
    </Stack>
  );
}

function Preview({ preset }: { preset: Preset }) {
  return (
    <Stack spacing={1.5}>
      <Stack direction="row" sx={{ alignItems: 'flex-end', justifyContent: 'space-between', gap: 1 }}>
        <Box sx={{ minWidth: 0 }}>
          <Typography variant="caption" color="text.secondary">
            Admin / Services
          </Typography>
          <Typography variant="h3" component="div" noWrap>
            Services
          </Typography>
        </Box>
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
          <EnvBadge name="prod" />
          <Button size="small" variant="contained">
            Add
          </Button>
        </Stack>
      </Stack>
      <Stack direction="row" useFlexGap sx={{ flexWrap: 'wrap', gap: 0.75 }}>
        <StatusChip status="ready" size="small" />
        <StatusChip status="waiting" size="small" />
        <StatusChip status="degraded" size="small" />
        <StatusChip status="draining" size="small" />
        <StatusChip status="sunsetable" size="small" />
      </Stack>
      <Box sx={(t) => ({ border: `1px solid ${t.gwag.tokens.border}`, borderRadius: `${preset.radius.md}px`, overflow: 'hidden' })}>
        <Table>
          <TableHead>
            <TableRow>
              <TableCell>Namespace</TableCell>
              <TableCell>Status</TableCell>
              <TableCell align="right">Replicas</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            <TableRow>
              <TableCell>greeter</TableCell>
              <TableCell>
                <StatusChip status="ready" size="small" />
              </TableCell>
              <TableCell align="right">3</TableCell>
            </TableRow>
            <TableRow>
              <TableCell>library</TableCell>
              <TableCell>
                <StatusChip status="degraded" size="small" />
              </TableCell>
              <TableCell align="right">2</TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </Box>
      <Stack direction="row" spacing={1.5} sx={{ alignItems: 'flex-start' }}>
        <TextField label="Namespace" defaultValue="Greeter" error helperText="Must be lowercase." fullWidth />
        <TextField label="Version" defaultValue="v1" helperText="Locked after v1." fullWidth />
      </Stack>
    </Stack>
  );
}

function PresetCard({
  preset,
  mode,
  environment,
  selected,
  onSelect,
}: {
  preset: Preset;
  mode: Mode;
  environment: string;
  selected: boolean;
  onSelect: (id: PresetId) => void;
}) {
  return (
    <ScopedTheme preset={preset.id} mode={mode} environment={environment}>
      <Box
        role="button"
        tabIndex={0}
        aria-pressed={selected}
        aria-label={`Use the ${preset.label} preset`}
        onClick={() => onSelect(preset.id)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            onSelect(preset.id);
          }
        }}
        sx={(t) => ({
          bgcolor: 'background.default',
          color: 'text.primary',
          p: 2,
          cursor: 'pointer',
          textAlign: 'left',
          borderRadius: `${t.gwag.radius.lg}px`,
          border: `${selected ? 2 : 1}px solid ${selected ? t.gwag.tokens.primary : t.gwag.tokens.border}`,
          ...(selected ? {} : t.gwag.accentFrame),
          '&:hover': { borderColor: t.gwag.tokens.borderStrong },
          '&:focus-visible': { outline: `2px solid ${t.gwag.tokens.primary}`, outlineOffset: 2 },
        })}
      >
        <Stack direction="row" sx={{ alignItems: 'baseline', justifyContent: 'space-between', mb: 0.5 }}>
          <Typography variant="h2" component="div">
            {preset.label}
          </Typography>
          {selected && (
            <Stack direction="row" spacing={0.5} sx={{ alignItems: 'center', color: 'primary.main' }}>
              <CheckIcon fontSize="small" />
              <Typography variant="caption" sx={{ fontWeight: 600 }}>
                In use
              </Typography>
            </Stack>
          )}
        </Stack>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5, minHeight: '3em' }}>
          {preset.blurb} Accent: {preset.accentStyle}.
        </Typography>
        <Stack spacing={0.75} sx={{ mb: 2 }}>
          <SwatchRow preset={preset} mode="dark" />
          <SwatchRow preset={preset} mode="light" />
        </Stack>
        {/* Preview is decoration: not focusable, not clickable on its own. */}
        <Box inert aria-hidden sx={{ pointerEvents: 'none' }}>
          <Preview preset={preset} />
        </Box>
      </Box>
    </ScopedTheme>
  );
}

/** One compact card per preset. Click a card to apply that preset. */
export function PresetGallery({
  mode,
  environment,
  selected,
  onSelect,
}: {
  mode: Mode;
  environment: string;
  selected: PresetId;
  onSelect: (id: PresetId) => void;
}) {
  return (
    <Box
      sx={{
        display: 'grid',
        gap: 3,
        gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))', xl: 'repeat(3, minmax(0, 1fr))' },
      }}
    >
      {presetList.map((p) => (
        <PresetCard
          key={p.id}
          preset={p}
          mode={mode}
          environment={environment}
          selected={p.id === selected}
          onSelect={onSelect}
        />
      ))}
    </Box>
  );
}
