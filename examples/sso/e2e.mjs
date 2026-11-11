// Browser + CLI sign-in against the running example (run via ./e2e.sh).
// Headless google-chrome over CDP, 127.0.0.1 only. Node 22+.
import { spawn } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync, rmSync, readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const GW = 'http://127.0.0.1:28500';
const OTHER = 'http://127.0.0.1:28599'; // a second origin, served here
const RUN = process.env.SSO_RUN;
const GWAG = process.env.GWAG_BIN;
const IDP = process.env.IDP || 'dex';
const PHASE = process.env.SSO_PHASE || 'main';
const KC = 'http://127.0.0.1:28557';
const PASS = IDP === 'keycloak' ? Object.fromEntries( // fixtures: the realm file is the only copy
  JSON.parse(readFileSync(new URL('./keycloak-realm.json', import.meta.url), 'utf8')).users
    .map((u) => [u.email, u.credentials[0].value])) : { // fixtures, same as dex.yaml
  'admin@example.test': 'admin-pass-7f3k',
  'dev@example.test': 'dev-pass-2q9m',
  'stranger@other.test': 'stranger-pass-5x1v',
};
mkdirSync(RUN, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failed = 0;
const check = (name, ok, extra = '') => {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok ? '' : '  ' + extra}`);
  if (!ok) failed++;
};

// --- minimal CDP: one browser socket, flat sessions ---
const port = 9345;
const prof = mkdtempSync(join(tmpdir(), 'sso-e2e-'));
const chrome = spawn('google-chrome', ['--headless=new', `--remote-debugging-port=${port}`,
  `--user-data-dir=${prof}`, '--no-first-run', '--no-sandbox', '--hide-scrollbars',
  '--window-size=1280,800', 'about:blank'], { stdio: 'ignore' });
let ver;
for (let i = 0; i < 60 && !ver; i++) {
  try { ver = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json(); } catch { await sleep(250); }
}
if (!ver) { console.error('chrome did not start'); process.exit(2); }
const ws = new WebSocket(ver.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener('open', r));
let seq = 0; const pend = new Map(); const listeners = new Map();
ws.addEventListener('message', (e) => {
  const m = JSON.parse(e.data);
  if (m.sessionId && listeners.has(m.sessionId)) listeners.get(m.sessionId)(m);
  if (m.id && pend.has(m.id)) { const [res, rej] = pend.get(m.id); pend.delete(m.id); m.error ? rej(new Error(m.error.message)) : res(m.result); }
});
const send = (method, params = {}, sessionId) => new Promise((res, rej) => {
  const id = ++seq; pend.set(id, [res, rej]);
  ws.send(JSON.stringify({ id, method, params, sessionId }));
});

// A page in a fresh browser context = own cookie jar.
async function newPage() {
  const { browserContextId } = await send('Target.createBrowserContext');
  const { targetId } = await send('Target.createTarget', { url: 'about:blank', browserContextId });
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
  const call = (m, p) => send(m, p, sessionId);
  const responses = [];
  listeners.set(sessionId, (m) => { if (m.method === 'Network.responseReceived') responses.push({ url: m.params.response.url, status: m.params.response.status }); });
  await call('Page.enable');
  await call('Network.enable');
  await call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false });
  return {
    responses,
    goto: async (url) => { await call('Page.navigate', { url }); await sleep(600); },
    eval: async (expression) => {
      const r = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.text + ' ' + (r.exceptionDetails.exception?.description ?? ''));
      return r.result.value;
    },
    url: () => call('Runtime.evaluate', { expression: 'location.href', returnByValue: true }).then((r) => r.result.value),
    until: async (expr, what, ms = 20000) => {
      const t = Date.now();
      for (;;) {
        try { const v = await call('Runtime.evaluate', { expression: expr, returnByValue: true }); if (v.result.value) return v.result.value; } catch {}
        if (Date.now() - t > ms) throw new Error('timeout waiting for ' + what);
        await sleep(200);
      }
    },
    shot: async (name) => {
      const s = await call('Page.captureScreenshot', { format: 'png' });
      writeFileSync(join(RUN, name + '.png'), Buffer.from(s.data, 'base64'));
    },
    close: () => send('Target.closeTarget', { targetId }),
  };
}

const api = (path, method = 'GET', body) => `fetch(${JSON.stringify(GW + path)}, {method:${JSON.stringify(method)}, credentials:'same-origin'${body ? `, headers:{'content-type':'application/json'}, body:${JSON.stringify(JSON.stringify(body))}` : ''}}).then(async r => ({status:r.status, text: await r.text()}))`;
const status = (p, path, method, body) => p.eval(api(path, method, body)).then((r) => r.status);

// Dex form -> back at the gateway. Returns once off dex.
async function dexLogin(p, email) {
  await p.until(`!!document.querySelector('input[name=login]')`, 'dex login form');
  await p.eval(`document.querySelector('input[name=login]').value=${JSON.stringify(email)};document.querySelector('input[name=password]').value=${JSON.stringify(PASS[email])};`);
  return p;
}
const submit = (p) => p.eval(`document.querySelector('button[type=submit]').click()`);

const ADMIN_OP = ['/api/admin/mcp/auto-include', 'POST', { autoInclude: false }];
const GQL = ['/api/graphql', 'POST', { query: 'mutation { admin { mcpSetAutoInclude(body: {autoInclude: false}) { autoInclude } } }' }];
const DEV_OP = ['/api/admin/notifications/no-such-id/dismiss', 'POST']; // dev or admin; 404 = allowed, no such item

async function dexSuite() {
  // ---- browser: admin ----
  let p = await newPage();
  await p.goto(GW + '/');
  await p.until(`!!document.querySelector('[aria-label="sign in with SSO"]')`, 'SSO button');
  await sleep(500);
  await p.shot('1-login');
  check('login screen shows Sign in with SSO', true);
  await p.eval(`document.querySelector('[aria-label="sign in with SSO"]').click()`);
  await dexLogin(p, 'admin@example.test');
  await p.shot('2-dex-form');
  await submit(p);
  await p.until(`location.origin==='${GW}' && !!document.querySelector('[aria-label="sign out"]')`, 'signed-in header');
  let chip = true;
  await p.until(`/\\bAdmin\\b/.test(document.querySelector('header')?.innerText ?? '')`, 'Admin role chip', 8000).catch(() => { chip = false; });
  await sleep(1500);
  await p.shot('3-signed-in');
  check('UI header role chip reads Admin (admin.whoami over the cookie)', chip, (await p.eval(`document.querySelector('header')?.innerText`)));
  const who = JSON.parse((await p.eval(api('/auth/whoami'))).text);
  check('whoami: admin@example.test, admin, sso', who.authenticated && who.email === 'admin@example.test' && who.role === 'admin' && who.via === 'sso', JSON.stringify(who));
  check('header shows the email', (await p.eval('document.body.innerText')).includes('admin@example.test'));
  const aw = JSON.parse((await p.eval(api('/api/admin/whoami'))).text);
  check('/api/admin/whoami role admin', aw.role === 'admin', JSON.stringify(aw));
  let s = await status(p, ...ADMIN_OP);
  check('admin mutation same-origin from UI page', s === 200, 'HTTP ' + s);
  let g = JSON.parse((await p.eval(api(...GQL))).text);
  check('admin mutation through GraphQL (cookie + Sec-Fetch-Site forwarded)', !g.errors && g.data?.admin?.mcpSetAutoInclude?.autoInclude === false, JSON.stringify(g));
  s = await status(p, ...DEV_OP);
  check('dev op as admin passes authz (404 no such item)', s === 404, 'HTTP ' + s);

  // cross-origin page (other port: same site, different origin; the
  // cookie goes along). A no-body POST is a simple request, so no
  // preflight; its status is read from CDP network events.
  const srv = (await import('node:http')).createServer((q, r) => { r.setHeader('content-type', 'text/html'); r.end('<html><body>other origin</body></html>'); });
  await new Promise((r) => srv.listen(28599, '127.0.0.1', r));
  await p.goto(OTHER + '/');
  p.responses.length = 0;
  await p.eval(`fetch(${JSON.stringify(GW + DEV_OP[0])}, {method:'POST', credentials:'include', mode:'no-cors'}).then(()=>1,()=>0)`);
  await sleep(800);
  const xs = p.responses.find((u) => u.url.endsWith(DEV_OP[0]));
  check('cross-origin POST with the session cookie refused (403)', xs?.status === 403, JSON.stringify(xs));
  const xg = await p.eval(`fetch(${JSON.stringify(GW + '/auth/whoami')}, {credentials:'include', mode:'no-cors'}).then(()=>1,()=>0)`);
  void xg;
  srv.close();
  await p.goto(GW + '/');
  await p.until(`!!document.querySelector('[aria-label="sign out"]')`, 'header back');

  // logout
  await p.eval(`document.querySelector('[aria-label="sign out"]').click()`);
  await p.until(`!!document.querySelector('[aria-label="sign in with SSO"]')`, 'signed-out UI');
  const w2 = JSON.parse((await p.eval(api('/auth/whoami'))).text);
  check('logout: whoami unauthenticated', w2.sso && !w2.authenticated, JSON.stringify(w2));
  s = await status(p, ...ADMIN_OP);
  check('after logout admin mutation refused', s === 401 || s === 403, 'HTTP ' + s);
  await p.close();

  // ---- browser: dev ----
  p = await newPage();
  await p.goto(GW + '/auth/login');
  await dexLogin(p, 'dev@example.test'); await submit(p);
  await p.until(`location.origin==='${GW}'`, 'back at gateway');
  await p.goto(GW + '/');
  await p.until(`!!document.querySelector('[aria-label="sign out"]')`, 'dev header');
  const dw = JSON.parse((await p.eval(api('/auth/whoami'))).text);
  check('dev: whoami role dev', dw.email === 'dev@example.test' && dw.role === 'dev', JSON.stringify(dw));
  s = await status(p, ...ADMIN_OP);
  check('dev: admin-only op refused', s === 403, 'HTTP ' + s);
  const dg = JSON.parse((await p.eval(api(...GQL))).text);
  check('dev: admin mutation through GraphQL refused', !!dg.errors && !/dial|lookup/.test(JSON.stringify(dg)) && dg.data?.admin?.mcpSetAutoInclude == null, JSON.stringify(dg));
  s = await status(p, ...DEV_OP);
  check('dev: dev op allowed', s === 404, 'HTTP ' + s);
  await p.close();

  // ---- browser: unmatched ----
  p = await newPage();
  await p.goto(GW + '/auth/login');
  await dexLogin(p, 'stranger@other.test'); await submit(p);
  await p.until(`location.origin==='${GW}' && location.pathname.startsWith('/auth/')`, 'denied page');
  await sleep(300);
  await p.shot('4-denied');
  const body = await p.eval('document.body.innerText');
  const ws3 = JSON.parse((await p.eval(api('/auth/whoami'))).text);
  check('unmatched user: denied page, no session', !ws3.authenticated && /denied|not allowed|no role|refused|rule/i.test(body), body.slice(0, 120));
  await p.close();

  await cliSuite('admin@example.test', async (p) => { await dexLogin(p, 'admin@example.test'); await submit(p); });
}

async function cliSuite(email, loginFn) {
  let p;
  const cli = join(RUN, 'cli'); rmSync(cli, { recursive: true, force: true }); mkdirSync(cli, { recursive: true });
  const env = { ...process.env, XDG_CONFIG_HOME: join(cli, 'xdg'), HOME: cli };
  const run = (args) => new Promise((res) => {
    const c = spawn(GWAG, args, { cwd: cli, env }); let out = '';
    c.stdout.on('data', (d) => (out += d)); c.stderr.on('data', (d) => (out += d));
    c.on('close', (code) => res({ code, out }));
  });
  const login = spawn(GWAG, ['login', '--sso', '--no-browser', '--name', 'sso-e2e', '--gateway', '127.0.0.1:28501', '--endpoint', GW], { cwd: cli, env });
  let lout = ''; login.stdout.on('data', (d) => (lout += d)); login.stderr.on('data', (d) => (lout += d));
  const done = new Promise((r) => login.on('close', (code) => r(code)));
  let u; for (let i = 0; i < 50 && !u; i++) { u = lout.match(/https?:\/\/127\.0\.0\.1:28500\/auth\/cli\S+/)?.[0]; await sleep(200); }
  check('CLI printed sign-in URL', !!u, lout);
  p = await newPage();
  await p.goto(u);
  await loginFn(p);
  const code = await Promise.race([done, sleep(30000).then(() => 'timeout')]);
  check('CLI login exits 0', code === 0, lout);
  check('CLI prints email and role', lout.includes(email) && /admin/.test(lout), lout);
  await p.shot('5-cli-done').catch(() => {});
  await p.close();
  const credPath = [join(cli, '.gw/credentials.json')].find(existsSync);
  const creds = credPath ? readFileSync(credPath, 'utf8') : '';
  check('credentials hold a gwo_ token', /"gwo_[^"]+/.test(creds), credPath ?? 'no credentials file');
  let r = await run(['peer', 'list']);
  check('gwag peer list (dev or admin) works with the bearer', r.code === 0, r.out.slice(0, 200));
  const tok = creds.match(/gwo_[^"]+/)?.[0];
  r = await run(['logout']);
  check('gwag logout exits 0', r.code === 0, r.out);
  const gone = await fetch(GW + '/api/admin/peers', { headers: { authorization: 'Bearer ' + tok } });
  check('GET /api/admin/peers with the ended bearer refused', gone.status === 401 || gone.status === 403, 'HTTP ' + gone.status);
  // the ended bearer: replay it directly
  const res = await fetch(GW + ADMIN_OP[0], { method: 'POST', headers: { authorization: 'Bearer ' + tok, 'content-type': 'application/json' }, body: '{"autoInclude":false}' });
  check('ended bearer refused', res.status === 401 || res.status === 403, 'HTTP ' + res.status);
}

// ---- Keycloak ----
const U = (n) => n + '@corp.test';
async function kcLogin(p, email) {
  await p.until(`!!document.querySelector('#username')`, 'keycloak login form');
  await p.eval(`document.querySelector('#username').value=${JSON.stringify(email)};document.querySelector('#password').value=${JSON.stringify(PASS[email])};`);
  await p.eval(`document.querySelector('#kc-login').click()`);
}
const whoami = async (p) => JSON.parse((await p.eval(api('/auth/whoami'))).text);
// Fresh context, sign in through the gateway, land back on it.
async function signIn(email) {
  const p = await newPage();
  await p.goto(GW + '/auth/login');
  await kcLogin(p, email);
  await p.until(`location.origin==='${GW}'`, 'back at gateway');
  await sleep(500);
  return p;
}
async function kcAdminToken() {
  const r = await fetch(KC + '/realms/master/protocol/openid-connect/token', { method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'password', client_id: 'admin-cli', username: process.env.KC_ADMIN_USER, password: process.env.KC_ADMIN_PASS }) });
  if (!r.ok) throw new Error('keycloak admin token: HTTP ' + r.status);
  return (await r.json()).access_token;
}
async function kcRemoveFromGroup(email, group) {
  const h = { authorization: 'Bearer ' + await kcAdminToken() };
  const users = await (await fetch(`${KC}/admin/realms/gwag/users?email=${encodeURIComponent(email)}&exact=true`, { headers: h })).json();
  const groups = await (await fetch(`${KC}/admin/realms/gwag/groups?search=${group}&exact=true`, { headers: h })).json();
  if (users.length !== 1 || !groups.length) throw new Error('user or group not found');
  const r = await fetch(`${KC}/admin/realms/gwag/users/${users[0].id}/groups/${groups[0].id}`, { method: 'DELETE', headers: h });
  return r.status;
}
// Role as the gateway sees it: whoami, then an admin op and a dev op.
async function roleChecks(p, name, email, role) {
  const w = await whoami(p);
  check(`${name}: whoami ${email}, role ${role}, via sso`, w.authenticated && w.email === email && w.role === role && w.via === 'sso', JSON.stringify(w));
  const s = await status(p, ...ADMIN_OP);
  check(`${name}: admin op ${role === 'admin' ? 'allowed' : 'refused'}`, s === (role === 'admin' ? 200 : 403), 'HTTP ' + s);
  const d = await status(p, ...DEV_OP);
  check(`${name}: dev op allowed`, d === 404, 'HTTP ' + d);
}
async function denied(name, email, shot) {
  const p = await newPage();
  await p.goto(GW + '/auth/login');
  await kcLogin(p, email);
  await p.until(`location.origin==='${GW}' && location.pathname.startsWith('/auth/')`, 'denied page');
  await sleep(300);
  if (shot) await p.shot(shot);
  const body = await p.eval('document.body.innerText');
  const w = await whoami(p);
  check(`${name}: denied page, no session`, !w.authenticated && /denied|not allowed|no role|refused|rule/i.test(body), body.slice(0, 120));
  await p.close();
}

async function kcSuite() {
  // admin-group user, through the UI
  let p = await newPage();
  await p.goto(GW + '/');
  await p.until(`!!document.querySelector('[aria-label="sign in with SSO"]')`, 'SSO button');
  await p.eval(`document.querySelector('[aria-label="sign in with SSO"]').click()`);
  await p.until(`!!document.querySelector('#username')`, 'keycloak form');
  await sleep(300);
  await p.shot('kc-1-form');
  await kcLogin(p, U('grp-admin'));
  await p.until(`location.origin==='${GW}' && !!document.querySelector('[aria-label="sign out"]')`, 'signed-in header');
  let chip = true;
  await p.until(`/\\bAdmin\\b/.test(document.querySelector('header')?.innerText ?? '')`, 'Admin chip', 8000).catch(() => { chip = false; });
  await sleep(1000);
  await p.shot('kc-2-admin');
  check('gwag-admins member: UI role chip reads Admin', chip, await p.eval(`document.querySelector('header')?.innerText`));
  await roleChecks(p, 'gwag-admins', U('grp-admin'), 'admin');
  const g = JSON.parse((await p.eval(api(...GQL))).text);
  check('gwag-admins: admin mutation through GraphQL', !g.errors && g.data?.admin?.mcpSetAutoInclude?.autoInclude === false, JSON.stringify(g));
  await p.close();

  p = await signIn(U('grp-dev'));
  await roleChecks(p, 'gwag-devs', U('grp-dev'), 'dev');
  const dg = JSON.parse((await p.eval(api(...GQL))).text);
  check('gwag-devs: admin mutation through GraphQL refused', !!dg.errors && dg.data?.admin?.mcpSetAutoInclude == null, JSON.stringify(dg));
  await p.shot('kc-3-dev');
  await p.close();

  p = await signIn(U('grp-both'));
  await roleChecks(p, 'both groups (admin rule first)', U('grp-both'), 'admin');
  await p.close();

  await denied('no group', U('grp-none'), 'kc-4-denied');

  // group change: remove from gwag-admins while signed in
  p = await signIn(U('grp-demote'));
  await roleChecks(p, 'demote user before change', U('grp-demote'), 'admin');
  const st = await kcRemoveFromGroup(U('grp-demote'), 'gwag-admins');
  check('keycloak admin REST: removed from gwag-admins (204)', st === 204, 'HTTP ' + st);
  const w = await whoami(p);
  const s = await status(p, ...ADMIN_OP);
  console.log(`INFO  after removal, same cookie: role=${w.role}, admin op HTTP ${s}`);
  check('documented: groups live in the cookie, role stays admin until re-sign-in', w.role === 'admin' && s === 200, JSON.stringify(w) + ' HTTP ' + s);
  await p.eval(api('/auth/logout', 'POST'));
  await p.goto(GW + '/auth/login'); // Keycloak's own session is still open: no form
  await p.until(`location.origin==='${GW}' && !location.pathname.startsWith('/auth/login')`, 'back at gateway', 30000).catch(() => {});
  if (await p.eval(`!!document.querySelector('#username')`)) await kcLogin(p, U('grp-demote'));
  await p.until(`location.origin==='${GW}'`, 'back at gateway');
  await sleep(500);
  await roleChecks(p, 'demote user after re-sign-in (now only gwag-devs)', U('grp-demote'), 'dev');
  await p.close();

  await cliSuite(U('grp-admin'), async (p) => { await kcLogin(p, U('grp-admin')); });
}

async function kcClaimSuite() {
  // gateway runs with --sso-groups-claim roles_groups, rules on full paths
  // (/gwag-admins), dev rule first. Only the roles_groups mapper emits paths.
  let p = await signIn(U('grp-admin'));
  await roleChecks(p, 'claim roles_groups: admin group', U('grp-admin'), 'admin');
  await p.close();
  p = await signIn(U('grp-dev'));
  await roleChecks(p, 'claim roles_groups: dev group', U('grp-dev'), 'dev');
  await p.close();
  p = await signIn(U('grp-both'));
  await roleChecks(p, 'claim roles_groups, dev rule first: both groups -> dev', U('grp-both'), 'dev');
  await p.close();
  await denied('claim roles_groups: no group', U('grp-none'));
}

try {
  await (IDP === 'keycloak' ? (PHASE === 'claim' ? kcClaimSuite() : kcSuite()) : dexSuite());
} catch (e) {
  console.log('FAIL  exception: ' + (e.stack || e));
  failed++;
} finally {
  try { ws.close(); } catch {}
  chrome.kill();
  await sleep(1000);
  try { rmSync(prof, { recursive: true, force: true, maxRetries: 5, retryDelay: 300 }); } catch {}
}
console.log(failed ? `${failed} FAILED` : 'ALL PASSED');
process.exit(failed ? 1 : 0);
