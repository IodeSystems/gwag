// Theme presets. Each preset is a full dark + light pair with its own type,
// radius, status palette and accent treatment. Data only: this file imports
// types and nothing else, so scripts can load it without a bundler
// (see ui/scripts/contrast.mjs).
import type { EnvTune, Mode, ModeTokens, Radius, StatusColors, StatusKey } from './tokens';

export type PresetId = 'glow' | 'walnut' | 'graphite' | 'paper' | 'harbor' | 'terminal';

/**
 * How the environment accent frames the app bar and framed cards.
 * - glow:     thick top bar plus a soft outer glow (the original look)
 * - rule:     thin solid rule on the bottom / left edge, no glow
 * - none:     no frame; the environment shows only in the EnvBadge
 * - double:   top double rule (print style)
 * - hairline: 1px ring at reduced strength
 * - outline:  1px solid outline
 */
export type AccentStyle = 'glow' | 'rule' | 'none' | 'double' | 'hairline' | 'outline';

export interface Fonts {
  body: string;
  heading: string;
  mono: string;
}

export interface Preset {
  id: PresetId;
  label: string;
  blurb: string;
  accentStyle: AccentStyle;
  /**
   * Frame in the preset's primary colour in quiet environments (dev, local,
   * test); staging and prod still frame in the environment colour. The
   * EnvBadge always uses the environment colour.
   */
  brandFrame?: boolean;
  fonts: Fonts;
  radius: Radius;
  env: Record<Mode, EnvTune>;
  modes: Record<Mode, ModeTokens>;
  status: Record<Mode, Record<StatusKey, StatusColors>>;
}

const sans = '"Inter", "Roboto", -apple-system, "Segoe UI", Helvetica, Arial, sans-serif';
const mono =
  '"JetBrains Mono", "SFMono-Regular", Menlo, Consolas, "Liberation Mono", monospace';
const serif = '"Source Serif 4", "Iowan Old Style", Georgia, "Times New Roman", serif';

const darkShadow = {
  sm: '0 1px 2px rgba(0,0,0,0.5)',
  md: '0 4px 12px rgba(0,0,0,0.5)',
  lg: '0 12px 32px rgba(0,0,0,0.6)',
};
const lightShadow = (rgb: string) => ({
  sm: `0 1px 2px rgba(${rgb},0.08)`,
  md: `0 4px 12px rgba(${rgb},0.10)`,
  lg: `0 12px 32px rgba(${rgb},0.16)`,
});
const noShadow = { sm: 'none', md: 'none', lg: 'none' };

// Dark-mode chip: translucent tint of the foreground hue.
const dk = (fg: string, rgb: string): StatusColors => ({
  fg,
  bg: `rgba(${rgb},0.14)`,
  border: `rgba(${rgb},0.5)`,
});
const lt = (fg: string, bg: string, border: string): StatusColors => ({ fg, bg, border });

