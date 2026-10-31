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

const { subscribeRaw, liveSnapshot, subscribeLive, setHeartbeat, setMintRetry } = await import(
  './live-runtime.ts',
)

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

test('a failed mint retries with backoff until it works', async () => {
  setMintRetry(4, 16)
  let calls = 0
  const states = []
  const flaky = async () => {
    if (++calls <= 3) throw new Error('denied')
    return { token: 't', channel: 'e', exp: '' }
  }
  const stop = subscribeRaw(flaky, () => {}, (s) => states.push(s))
  const ws = latest()
  ws.open()
  await until(() => subs(ws).length === 1)
  assert.equal(calls, 4)
  assert.deepEqual(states[0], { status: 'failed', error: 'denied' })
  assert.equal(states.length, 1, 'same failure repeats no state change')
  ws.deliver({ op: 'ack', id: subs(ws)[0].id })
  assert.deepEqual(states.at(-1), { status: 'subscribed', error: null })
  stop()
})

test('a relay err marks the sub failed and backs off on a resolved mint', async () => {
  setMintRetry(20, 40)
  let calls = 0
  const states = []
  const counting = async () => {
    calls++
    return { token: 'bad', channel: 'f', exp: '' }
  }
  const stop = subscribeRaw(counting, () => {}, (s) => states.push(s))
  const ws = latest()
  ws.open()
  await until(() => subs(ws).length === 1)
  const id = subs(ws)[0].id
  ws.deliver({ op: 'err', id, err: 'relay: expired token' })
  assert.deepEqual(states.at(-1), { status: 'failed', error: 'relay: expired token' })
  assert.equal(calls, 1, 're-mint must wait for the backoff')
  await until(() => subs(ws).length === 2)
  ws.deliver({ op: 'ack', id })
  assert.equal(states.at(-1).status, 'subscribed')

  // No timer outlives the sub.
  ws.deliver({ op: 'err', id, err: 'again' })
  const before = calls
  stop()
  await sleep(60)
  assert.equal(calls, before)
})

test('a lost socket returns subs to pending and cancels the retry', async () => {
  setMintRetry(5, 5)
  const states = []
  const stop = subscribeRaw(mint('g'), () => {}, (s) => states.push(s.status))
  const first = latest()
  first.open()
  await until(() => subs(first).length === 1)
  first.deliver({ op: 'ack', id: subs(first)[0].id })
  first.deliver({ op: 'err', id: subs(first)[0].id, err: 'x' })
  first.dropped()
  assert.deepEqual(states, ['subscribed', 'failed', 'pending'])
  await until(() => latest() !== first)
  const second = latest()
  await sleep(20)
  assert.equal(subs(first).length, 1, 'retry fired on a dead socket')
  second.open()
  await until(() => subs(second).length === 1)
  second.deliver({ op: 'ack', id: subs(second)[0].id })
  assert.equal(states.at(-1), 'subscribed')
  stop()
  setMintRetry(2_000, 60_000)
})
