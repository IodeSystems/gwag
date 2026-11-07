// Turns the admin dependencyGraph response into React Flow nodes and
// edges, with a layered left-to-right layout: callers on the left,
// providers on the right, one column per dependency depth. Layout is
// per namespace (a group node), with its lines stacked inside.

import { MarkerType, type Edge, type Node } from '@xyflow/react';
import type { DependencyGraphQuery } from '@/gql/graphql';

export type Graph = NonNullable<DependencyGraphQuery['admin']['dependencyGraph']>;
export type GraphLine = NonNullable<Graph['nodes']>[number];
export type DesiredEdge = NonNullable<Graph['desiredEdges']>[number];
export type ActiveEdge = NonNullable<Graph['activeEdges']>[number];

export type EdgeView = 'desired' | 'active' | 'both';

export interface EdgeColors {
  met: string;
  unmet: string;
  active: string;
  muted: string;
  labelBg: string;
  labelText: string;
}

export type GroupData = {
  namespace: string;
  team: string | null;
  missing: boolean;
  [k: string]: unknown;
};
export type LineData = { line: GraphLine; [k: string]: unknown };
export type CallerData = { caller: string; count: number; [k: string]: unknown };

export const GROUP_W = 300;
const LINE_W = GROUP_W - 16;
export const LINE_H = 96;
const LINE_GAP = 8;
const GROUP_HEADER = 44;
const MISSING_H = 44;
const COL_GAP = 140;
const ROW_GAP = 32;
const CALLER_W = 180;
const CALLER_H = 56;

export const groupId = (ns: string) => `ns:${ns}`;
export const lineId = (ns: string, ver: string) => `line:${ns}/${ver}`;
const callerId = (c: string) => `caller:${c}`;

function groupHeight(lines: number): number {
  if (lines === 0) return GROUP_HEADER + MISSING_H;
  return GROUP_HEADER + lines * (LINE_H + LINE_GAP);
}

// The gateway's own namespaces: the self-ingested admin API, admin
// events and pub/sub. Hidden by default; the UI's own polling would
// otherwise show up as traffic.
const gatewayNamespaces = new Set(['admin', 'admin_events', 'ps']);

export function isGatewayLine(l: GraphLine): boolean {
  return gatewayNamespaces.has(l.namespace) || l.kind === 'internalproto';
}

/** Drops the gateway's own lines and the edges that touch them. */
export function withoutGatewayLines(g: Graph): Graph {
  const hidden = new Set((g.nodes ?? []).filter(isGatewayLine).map((l) => l.namespace));
  if (hidden.size === 0) return g;
  return {
    ...g,
    nodes: (g.nodes ?? []).filter((l) => !hidden.has(l.namespace)),
    desiredEdges: (g.desiredEdges ?? []).filter(
      (e) => !hidden.has(e.provider) && !hidden.has(e.consumerNamespace),
    ),
    activeEdges: (g.activeEdges ?? []).filter(
      (e) => !hidden.has(e.namespace) && !hidden.has(e.consumerNamespace),
    ),
  };
}