export const presets: Record<PresetId, Preset> = {
  glow: {
    id: 'glow',
    label: 'Glow',
    blurb: 'Original. Cool blue-black, bright accent bar with a soft glow.',
    accentStyle: 'glow',
    fonts: { body: sans, heading: sans, mono },
    radius: { sm: 4, md: 8, lg: 12, pill: 999 },
    env: { dark: { s: 70, l: 64 }, light: { s: 75, l: 27 } },
    status: {
      dark: {
        ready: dk('#6fdc9a', '46,180,100'),
        waiting: dk('#8fb4ff', '90,140,255'),
        degraded: dk('#ffc857', '255,184,40'),
        draining: dk('#d6a4ff', '170,100,240'),
        sunsetable: dk('#ff9a7a', '255,120,80'),
      },
      light: {
        ready: lt('#0b6b3a', '#e2f6ea', '#8ed2ac'),
        waiting: lt('#1b4fb8', '#e5eeff', '#9db9f2'),
        degraded: lt('#8a5300', '#fff3d6', '#e6c068'),
        draining: lt('#6d2fb0', '#f1e6fc', '#c6a2ec'),
        sunsetable: lt('#a53a14', '#ffeae2', '#f0a98f'),
      },
    },
    modes: {
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
        error: '#ff7b7b',
        tooltip: '#2a3648',
        hover: 'rgba(148, 176, 214, 0.08)',
        shadow: darkShadow,
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
        error: '#c62828',
        tooltip: '#1d2a3f',
        hover: 'rgba(20, 50, 100, 0.06)',
        shadow: lightShadow('16,30,54'),
      },
    },
  },

  walnut: {
    id: 'walnut',
    label: 'Walnut',
    blurb: 'Warm umber surfaces, brass accent. Solid thin rule, no glow.',
    accentStyle: 'rule',
    brandFrame: true,
    fonts: { body: sans, heading: sans, mono },
    radius: { sm: 3, md: 6, lg: 10, pill: 999 },
    env: { dark: { s: 62, l: 66 }, light: { s: 72, l: 27 } },
    status: {
      dark: {
        ready: dk('#8fd49a', '90,185,110'),
        waiting: dk('#92b8e8', '100,150,230'),
        degraded: dk('#f2c451', '240,190,60'),
        draining: dk('#d3a6e6', '175,110,215'),
        sunsetable: dk('#ff9f80', '255,125,90'),
      },
      light: {
        ready: lt('#1f6a35', '#e3f1e0', '#98c9a0'),
        waiting: lt('#1f4f96', '#e3ecf7', '#9ab6dc'),
        degraded: lt('#7d4d00', '#f9ecc9', '#dcb762'),
        draining: lt('#69308f', '#f0e5f5', '#c5a3d8'),
        sunsetable: lt('#9d3512', '#fbe5da', '#e8a58a'),
      },
    },
    modes: {
      dark: {
        bg: '#17110d',
        surface: '#201813',
        surfaceRaised: '#291f18',
        surfaceSunken: '#130e0b',
        border: '#3b2d23',
        borderStrong: '#56412f',
        text: '#efe4d6',
        textMuted: '#b5a28d',
        primary: '#d9a441',
        primaryContrast: '#1a1208',
        error: '#ff8a7a',
        tooltip: '#3a2c20',
        hover: 'rgba(217, 164, 65, 0.09)',
        shadow: darkShadow,
      },
      light: {
        bg: '#f2eadf',
        surface: '#fbf6ee',
        surfaceRaised: '#fffaf2',
        surfaceSunken: '#eadfd0',
        border: '#dccfbc',
        borderStrong: '#bba98e',
        text: '#2b1d12',
        textMuted: '#6a5540',
        primary: '#8a5a14',
        primaryContrast: '#fff8ec',
        error: '#b3261e',
        tooltip: '#3a2819',
        hover: 'rgba(110, 70, 20, 0.07)',
        shadow: lightShadow('60,35,10'),
      },
    },
  },

  graphite: {
    id: 'graphite',
    label: 'Graphite',
    blurb: 'Neutral greys. No colour accent; emphasis by weight and contrast.',
    accentStyle: 'none',
    fonts: { body: sans, heading: sans, mono },
    radius: { sm: 3, md: 6, lg: 8, pill: 999 },
    env: { dark: { s: 55, l: 66 }, light: { s: 70, l: 28 } },
    status: {
      dark: {
        ready: dk('#7ad6a0', '60,180,110'),
        waiting: dk('#92b6f2', '100,150,240'),
        degraded: dk('#f2c755', '240,190,60'),
        draining: dk('#cfa8f0', '170,110,230'),
        sunsetable: dk('#ff9f85', '255,125,95'),
      },
      light: {
        ready: lt('#0f6a3a', '#e6f3ec', '#9ccdb2'),
        waiting: lt('#1f4fb0', '#e8eefb', '#a3b9ea'),
        degraded: lt('#7f4e00', '#f8eed2', '#dcbd70'),
        draining: lt('#6a2fa8', '#f1e8fa', '#c8a8e8'),
        sunsetable: lt('#a03512', '#fbe9e1', '#eaa88f'),
      },
    },
    modes: {
      dark: {
        bg: '#111111',
        surface: '#191919',
        surfaceRaised: '#202020',
        surfaceSunken: '#141414',
        border: '#2e2e2e',
        borderStrong: '#464646',
        text: '#ededed',
        textMuted: '#a3a3a3',
        primary: '#ededed',
        primaryContrast: '#111111',
        error: '#ff8080',
        tooltip: '#333333',
        hover: 'rgba(255, 255, 255, 0.07)',
        shadow: darkShadow,
      },
      light: {
        bg: '#f4f4f4',
        surface: '#ffffff',
        surfaceRaised: '#ffffff',
        surfaceSunken: '#eeeeee',
        border: '#dcdcdc',
        borderStrong: '#b5b5b5',
        text: '#161616',
        textMuted: '#5c5c5c',
        primary: '#1a1a1a',
        primaryContrast: '#ffffff',
        error: '#c62828',
        tooltip: '#262626',
        hover: 'rgba(0, 0, 0, 0.05)',
        shadow: lightShadow('0,0,0'),
      },
    },
  },

  paper: {
    id: 'paper',
    label: 'Paper',
    blurb: 'Warm off-white and ink. Serif headings. Dark mode is sepia-night.',
    accentStyle: 'double',
    fonts: { body: sans, heading: serif, mono },
    radius: { sm: 2, md: 3, lg: 4, pill: 999 },
    env: { dark: { s: 55, l: 64 }, light: { s: 68, l: 28 } },
    status: {
      dark: {
        ready: dk('#9ccf8a', '120,185,100'),
        waiting: dk('#9dbbe6', '110,150,220'),
        degraded: dk('#e9c260', '230,185,70'),
        draining: dk('#cfa8dc', '170,115,200'),
        sunsetable: dk('#f09f86', '240,125,95'),
      },
      light: {
        ready: lt('#2a601f', '#e6efdc', '#a5c795'),
        waiting: lt('#244b8c', '#e4ebf5', '#a1b7d8'),
        degraded: lt('#6f4700', '#f5ebc8', '#d6b865'),
        draining: lt('#62308a', '#eee4f3', '#c3a5d4'),
        sunsetable: lt('#962f10', '#f8e4d8', '#e3a58b'),
      },
    },
    modes: {
      dark: {
        bg: '#1b1813',
        surface: '#242018',
        surfaceRaised: '#2c271e',
        surfaceSunken: '#171410',
        border: '#3c3629',
        borderStrong: '#564d3b',
        text: '#e8dfc9',
        textMuted: '#ada28a',
        primary: '#9dbbe6',
        primaryContrast: '#14181f',
        error: '#f0948a',
        tooltip: '#3a3427',
        hover: 'rgba(232, 223, 201, 0.07)',
        shadow: darkShadow,
      },
      light: {
        bg: '#f5f0e4',
        surface: '#fffdf8',
        surfaceRaised: '#ffffff',
        surfaceSunken: '#eee7d8',
        border: '#ddd3be',
        borderStrong: '#bfb298',
        text: '#1f1a14',
        textMuted: '#625849',
        primary: '#1c3d6e',
        primaryContrast: '#fffdf8',
        error: '#a8231b',
        tooltip: '#2a241b',
        hover: 'rgba(60, 45, 20, 0.06)',
        shadow: lightShadow('50,40,20'),
      },
    },
  },

  harbor: {
    id: 'harbor',
    label: 'Harbor',
    blurb: 'Cool blue-grey, muted frost accent. Hairline ring, quiet.',
    accentStyle: 'hairline',
    fonts: { body: sans, heading: sans, mono },
    radius: { sm: 5, md: 8, lg: 14, pill: 999 },
    env: { dark: { s: 50, l: 68 }, light: { s: 60, l: 29 } },
    status: {
      dark: {
        ready: dk('#8fd1ac', '90,185,130'),
        waiting: dk('#9cc3ee', '110,160,230'),
        degraded: dk('#ecc972', '235,195,95'),
        draining: dk('#c6b0ee', '160,130,225'),
        sunsetable: dk('#f2a590', '240,135,105'),
      },
      light: {
        ready: lt('#1a6140', '#e0f0e8', '#95c7ad'),
        waiting: lt('#1f4f8a', '#e0ebf6', '#9bb9dc'),
        degraded: lt('#765000', '#f6ecd0', '#d8bd72'),
        draining: lt('#5a3d9a', '#ebe6f6', '#bdaee0'),
        sunsetable: lt('#9a3818', '#f9e6df', '#e5ab98'),
      },
    },
    modes: {
      dark: {
        bg: '#1a1f27',
        surface: '#212832',
        surfaceRaised: '#29313d',
        surfaceSunken: '#171b22',
        border: '#344050',
        borderStrong: '#495870',
        text: '#dde3ec',
        textMuted: '#9eabbe',
        primary: '#88b6d2',
        primaryContrast: '#0f1a22',
        error: '#f09a9a',
        tooltip: '#3a4658',
        hover: 'rgba(136, 182, 210, 0.09)',
        shadow: darkShadow,
      },
      light: {
        bg: '#e8edf2',
        surface: '#f6f8fa',
        surfaceRaised: '#ffffff',
        surfaceSunken: '#dde4ec',
        border: '#cbd4df',
        borderStrong: '#a8b5c6',
        text: '#1f2937',
        textMuted: '#50607a',
        primary: '#2f6485',
        primaryContrast: '#ffffff',
        error: '#b3333a',
        tooltip: '#2b3646',
        hover: 'rgba(40, 80, 120, 0.07)',
        shadow: lightShadow('30,50,80'),
      },
    },
  },

  terminal: {
    id: 'terminal',
    label: 'Terminal',
    blurb: 'Near-black, monospace, phosphor green. Square corners, outline.',
    accentStyle: 'outline',
    fonts: { body: mono, heading: mono, mono },
    radius: { sm: 0, md: 0, lg: 0, pill: 0 },
    env: { dark: { s: 80, l: 60 }, light: { s: 80, l: 26 } },
    status: {
      dark: {
        ready: dk('#5dff94', '60,230,120'),
        waiting: dk('#6cc4ff', '60,160,255'),
        degraded: dk('#ffd04d', '255,200,50'),
        draining: dk('#e08cff', '200,100,255'),
        sunsetable: dk('#ff8a66', '255,110,70'),
      },
      light: {
        ready: lt('#0a5c26', '#dcf2de', '#7fc48d'),
        waiting: lt('#10508f', '#dcebf7', '#8fb8dc'),
        degraded: lt('#705000', '#f5ecc4', '#d4b84e'),
        draining: lt('#68248f', '#eddcf5', '#c096d8'),
        sunsetable: lt('#9a2f0a', '#f8e0d4', '#e49c7e'),
      },
    },
    modes: {
      dark: {
        bg: '#050805',
        surface: '#0a100a',
        surfaceRaised: '#0f170f',
        surfaceSunken: '#070b07',
        border: '#1f321f',
        borderStrong: '#33513a',
        text: '#c4f0c8',
        textMuted: '#80a888',
        primary: '#39e07a',
        primaryContrast: '#03120a',
        error: '#ff7070',
        tooltip: '#1d2e1f',
        hover: 'rgba(57, 224, 122, 0.09)',
        shadow: noShadow,
      },
      light: {
        bg: '#edf1e6',
        surface: '#f7faf2',
        surfaceRaised: '#fcfef9',
        surfaceSunken: '#e3e9db',
        border: '#b8c6ac',
        borderStrong: '#8ea17f',
        text: '#0d1f10',
        textMuted: '#3f5a43',
        primary: '#0b6b2c',
        primaryContrast: '#f4fff6',
        error: '#b3201a',
        tooltip: '#16301b',
        hover: 'rgba(11, 107, 44, 0.08)',
        shadow: noShadow,
      },
    },
  },
};

export const presetList: Preset[] = [
  presets.glow,
  presets.walnut,
  presets.graphite,
  presets.paper,
  presets.harbor,
  presets.terminal,
];

export const DEFAULT_PRESET: PresetId = 'walnut';

export function isPresetId(v: unknown): v is PresetId {
  return typeof v === 'string' && v in presets;
}
