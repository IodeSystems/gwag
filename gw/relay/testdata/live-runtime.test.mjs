// Runs the generated runtime against a fake WebSocket. Driven by
// TestLiveRuntime, which supplies a stub react next to the runtime.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { setTimeout as sleep } from 'node:timers/promises'

class FakeSocket {
  static OPEN = 1
  static all = []
  readyState = 0
  sent = []
  closed = null
  constructor(url) {
    this.url = url
    FakeSocket.all.push(this)
  }
  send(s) {
    this.sent.push(JSON.parse(s))
  }
  close(code) {
    this.closed = code ?? 1000
    this.readyState = 3
  }
  open() {
    this.readyState = 1
    this.onopen()
  }
  deliver(frame) {
    this.onmessage({ data: JSON.stringify(frame) })
  }
  dropped() {
    this.readyState = 3
    this.onclose()
  }
}

globalThis.WebSocket = FakeSocket
globalThis.window = { location: { protocol: 'http:', host: 'test' } }

const { subscribeRaw, liveSnapshot, subscribeLive, setHeartbeat } = await import('./live-runtime.ts')

const latest = () => FakeSocket.all.at(-1)
const mint = (channel) => async () => ({ token: 'tok-' + channel, channel, exp: '' })
const until = async (cond) => {
  for (let i = 0; i < 500 && !cond(); i++) await sleep(2)
  assert.ok(cond(), 'condition not met')
}
const subs = (ws) => ws.sent.filter((f) => f.op === 'sub')

test('wire ids come from the channel, not local order', async () => {
  const got = []
  const stopA = subscribeRaw(mint('a'), (e) => got.push(['a', e]))
  const stopB1 = subscribeRaw(mint('b'), (e) => got.push(['b1', e]))
  const stopB2 = subscribeRaw(mint('b'), (e) => got.push(['b2', e]))
  const ws = latest()
  assert.equal(liveSnapshot().state, 'connecting')
  ws.open()
  await until(() => subs(ws).length === 3)
  assert.deepEqual(subs(ws).map((f) => f.id).sort(), ['a#0', 'b#0', 'b#1'])

  ws.deliver({ op: 'data', id: 'a#0', payload: 1 })
  ws.deliver({ op: 'data', id: 'b#1', payload: 2 })
  assert.equal(got.length, 2)
  assert.deepEqual(got[0], ['a', 1])

  stopB1()
  assert.ok(ws.sent.some((f) => f.op === 'unsub' && f.id.startsWith('b#')))
  ws.deliver({ op: 'pong', id: 'hb' })
  stopA()
  stopB2()
  assert.equal(ws.closed, 1000)
  assert.equal(liveSnapshot().state, 'idle')
})

test('reconnect keeps wire ids and counts', async () => {
  const before = liveSnapshot().reconnects
  const stop = subscribeRaw(mint('c'), () => {})
  const first = latest()
  first.open()
  await until(() => subs(first).length === 1)
  assert.equal(liveSnapshot().state, 'live')
  first.dropped()
  assert.equal(liveSnapshot().state, 'reconnecting')
  await until(() => latest() !== first)
  const second = latest()
  second.open()
  await until(() => subs(second).length === 1)
  assert.equal(subs(second)[0].id, subs(first)[0].id)
  assert.equal(liveSnapshot().state, 'live')
  assert.equal(liveSnapshot().reconnects, before + 1)
  stop()
})

test('heartbeat pings, and replaces a socket that stops answering', async () => {
  setHeartbeat(10, 15)
  const states = []
  const off = subscribeLive(() => states.push(liveSnapshot().state))
  const stop = subscribeRaw(mint('d'), () => {})
  const ws = latest()
  ws.open()
  await until(() => ws.sent.some((f) => f.op === 'ping'))

  // An answer of any kind keeps it alive past deadAfterMs.
  for (let i = 0; i < 5; i++) {
    await sleep(10)
    ws.deliver({ op: 'err', id: 'hb', err: 'unknown op' })
  }
  assert.equal(ws.closed, null)
  assert.ok(liveSnapshot().lastSeen !== null)

  await until(() => ws.closed !== null)
  assert.equal(ws.closed, 4000)
  assert.ok(states.includes('reconnecting'))
  await until(() => latest() !== ws)
  off()
  stop()
  setHeartbeat(20_000, 15_000)
})
