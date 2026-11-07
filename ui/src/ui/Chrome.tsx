import { useState } from 'react';
import {
  Box,
  IconButton,
  ListItemText,
  Menu,
  MenuItem,
  TextField,
  Tooltip,
} from '@mui/material';
import PaletteIcon from '@mui/icons-material/Palette';
import DarkModeIcon from '@mui/icons-material/DarkMode';
import LightModeIcon from '@mui/icons-material/LightMode';
import { presetList, useThemeMode, type Mode, type Preset } from './theme';

/** Dark/light switch. Persists via AppThemeProvider. */
export function ThemeToggle() {
  const { mode, toggle } = useThemeMode();
  const next = mode === 'dark' ? 'light' : 'dark';
  return (
    <Tooltip title={`Switch to ${next} theme`}>
      <IconButton color="inherit" onClick={toggle} aria-label={`switch to ${next} theme`}>
        {mode === 'dark' ? <LightModeIcon /> : <DarkModeIcon />}
      </IconButton>
    </Tooltip>
  );
}

/** Environment name pill, coloured by the environment accent. */
export function EnvBadge({ name }: { name?: string }) {
  return (
    <Box
      component="span"
      title="Environment"
      sx={(t) => ({
        display: 'inline-flex',
        alignItems: 'center',
        gap: 0.75,
        height: 24,
        px: 1,
        borderRadius: `${t.gwag.radius.sm}px`,
        border: `1px solid ${t.gwag.envAccent}`,
        color: t.gwag.envAccent,
        fontSize: '0.6875rem',
        fontWeight: 700,
        letterSpacing: '0.08em',
        textTransform: 'uppercase',
        '&::before': {
          content: '""',
          width: 7,
          height: 7,
          borderRadius: '50%',
          bgcolor: t.gwag.envAccent,
        },
      })}
    >
      {name ?? 'env'}
    </Box>
  );
}

/** Three-colour chip showing a preset's page, surface and primary colours. */
export function PresetSwatch({ preset, mode }: { preset: Preset; mode: Mode }) {
  const t = preset.modes[mode];
  return (
    <Box
      component="span"
      aria-hidden
      sx={{
        display: 'inline-flex',
        flexShrink: 0,
        height: 16,
        border: `1px solid ${t.borderStrong}`,
        borderRadius: `${Math.min(preset.radius.sm, 4)}px`,
        overflow: 'hidden',
        '& > span': { width: 10, height: '100%' },
      }}
    >
      <span style={{ background: t.bg }} />
      <span style={{ background: t.surface }} />
      <span style={{ background: t.primary }} />
    </Box>
  );
}

/**
 * Preset picker. `compact` renders a palette icon button with a menu (for
 * the app bar); otherwise a labelled select (for settings).
 */
export function PresetPicker({ compact }: { compact?: boolean }) {
  const { preset, setPreset, mode } = useThemeMode();
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const current = presetList.find((p) => p.id === preset) ?? presetList[0];

  if (compact) {
    return (
      <>
        <Tooltip title={`Theme preset: ${current.label}`}>
          <IconButton
            color="inherit"
            onClick={(e) => setAnchor(e.currentTarget)}
            aria-label="theme preset"
            aria-haspopup="menu"
          >
            <PaletteIcon />
          </IconButton>
        </Tooltip>
        <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)}>
          {presetList.map((p) => (
            <MenuItem
              key={p.id}
              selected={p.id === preset}
              onClick={() => {
                setPreset(p.id);
                setAnchor(null);
              }}
              sx={{ gap: 1.5 }}
            >
              <PresetSwatch preset={p} mode={mode} />
              <ListItemText primary={p.label} secondary={p.blurb} slotProps={{ secondary: { sx: { maxWidth: 260, whiteSpace: 'normal' } } }} />
            </MenuItem>
          ))}
        </Menu>
      </>
    );
  }
  return (
    <TextField
      select
      fullWidth
      label="Theme preset"
      value={preset}
      onChange={(e) => setPreset(e.target.value as Preset['id'])}
      helperText={current.blurb}
    >
      {presetList.map((p) => (
        <MenuItem key={p.id} value={p.id} sx={{ gap: 1.5 }}>
          <PresetSwatch preset={p} mode={mode} />
          {p.label}
        </MenuItem>
      ))}
    </TextField>
  );
}
