// Design tokens. Single source for colour, type, spacing, radius, shadow.
// Components read these through the MUI theme (theme.palette, theme.gwag),
// not by importing this file, except the theme builder itself.

export type Mode = 'dark' | 'light';

export const spacingUnit = 8;
export const radius = { sm: 4, md: 8, lg: 12, pill: 999 } as const;

export const fontFamily =
  '"Inter", "Roboto", -apple-system, "Segoe UI", Helvetica, Arial, sans-serif';
export const monoFamily =
  '"JetBrains Mono", "SFMono-Regular", Menlo, Consolas, "Liberation Mono", monospace';

// Type scale (rem). Headings are tight; body 14px for dense admin tables.
export const typeScale = {
  h1: { fontSize: '1.75rem', fontWeight: 650, lineHeight: 1.2, letterSpacing: '-0.01em' },
  h2: { fontSize: '1.375rem', fontWeight: 650, lineHeight: 1.25, letterSpacing: '-0.005em' },
  h3: { fontSize: '1.125rem', fontWeight: 600, lineHeight: 1.3 },
  h4: { fontSize: '1rem', fontWeight: 600, lineHeight: 1.35 },
  h5: { fontSize: '0.875rem', fontWeight: 600, lineHeight: 1.4 },
  h6: { fontSize: '0.8125rem', fontWeight: 600, lineHeight: 1.4 },
  subtitle1: { fontSize: '0.9375rem', fontWeight: 500, lineHeight: 1.45 },
  subtitle2: { fontSize: '0.8125rem', fontWeight: 500, lineHeight: 1.45 },
  body1: { fontSize: '0.875rem', lineHeight: 1.5 },
  body2: { fontSize: '0.8125rem', lineHeight: 1.5 },
  caption: { fontSize: '0.75rem', lineHeight: 1.4 },
  overline: {
    fontSize: '0.6875rem',
    fontWeight: 600,
    lineHeight: 1.4,
    letterSpacing: '0.08em',
  },
  button: { fontSize: '0.8125rem', fontWeight: 600, textTransform: 'none' as const },
} as const;

export interface ModeTokens {
  bg: string;
  surface: string; // panels, cards
  surfaceRaised: string; // dialogs, drawers, menus
  surfaceSunken: string; // code blocks, table headers
  border: string;
  borderStrong: string;
  text: string;
  textMuted: string;
  primary: string;
  primaryContrast: string;
  hover: string;
  shadow: { sm: string; md: string; lg: string };
}

export const modeTokens: Record<Mode, ModeTokens> = {
  dark: {
    bg: '#0b0f14',
    surface: '#121821',
    surfaceRaised: '#171e29',
    surfaceSunken: '#0e131a',
    border: '#243040',
    borderStrong: '#34445a',
    text: '#e6ebf2',
    textMuted: '#93a0b4',
    primary: '#6ea8ff',
    primaryContrast: '#06101f',
    hover: 'rgba(148, 176, 214, 0.08)',
    shadow: {
      sm: '0 1px 2px rgba(0,0,0,0.5)',
      md: '0 4px 12px rgba(0,0,0,0.5)',
      lg: '0 12px 32px rgba(0,0,0,0.6)',
    },
  },
  light: {
    bg: '#f4f6f9',
    surface: '#ffffff',
    surfaceRaised: '#ffffff',
    surfaceSunken: '#eef1f5',
    border: '#dbe1ea',
    borderStrong: '#bcc6d4',
    text: '#142033',
    textMuted: '#5a6a80',
    primary: '#1f5fd6',
    primaryContrast: '#ffffff',
    hover: 'rgba(20, 50, 100, 0.06)',
    shadow: {
      sm: '0 1px 2px rgba(16,30,54,0.08)',
      md: '0 4px 12px rgba(16,30,54,0.10)',
      lg: '0 12px 32px rgba(16,30,54,0.16)',
    },
  },
};

// Status colours. Each state has a distinct hue AND a distinct icon
// (see StatusChip) so colour is never the only signal. fg/bg/border
// pairs are chosen for >= 4.5:1 text contrast on the chip background.
export type StatusKey = 'ready' | 'waiting' | 'degraded' | 'draining' | 'sunsetable';

export interface StatusColors {
  fg: string;
  bg: string;
  border: string;
}

export const statusTokens: Record<Mode, Record<StatusKey, StatusColors>> = {
  dark: {
    ready: { fg: '#6fdc9a', bg: 'rgba(46,180,100,0.14)', border: 'rgba(46,180,100,0.45)' },
    waiting: { fg: '#8fb4ff', bg: 'rgba(90,140,255,0.14)', border: 'rgba(90,140,255,0.45)' },
    degraded: { fg: '#ffc857', bg: 'rgba(255,184,40,0.14)', border: 'rgba(255,184,40,0.5)' },
    draining: { fg: '#d6a4ff', bg: 'rgba(170,100,240,0.16)', border: 'rgba(170,100,240,0.5)' },
    sunsetable: { fg: '#ff9a7a', bg: 'rgba(255,120,80,0.14)', border: 'rgba(255,120,80,0.5)' },
  },
  light: {
    ready: { fg: '#0b6b3a', bg: '#e2f6ea', border: '#8ed2ac' },
    waiting: { fg: '#1b4fb8', bg: '#e5eeff', border: '#9db9f2' },
    degraded: { fg: '#8a5300', bg: '#fff3d6', border: '#e6c068' },
    draining: { fg: '#6d2fb0', bg: '#f1e6fc', border: '#c6a2ec' },
    sunsetable: { fg: '#a53a14', bg: '#ffeae2', border: '#f0a98f' },
  },
};

// Environment accent. Named environments get fixed hues; unknown names
// hash to a stable hue so every env is distinguishable.
const envFixed: Record<string, number> = {
  dev: 160, // green-teal
  development: 160,
  local: 200,
  test: 190,
  staging: 40, // amber
  stage: 40,
  qa: 280,
  prod: 5, // red
  production: 5,
};

export function envHue(name: string): number {
  const key = name.trim().toLowerCase();
  const fixed = envFixed[key];
  if (fixed !== undefined) return fixed;
  let h = 0;
  for (const ch of key) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return h % 360;
}

export function envAccent(name: string, mode: Mode): string {
  const hue = envHue(name);
  return mode === 'dark' ? `hsl(${hue} 70% 62%)` : `hsl(${hue} 70% 36%)`;
}
