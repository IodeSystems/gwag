// In-process Backend for tests. Same semantics as events/memory: each
// group gets every message published after the group's first
// subscriber (or every stored message with deliverAll); subscribers
// sharing a group share its queue; failed deliveries come back after
// the backoff; after maxDeliver failures the message goes to
// deadLetters(). Message ids dedup within 2 minutes. ackWaitMs is not
// modelled.
import {
  delayMs,
  dlqSubject,
  HeaderError,
  type Backend,
  type Channel,
  type Handler,
  type Message,
  type ResolvedSubscribeOptions,
  type Stop,
} from "./events.ts";

/** How long a message id is remembered. */
export const dedupWindowMs = 2 * 60 * 1000;

interface Item {
  m: Message;
  delivery: number;
}

class Group {
  queue: Item[] = [];
  waiters: Array<() => void> = [];

  push(it: Item): void {
    this.queue.push(it);
    this.waiters.shift()?.();
  }
}

interface Stored {
  log: Message[];
  groups: Map<string, Group>;
}

function clone(m: Message): Message {
  return { ...m, headers: { ...m.headers }, data: m.data.slice() };
}

export class MemoryBackend implements Backend {
  #closed = false;
  readonly #channels = new Map<string, Stored>();
  readonly #seen = new Map<string, number>();
  readonly #dlq: Message[] = [];
  readonly #timers = new Set<NodeJS.Timeout>();
  readonly #wake = new Set<() => void>();

  async publish(ch: Channel, m: Message): Promise<void> {
    if (this.#closed) throw new Error("memory backend: closed");
    const now = Date.now();
    if (m.id !== "") {
      const k = `${ch.stream}\u0000${m.id}`;
      const t = this.#seen.get(k);
      if (t !== undefined && now - t < dedupWindowMs) return;
      this.#seen.set(k, now);
    }
    const stored = clone(m);
    const c = this.#channel(ch);
    c.log.push(stored);
    for (const g of c.groups.values()) g.push({ m: stored, delivery: 1 });
  }

  #channel(ch: Channel): Stored {
    let c = this.#channels.get(ch.stream);
    if (!c) {
      c = { log: [], groups: new Map() };
      this.#channels.set(ch.stream, c);
    }
    return c;
  }

  async subscribe(ch: Channel, groupName: string, opts: ResolvedSubscribeOptions, h: Handler): Promise<Stop> {
    if (this.#closed) throw new Error("memory backend: closed");
    const c = this.#channel(ch);
    let g = c.groups.get(groupName);
    if (!g) {
      g = new Group();
      c.groups.set(groupName, g);
      if (opts.deliverAll) for (const m of c.log) g.push({ m, delivery: 1 });
    }
    let stopped = false;
    let wakeSelf: (() => void) | undefined;
    const group = g;
    void this.#work(ch, group, opts, h, () => stopped, (w) => (wakeSelf = w)).finally(() => {
      // A wake meant for this subscriber may have been for an item it left.
      if (group.queue.length > 0) group.waiters.shift()?.();
    });
    return async () => {
      stopped = true;
      wakeSelf?.();
    };
  }

  async #work(
    ch: Channel,
    g: Group,
    opts: ResolvedSubscribeOptions,
    h: Handler,
    stopped: () => boolean,
    setWake: (w: () => void) => void,
  ): Promise<void> {
    for (;;) {
      if (this.#closed || stopped()) return;
      const it = g.queue.shift();
      if (!it) {
        await new Promise<void>((resolve) => {
          const w = () => {
            g.waiters = g.waiters.filter((x) => x !== w);
            this.#wake.delete(w);
            resolve();
          };
          g.waiters.push(w);
          this.#wake.add(w);
          setWake(w);
        });
        continue;
      }
      if (stopped()) {
        // Stopped while holding an item: put it back for the group.
        g.queue.unshift(it);
        return;
      }
      const m = clone(it.m);
      m.delivery = it.delivery;
      try {
        await h(m);
        continue;
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        if (it.delivery >= opts.maxDeliver) {
          const dead = clone(it.m);
          dead.subject = dlqSubject(ch.stream);
          dead.headers[HeaderError] = msg;
          dead.delivery = it.delivery;
          this.#dlq.push(dead);
          continue;
        }
        const next: Item = { m: it.m, delivery: it.delivery + 1 };
        const t = setTimeout(() => {
          this.#timers.delete(t);
          if (!this.#closed) g.push(next);
        }, delayMs(opts, it.delivery));
        this.#timers.add(t);
      }
    }
  }

  /** The dead-lettered messages, oldest first. Each has subject dlqSubject(stream) and HeaderError. */
  deadLetters(): Message[] {
    return this.#dlq.map(clone);
  }

  async close(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    for (const t of this.#timers) clearTimeout(t);
    this.#timers.clear();
    for (const w of [...this.#wake]) w();
  }
}

export function memory(): MemoryBackend {
  return new MemoryBackend();
}
