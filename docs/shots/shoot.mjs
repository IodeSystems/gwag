// Screenshot the gwag admin UI with headless Chrome over the DevTools protocol.
// Driven by bin/shots; no dependencies (Node 22+, google-chrome on PATH).
//
//   node docs/shots/shoot.mjs SPEC.json
//
// SPEC: { base, token, out, chromePort, scale, shots: [{ file, path,
//   preset, mode, token? , fill? }] }
//   token:  admin token placed in sessionStorage; false for a signed-out page
//   fill:   [{ selector, value }] typed into inputs before the capture
import { spawn } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const spec = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const { base, out, chromePort = 9334, scale = 1 } = spec;
mkdirSync(out, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const prof = mkdtempSync(join(tmpdir(), 'gwag-shots-'));
const chrome = spawn('google-chrome', [
  '--headless=new', `--remote-debugging-port=${chromePort}`, `--user-data-dir=${prof}`,
  '--window-size=1440,900', '--hide-scrollbars', '--no-first-run',
  '--no-default-browser-check', 'about:blank',
], { stdio: 'ignore' });
const exited = new Promise((r) => chrome.on('exit', r));
const cleanup = async () => {
  chrome.kill();
  await Promise.race([exited, sleep(5000)]);
  try { rmSync(prof, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 }); } catch {}
};

let tab;
for (let i = 0; i < 100 && !tab; i++) {
  try {
    const l = await (await fetch(`http://127.0.0.1:${chromePort}/json/list`)).json();
    tab = l.find((t) => t.type === 'page');
  } catch { await sleep(200); }
}
if (!tab) { console.error('chrome did not start'); await cleanup(); process.exit(1); }

const ws = new WebSocket(tab.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener('open', r));
let id = 0;
const pending = new Map();
ws.addEventListener('message', (e) => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
});
const send = (method, params = {}) => new Promise((resolve) => {
  const i = ++id;
  pending.set(i, resolve);
  ws.send(JSON.stringify({ id: i, method, params }));
  setTimeout(() => { if (pending.has(i)) { pending.delete(i); resolve({ timeout: true }); } }, 20000);
});
const evaluate = async (expression) =>
  (await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })).result?.result?.value;

await send('Page.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: scale, mobile: false });

// Ready: no progress indicator and the page text stops changing for 1.2 s.
async function settle(maxMs = 20000) {
  const t0 = Date.now();
  let last = '', stableSince = Date.now();
  while (Date.now() - t0 < maxMs) {
    const state = await evaluate(`JSON.stringify({
      busy: document.querySelectorAll('[role=progressbar], .MuiSkeleton-root').length,
      text: document.body.innerText.length })`);
    const s = state ? JSON.parse(state) : { busy: 1, text: 0 };
    const key = String(s.text);
    if (key !== last) { last = key; stableSince = Date.now(); }
    if (s.busy === 0 && s.text > 0 && Date.now() - stableSince > 1200) return true;
    await sleep(250);
  }
  return false;
}

let storedKey = null;
for (const s of spec.shots) {
  const token = s.token === false ? '' : (s.token ?? spec.token ?? '');
  const key = `${token ? 1 : 0}|${s.preset}|${s.mode}`;
  if (key !== storedKey) {
    await send('Page.navigate', { url: base + '/' });
    await sleep(1500);
    await evaluate(`(() => {
      sessionStorage.clear();
      ${token ? `sessionStorage.setItem('gwag:admin-token', ${JSON.stringify(token)});` : ''}
      localStorage.setItem('gwag.ui.themePreset', ${JSON.stringify(s.preset)});
      localStorage.setItem('gwag.ui.themeMode', ${JSON.stringify(s.mode)});
    })()`);
    storedKey = key;
  }
  await send('Page.navigate', { url: base + s.path });
  await sleep(1000);
  const ok = await settle();
  for (const f of s.fill ?? []) {
    await evaluate(`(() => {
      const el = document.querySelector(${JSON.stringify(f.selector)});
      const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      set.call(el, ${JSON.stringify(f.value)});
      el.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
  }
  if (s.fill) await sleep(500);
  if (s.after) { await sleep(s.after); }
  const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  if (shot.timeout || !shot.result) { console.log('FAILED', s.file); continue; }
  writeFileSync(join(out, s.file), Buffer.from(shot.result.data, 'base64'));
  console.log(ok ? 'ok    ' : 'unsettled', s.file);
}
ws.close();
await cleanup();
