// WCAG contrast check for every theme preset.
// Run: node ui/scripts/contrast.ts   (Node >= 22.6 strips the TS types)
// Exits non-zero (throws) if any pair is below 4.5:1.
import { presetList } from '../src/ui/theme/presets.ts';

type RGB = [number, number, number];
type RGBA = [number, number, number, number];

const hex = (h: string): RGB => {
  const n = h.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(n.slice(i, i + 2), 16)) as RGB;
};
const parse = (c: string): RGBA => {
  if (c.startsWith('#')) {
    const [r, g, b] = hex(c);
    return [r, g, b, 1];
  }
  let m = c.match(/rgba?\(([^)]+)\)/);
  if (m) {
    const p = (m[1] ?? '').split(',').map((x) => parseFloat(x));
    return [p[0] ?? 0, p[1] ?? 0, p[2] ?? 0, p[3] ?? 1];
  }
  m = c.match(/hsl\(([\d.]+) ([\d.]+)% ([\d.]+)%\)/);
  if (m) {
    const [h, s, l] = [Number(m[1]), Number(m[2]) / 100, Number(m[3]) / 100];
    const k = (n: number) => (n + h / 30) % 12;
    const a = s * Math.min(l, 1 - l);
    const f = (n: number) => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
    return [f(0) * 255, f(8) * 255, f(4) * 255, 1];
  }
  throw new Error(`cannot parse ${c}`);
};
const over = (fg: RGBA, bg: RGB): RGB => {
  const [r, g, b, a] = fg;
  return [r * a + bg[0] * (1 - a), g * a + bg[1] * (1 - a), b * a + bg[2] * (1 - a)];
};
const lum = ([r, g, b]: RGB): number => {
  const f = (v: number) => {
    v /= 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
};
const ratio = (a: RGB, b: RGB): number => {
  const la = lum(a);
  const lb = lum(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
};
const solid = (c: string, under: RGB = [0, 0, 0]): RGB => {
  const p = parse(c);
  return p[3] < 1 ? over(p, under) : [p[0], p[1], p[2]];
};

const fixed: Record<string, number> = { dev: 160, staging: 40, prod: 5, qa: 280 };
let fail = false;
const out: Record<string, string | number>[] = [];
for (const p of presetList) {
  for (const mode of ['dark', 'light'] as const) {
    const t = p.modes[mode];
    let chipMin = Infinity;
    let chipWhich = '';
    for (const [k, s] of Object.entries(p.status[mode])) {
      for (const under of ['bg', 'surface', 'surfaceSunken', 'surfaceRaised'] as const) {
        const base = solid(t[under]);
        const bg = over(parse(s.bg), base);
        const r = ratio(solid(s.fg, bg), bg);
        if (r < chipMin) {
          chipMin = r;
          chipWhich = `${k}@${under}`;
        }
      }
    }
    const surf = solid(t.surface);
    const bgc = solid(t.bg);
    const checks: Record<string, number> = {
      text: ratio(solid(t.text), surf),
      muted: Math.min(ratio(solid(t.textMuted), surf), ratio(solid(t.textMuted), bgc), ratio(solid(t.textMuted), solid(t.surfaceSunken))),
      onPrimary: ratio(solid(t.primaryContrast), solid(t.primary)),
      primary: ratio(solid(t.primary), surf),
      error: ratio(solid(t.error), surf),
      env: Math.min(
        ...['dev', 'staging', 'prod', 'qa', 'eu-west-1'].map((n) => {
          let acc = 0;
          for (let i = 0; i < n.length; i++) acc = (acc * 31 + n.charCodeAt(i)) >>> 0;
          const h = acc % 360;
          const hue = fixed[n] ?? h;
          const e = parse(`hsl(${hue} ${p.env[mode].s}% ${p.env[mode].l}%)`);
          const rgb: RGB = [e[0], e[1], e[2]];
          return Math.min(ratio(rgb, surf), ratio(rgb, bgc));
        }),
      ),
    };
    const row = { preset: p.id, mode, chipMin: +chipMin.toFixed(2), chipWhich, ...Object.fromEntries(Object.entries(checks).map(([k, v]) => [k, +v.toFixed(2)])) };
    out.push(row);
    const gates: [string, number][] = [
      ['chip', chipMin],
      ['text', checks.text ?? 0],
      ['muted', checks.muted ?? 0],
      ['onPrimary', checks.onPrimary ?? 0],
      ['error', checks.error ?? 0],
      ['env', checks.env ?? 0],
    ];
    for (const [k, v] of gates) {
      if (v < 4.5) {
        fail = true;
        console.log(`FAIL ${p.id}/${mode} ${k} ${v.toFixed(2)}${k === 'chip' ? ' ' + chipWhich : ''}`);
      }
    }
  }
}
console.table(out);
if (fail) throw new Error("contrast check failed");
