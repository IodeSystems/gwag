import { alpha, createTheme, type Theme } from '@mui/material/styles';
import {
  envAccent,
  fontFamily,
  modeTokens,
  monoFamily,
  radius,
  spacingUnit,
  statusTokens,
  typeScale,
  type Mode,
  type ModeTokens,
  type StatusColors,
  type StatusKey,
} from './tokens';

export interface GwagThemeExtras {
  environment: string;
  envAccent: string;
  mono: string;
  tokens: ModeTokens;
  status: Record<StatusKey, StatusColors>;
  radius: typeof radius;
}

declare module '@mui/material/styles' {
  interface Theme {
    gwag: GwagThemeExtras;
  }
  interface ThemeOptions {
    gwag?: GwagThemeExtras;
  }
}

export function createAppTheme(mode: Mode, environment: string): Theme {
  const t = modeTokens[mode];
  const accent = envAccent(environment, mode);
  const gwag: GwagThemeExtras = {
    environment,
    envAccent: accent,
    mono: monoFamily,
    tokens: t,
    status: statusTokens[mode],
    radius,
  };

  return createTheme({
    gwag,
    spacing: spacingUnit,
    shape: { borderRadius: radius.md },
    palette: {
      mode,
      primary: { main: t.primary, contrastText: t.primaryContrast },
      secondary: { main: accent },
      success: { main: statusTokens[mode].ready.fg },
      info: { main: statusTokens[mode].waiting.fg },
      warning: { main: statusTokens[mode].degraded.fg },
      error: { main: mode === 'dark' ? '#ff7b7b' : '#c62828' },
      background: { default: t.bg, paper: t.surface },
      text: { primary: t.text, secondary: t.textMuted },
      divider: t.border,
      action: { hover: t.hover },
    },
    typography: {
      fontFamily,
      ...typeScale,
    },
    shadows: [
      'none',
      t.shadow.sm,
      t.shadow.sm,
      t.shadow.md,
      t.shadow.md,
      t.shadow.md,
      t.shadow.md,
      t.shadow.md,
      t.shadow.lg,
      ...Array<string>(16).fill(t.shadow.lg),
    ] as Theme['shadows'],
    components: {
      MuiCssBaseline: {
        styleOverrides: {
          body: { backgroundColor: t.bg },
          'code, kbd, pre, samp': { fontFamily: monoFamily },
          '::selection': { background: alpha(t.primary, 0.35) },
        },
      },
      MuiPaper: {
        defaultProps: { elevation: 0 },
        styleOverrides: {
          root: { backgroundImage: 'none' },
          outlined: { borderColor: t.border },
        },
      },
      MuiAppBar: {
        defaultProps: { elevation: 0, color: 'default' },
        styleOverrides: {
          root: {
            backgroundColor: t.surface,
            color: t.text,
            backgroundImage: 'none',
            borderBottom: `1px solid ${t.border}`,
            // Environment accent: a 3px bar on the header's top edge.
            boxShadow: `inset 0 3px 0 ${accent}`,
          },
        },
      },
      MuiDrawer: {
        styleOverrides: {
          paper: { backgroundColor: t.surface, borderColor: t.border, backgroundImage: 'none' },
        },
      },
      MuiButton: {
        defaultProps: { disableElevation: true },
        styleOverrides: {
          root: { borderRadius: radius.md },
          outlined: { borderColor: t.borderStrong },
        },
      },
      MuiIconButton: { styleOverrides: { root: { borderRadius: radius.md } } },
      MuiChip: {
        styleOverrides: {
          root: { borderRadius: radius.sm, fontWeight: 600 },
        },
      },
      MuiTextField: { defaultProps: { size: 'small', variant: 'outlined' } },
      MuiOutlinedInput: {
        styleOverrides: {
          root: {
            backgroundColor: t.surfaceSunken,
            '& .MuiOutlinedInput-notchedOutline': { borderColor: t.border },
            '&:hover .MuiOutlinedInput-notchedOutline': { borderColor: t.borderStrong },
          },
        },
      },
      MuiSelect: { defaultProps: { size: 'small' } },
      MuiTable: { defaultProps: { size: 'small' } },
      MuiTableCell: {
        styleOverrides: {
          root: { borderBottomColor: t.border, padding: '10px 16px' },
          head: {
            backgroundColor: t.surfaceSunken,
            color: t.textMuted,
            fontSize: '0.6875rem',
            fontWeight: 600,
            letterSpacing: '0.06em',
            textTransform: 'uppercase',
          },
        },
      },
      MuiTableRow: {
        styleOverrides: { root: { '&:last-child td': { borderBottom: 0 } } },
      },
      MuiDialog: {
        styleOverrides: {
          paper: {
            backgroundColor: t.surfaceRaised,
            backgroundImage: 'none',
            border: `1px solid ${t.border}`,
            borderRadius: radius.lg,
            boxShadow: t.shadow.lg,
          },
        },
      },
      MuiDialogTitle: { styleOverrides: { root: { ...typeScale.h3 } } },
      MuiMenu: {
        styleOverrides: {
          paper: { backgroundColor: t.surfaceRaised, border: `1px solid ${t.border}` },
        },
      },
      MuiTooltip: {
        styleOverrides: {
          tooltip: {
            backgroundColor: mode === 'dark' ? '#2a3648' : '#1d2a3f',
            color: '#f2f5fa',
            fontSize: '0.75rem',
          },
        },
      },
      MuiAlert: { styleOverrides: { root: { borderRadius: radius.md, alignItems: 'center' } } },
      MuiListItemButton: {
        styleOverrides: {
          root: {
            borderRadius: radius.md,
            '&.active, &.Mui-selected': { backgroundColor: alpha(t.primary, 0.16) },
          },
        },
      },
      MuiLink: { defaultProps: { underline: 'hover' } },
      MuiDivider: { styleOverrides: { root: { borderColor: t.border } } },
    },
  });
}
