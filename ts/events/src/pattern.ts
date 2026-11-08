// Channel patterns. Same grammar and behaviour as events/pattern.go:
// NATS subject tokens, "*" one token, a trailing ">" the rest, and
// "{name}" one token with a name.

export type SegmentKind = "named" | "positional" | "rest";

export interface Segment {
  /** "orderId" as written; "P1".. for "*"; "Rest" for ">". */
  name: string;
  kind: SegmentKind;
}

export class Pattern {
  readonly raw: string;
  /** Literal token, or "" for a wildcard. */
  readonly #tokens: string[];
  readonly #segs: Segment[];

  constructor(raw: string, tokens: string[], segs: Segment[]) {
    this.raw = raw;
    this.#tokens = tokens;
    this.#segs = segs;
  }

  toString(): string {
    return this.raw;
  }

  segments(): Segment[] {
    return [...this.#segs];
  }

  /** The NATS subject filter: "{name}" becomes "*". */
  wildcard(): string {
    let si = 0;
    return this.#tokens
      .map((t) => {
        if (t !== "") return t;
        const seg = this.#segs[si++]!;
        return seg.kind === "rest" ? ">" : "*";
      })
      .join(".");
  }

  /** Fills the wildcard segments, in order, and returns the subject. */
  subject(values: readonly string[]): string {
    if (values.length !== this.#segs.length) {
      throw new Error(`pattern "${this.raw}": ${values.length} segment values, want ${this.#segs.length}`);
    }
    let si = 0;
    return this.#tokens
      .map((t) => {
        if (t !== "") return t;
        const seg = this.#segs[si]!;
        const v = values[si]!;
        si++;
        if (seg.kind === "rest") {
          if (!v.split(".").every(validToken)) {
            throw new Error(`pattern "${this.raw}": segment ${seg.name} = "${v}" is not a subject tail`);
          }
        } else if (!validToken(v)) {
          throw new Error(`pattern "${this.raw}": segment ${seg.name} = "${v}" is not a subject token`);
        }
        return v;
      })
      .join(".");
  }

  /** The wildcard segment values of subject, or undefined when it does not match. */
  match(subject: string): string[] | undefined {
    const parts = subject.split(".");
    const vals: string[] = [];
    let si = 0;
    for (let i = 0; i < this.#tokens.length; i++) {
      const t = this.#tokens[i]!;
      if (i >= parts.length) return undefined;
      if (t !== "") {
        if (parts[i] !== t) return undefined;
        continue;
      }
      if (this.#segs[si]!.kind === "rest") {
        vals.push(parts.slice(i).join("."));
        return vals;
      }
      vals.push(parts[i]!);
      si++;
    }
    return parts.length === this.#tokens.length ? vals : undefined;
  }
}

function validToken(t: string): boolean {
  return t !== "" && !/[.*> \t\r\n]/.test(t);
}

function isIdent(s: string): boolean {
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(s);
}

/** Parses and validates a channel pattern. Throws on a bad one. */
export function parsePattern(p: string): Pattern {
  if (p === "") throw new Error("pattern is empty");
  const parts = p.split(".");
  const tokens: string[] = [];
  const segs: Segment[] = [];
  const seen = new Set<string>();
  let positional = 0;
  parts.forEach((t, i) => {
    if (t === "") throw new Error(`pattern "${p}": empty token`);
    if (t === "*") {
      positional++;
      segs.push({ name: `P${positional}`, kind: "positional" });
      tokens.push("");
    } else if (t === ">") {
      if (i !== parts.length - 1) throw new Error(`pattern "${p}": ">" must be the last token`);
      segs.push({ name: "Rest", kind: "rest" });
      tokens.push("");
    } else if (t.startsWith("{") && t.endsWith("}")) {
      const name = t.slice(1, -1);
      if (!isIdent(name)) throw new Error(`pattern "${p}": segment name "${name}" is not an identifier`);
      if (seen.has(name)) throw new Error(`pattern "${p}": segment "${name}" repeats`);
      seen.add(name);
      segs.push({ name, kind: "named" });
      tokens.push("");
    } else {
      if (/[*>{} \t\r\n]/.test(t)) throw new Error(`pattern "${p}": bad token "${t}"`);
      tokens.push(t);
    }
  });
  return new Pattern(p, tokens, segs);
}
