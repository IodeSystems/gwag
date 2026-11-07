// Design token types + shared scales. Per-preset colour/font/radius data
// lives in presets.ts. Single source for colour, type, spacing, radius, shadow.
// Components read these through the MUI theme (theme.palette, theme.gwag),
// not by importing this file, except the theme builder itself.

export type Mode = 'dark' | 'light';

export const spacingUnit = 8;
export interface Radius {
  sm: number;
  md: number;
  lg: number;
  pill: number;
}

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
  error: string;
  tooltip: string; // tooltip background
  hover: string;
  shadow: { sm: string; md: string; lg: string };
}

// Status colours. Each state has a distinct hue AND a distinct icon
// (see StatusChip) so colour is never the only signal. Per-preset values
// live in presets.ts; fg/bg pairs are checked for >= 4.5:1 text contrast.
export type StatusKey = 'ready' | 'waiting' | 'degraded' | 'draining' | 'sunsetable';

export interface StatusColors {
  fg: string;
  bg: string;
  border: string;
}

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

export interface EnvTune {
  s: number; // saturation %
  l: number; // lightness %
}

export function envAccent(name: string, tune: EnvTune): string {
  return `hsl(${envHue(name)} ${tune.s}% ${tune.l}%)`;
}

const quietEnvs = new Set(['dev', 'development', 'local', 'test']);

/** True for low-stakes environments, where a brand-coloured frame is fine. */
export function isQuietEnv(name: string): boolean {
  return quietEnvs.has(name.trim().toLowerCase());
}