export function buildFlow(
  g: Graph,
  view: EdgeView,
  colors: EdgeColors,
): { nodes: Node[]; edges: Edge[] } {
  const lines = g.nodes ?? [];
  const desired = view === 'active' ? [] : (g.desiredEdges ?? []);
  const active = view === 'desired' ? [] : (g.activeEdges ?? []);

  // Namespaces: registered ones, plus providers named in depends that
  // nobody registered (drawn as "not registered").
  const byNs = new Map<string, GraphLine[]>();
  for (const l of lines) {
    const list = byNs.get(l.namespace) ?? [];
    list.push(l);
    byNs.set(l.namespace, list);
  }
  const missing = new Set<string>();
  for (const e of desired) {
    if (!byNs.has(e.provider)) missing.add(e.provider);
  }
  const namespaces = [...byNs.keys(), ...missing].sort();
  const lineSet = new Set(lines.map((l) => lineId(l.namespace, l.version)));

  // External callers (caller id not mapped to a registered namespace).
  const callers = new Map<string, number>();
  for (const e of active) {
    if (e.consumerNamespace === '') {
      callers.set(e.caller, (callers.get(e.caller) ?? 0) + Number(e.count));
    }
  }

  // Layer assignment: longest path from the callers, by namespace.
  const vertices = [...[...callers.keys()].map(callerId), ...namespaces.map(groupId)];
  const adj: [string, string][] = [];
  for (const e of desired) {
    if (e.consumerNamespace !== e.provider) adj.push([groupId(e.consumerNamespace), groupId(e.provider)]);
  }
  for (const e of active) {
    const from = e.consumerNamespace === '' ? callerId(e.caller) : groupId(e.consumerNamespace);
    if (from !== groupId(e.namespace)) adj.push([from, groupId(e.namespace)]);
  }
  const layer = new Map<string, number>(vertices.map((v) => [v, 0]));
  for (const c of callers.keys()) layer.set(callerId(c), -1);
  // Bounded relaxation: active edges can form cycles.
  for (let i = 0; i < vertices.length; i++) {
    let changed = false;
    for (const [a, b] of adj) {
      const next = (layer.get(a) ?? 0) + 1;
      if (next > (layer.get(b) ?? 0)) {
        layer.set(b, next);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const minLayer = Math.min(0, ...layer.values());
  const columns = new Map<number, string[]>();
  for (const v of vertices) {
    const l = (layer.get(v) ?? 0) - minLayer;
    columns.set(l, [...(columns.get(l) ?? []), v]);
  }

  const heightOf = (v: string) =>
    v.startsWith('caller:') ? CALLER_H : groupHeight(byNs.get(v.slice(3))?.length ?? 0);
  const widthOf = (v: string) => (v.startsWith('caller:') ? CALLER_W : GROUP_W);

  // Order each column by the mean row of its predecessors (one pass,
  // left to right) to cut down crossings; ties by name.
  const row = new Map<string, number>();
  const colKeys = [...columns.keys()].sort((a, b) => a - b);
  for (const c of colKeys) {
    const col = columns.get(c)!;
    const bary = (v: string) => {
      const preds = adj.filter(([, b]) => b === v).map(([a]) => row.get(a));
      const known = preds.filter((p): p is number => p !== undefined);
      return known.length ? known.reduce((s, x) => s + x, 0) / known.length : Number.MAX_SAFE_INTEGER;
    };
    col.sort((a, b) => bary(a) - bary(b) || a.localeCompare(b));
    col.forEach((v, i) => row.set(v, i));
  }

  // Positions: columns left to right, each column centred vertically.
  const colHeight = (col: string[]) =>
    col.reduce((s, v) => s + heightOf(v), 0) + ROW_GAP * Math.max(0, col.length - 1);
  const tallest = Math.max(0, ...colKeys.map((c) => colHeight(columns.get(c)!)));
  const pos = new Map<string, { x: number; y: number }>();
  let x = 0;
  for (const c of colKeys) {
    const col = columns.get(c)!;
    let y = (tallest - colHeight(col)) / 2;
    const w = Math.max(...col.map(widthOf));
    for (const v of col) {
      pos.set(v, { x: x + (w - widthOf(v)) / 2, y });
      y += heightOf(v) + ROW_GAP;
    }
    x += w + COL_GAP;
  }

  const nodes: Node[] = [];
  for (const [c, count] of callers) {
    nodes.push({
      id: callerId(c),
      type: 'caller',
      position: pos.get(callerId(c))!,
      data: { caller: c, count } satisfies CallerData,
      style: { width: CALLER_W, height: CALLER_H },
      selectable: false,
    });
  }
  for (const ns of namespaces) {
    const ls = (byNs.get(ns) ?? []).slice().sort((a, b) => a.version.localeCompare(b.version, undefined, { numeric: true }));
    nodes.push({
      id: groupId(ns),
      type: 'nsGroup',
      position: pos.get(groupId(ns))!,
      data: {
        namespace: ns,
        team: ls.find((l) => l.owner?.team)?.owner?.team ?? null,
        missing: missing.has(ns),
      } satisfies GroupData,
      style: { width: GROUP_W, height: groupHeight(ls.length) },
      selectable: false,
    });
    ls.forEach((l, i) => {
      nodes.push({
        id: lineId(l.namespace, l.version),
        type: 'line',
        parentId: groupId(ns),
        extent: 'parent',
        position: { x: 8, y: GROUP_HEADER + i * (LINE_H + LINE_GAP) },
        data: { line: l } satisfies LineData,
        style: { width: LINE_W, height: LINE_H },
      });
    });
  }

  const edges: Edge[] = [];
  const label = {
    labelStyle: { fill: colors.labelText, fontSize: 11, fontWeight: 600 },
    labelBgStyle: { fill: colors.labelBg },
    labelBgPadding: [4, 2] as [number, number],
    labelBgBorderRadius: 3,
  };
  // Desired: consumer line → each accepted line that exists (present
  // ones in the met colour, others muted); unmet → error colour, to
  // the accepted lines or the provider group when none exists.
  for (const e of desired) {
    const source = lineSet.has(lineId(e.consumerNamespace, e.consumerVersion))
      ? lineId(e.consumerNamespace, e.consumerVersion)
      : groupId(e.consumerNamespace);
    const present = new Set(e.presentLines ?? []);
    const targets = (e.lines ?? []).filter((l) => lineSet.has(lineId(e.provider, l)));
    const base = `d:${e.consumerNamespace}/${e.consumerVersion}->${e.provider}[${(e.lines ?? []).join(',')}]`;
    if (targets.length === 0) {
      edges.push({
        id: base,
        source,
        target: groupId(e.provider),
        style: { stroke: colors.unmet, strokeWidth: 2 },
        markerEnd: { type: MarkerType.ArrowClosed, color: colors.unmet },
        label: 'unmet',
        ...label,
      });
      continue;
    }
    for (const t of targets) {
      const color = !e.met ? colors.unmet : present.has(t) ? colors.met : colors.muted;
      edges.push({
        id: `${base}:${t}`,
        source,
        target: lineId(e.provider, t),
        style: { stroke: color, strokeWidth: present.has(t) ? 2 : 1.25 },
        markerEnd: { type: MarkerType.ArrowClosed, color },
        ...(e.met ? {} : { label: 'unmet', ...label }),
      });
    }
  }
  // Active: caller (line, namespace or external) → called line, dashed.
  for (const e of active) {
    const target = lineId(e.namespace, e.version);
    if (!lineSet.has(target)) continue;
    let source: string;
    if (e.consumerNamespace === '') source = callerId(e.caller);
    else if (e.consumerVersion && lineSet.has(lineId(e.consumerNamespace, e.consumerVersion)))
      source = lineId(e.consumerNamespace, e.consumerVersion);
    else source = groupId(e.consumerNamespace);
    edges.push({
      id: `a:${e.caller}->${e.namespace}/${e.version}`,
      source,
      target,
      animated: true,
      style: { stroke: colors.active, strokeWidth: 1.75, strokeDasharray: '6 4' },
      markerEnd: { type: MarkerType.ArrowClosed, color: colors.active },
      label: formatCount(Number(e.count)),
      ...label,
    });
  }
  return { nodes, edges };
}

export function formatCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}
