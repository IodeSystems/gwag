import { Box, IconButton, Tooltip } from '@mui/material';
import DarkModeIcon from '@mui/icons-material/DarkMode';
import LightModeIcon from '@mui/icons-material/LightMode';
import { useThemeMode } from './theme';

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
