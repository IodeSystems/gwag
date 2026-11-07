import { alpha, createTheme, type CSSObject, type Theme } from '@mui/material/styles';
import { appBarAccent, accentFrame } from './accent';
import { DEFAULT_PRESET, presets, type AccentStyle, type PresetId } from './presets';
import {
  envAccent,
  isQuietEnv,
  spacingUnit,
  typeScale,
  type Mode,
  type ModeTokens,
  type Radius,
  type StatusColors,
  type StatusKey,
} from './tokens';

export interface GwagThemeExtras {
  preset: PresetId;
  accentStyle: AccentStyle;
  /** Style object that frames a card with the environment accent. */
  accentFrame: CSSObject;
  environment: string;
  envAccent: string;
  mono: string;
  tokens: ModeTokens;
  status: Record<StatusKey, StatusColors>;
  radius: Radius;
}

declare module '@mui/material/styles' {
  interface Theme {
    gwag: GwagThemeExtras;
  }
  interface ThemeOptions {
    gwag?: GwagThemeExtras;
  }
}

/** Helper-text line height, in em of the caption size. */
const HELPER_LINE = 1.4;

export function createAppTheme(
  mode: Mode,
  environment: string,
  presetId: PresetId = DEFAULT_PRESET,
): Theme {
  const preset = presets[presetId];
  const t = preset.modes[mode];
  const status = preset.status[mode];
  const radius = preset.radius;
  const { body, heading, mono: monoFamily } = preset.fonts;
  const accent = envAccent(environment, preset.env[mode]);
  const frame = preset.brandFrame && isQuietEnv(environment) ? t.primary : accent;
  const gwag: GwagThemeExtras = {
    preset: preset.id,
    accentStyle: preset.accentStyle,
    accentFrame: accentFrame(preset.accentStyle, frame, t.surface),
    environment,
    envAccent: accent,
    mono: monoFamily,
    tokens: t,
    status,
    radius,
  };
  const headingFont = { fontFamily: heading };

  return createTheme({
    gwag,
    spacing: spacingUnit,
    shape: { borderRadius: radius.md },
    palette: {
      mode,
      primary: { main: t.primary, contrastText: t.primaryContrast },
      secondary: { main: accent },
      success: { main: status.ready.fg },
      info: { main: status.waiting.fg },
      warning: { main: status.degraded.fg },
      error: { main: t.error },
      background: { default: t.bg, paper: t.surface },
      text: { primary: t.text, secondary: t.textMuted },
      divider: t.border,
      action: { hover: t.hover },
    },
    typography: {
      fontFamily: body,
      ...typeScale,
      h1: { ...typeScale.h1, ...headingFont },
      h2: { ...typeScale.h2, ...headingFont },
      h3: { ...typeScale.h3, ...headingFont },
      h4: { ...typeScale.h4, ...headingFont },
      h5: { ...typeScale.h5, ...headingFont },
      h6: { ...typeScale.h6, ...headingFont },
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
            // Environment accent: treatment depends on the preset (see accent.ts).
            boxShadow: appBarAccent(preset.accentStyle, frame, t.surface),
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
      // Helper text: every field reserves one line of caption height, even
      // with no helper, so an error appearing does not move anything.
      // Up to two lines show; beyond that the text is clamped with an
      // ellipsis (the Form* components add the full text as a title).
      // Fields in a row align to the top (labels, inputs, helper text).
      MuiTextField: {
        defaultProps: { size: 'small', variant: 'outlined', helperText: ' ' },
        styleOverrides: { root: { verticalAlign: 'top' } },
      },
      MuiFormControl: { styleOverrides: { root: { verticalAlign: 'top' } } },
      MuiFormHelperText: {
        styleOverrides: {
          root: {
            ...typeScale.caption,
            lineHeight: HELPER_LINE,
            minHeight: `${HELPER_LINE}em`,
            marginTop: 4,
            display: '-webkit-box',
            WebkitBoxOrient: 'vertical',
            WebkitLineClamp: 2,
            overflow: 'hidden',
            overflowWrap: 'anywhere',
          },
        },
      },
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
            backgroundColor: t.tooltip,
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
