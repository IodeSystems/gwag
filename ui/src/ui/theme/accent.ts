import { alpha } from '@mui/material/styles';
import type { CSSObject } from '@mui/material/styles';
import type { AccentStyle } from './presets';

/** Environment-accent treatment for the app bar (a box-shadow value). */
export function appBarAccent(style: AccentStyle, accent: string, surface: string): string {
  switch (style) {
    case 'glow':
      return `inset 0 3px 0 ${accent}`;
    case 'rule':
      return `inset 0 -2px 0 ${accent}`;
    case 'double':
      return `inset 0 2px 0 ${accent}, inset 0 4px 0 ${surface}, inset 0 5px 0 ${accent}`;
    case 'hairline':
      return `inset 0 -1px 0 ${accent}`;
    case 'outline':
      return `inset 0 0 0 1px ${accent}`;
    case 'none':
      return 'none';
  }
}

/** Environment-accent treatment for framed cards (a style object). */
export function accentFrame(style: AccentStyle, accent: string, surface: string): CSSObject {
  switch (style) {
    case 'glow':
      return {
        borderColor: alpha(accent, 0.5),
        boxShadow: `inset 0 3px 0 ${accent}, 0 0 28px ${alpha(accent, 0.22)}`,
      };
    case 'rule':
      return { boxShadow: `inset 3px 0 0 ${accent}` };
    case 'double':
      return {
        boxShadow: `inset 0 2px 0 ${accent}, inset 0 4px 0 ${surface}, inset 0 5px 0 ${accent}`,
      };
    case 'hairline':
      return { boxShadow: `inset 0 0 0 1px ${alpha(accent, 0.55)}` };
    case 'outline':
      return { boxShadow: `inset 0 0 0 1px ${accent}` };
    case 'none':
      return {};
  }
}
