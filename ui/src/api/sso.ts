// Browser session state from GET /auth/whoami: an SSO (OpenID
// Connect) session, or one redeemed from a sign-in code (gwag login
// --web). Without --sso-issuer the gateway still answers, with sso
// false; anything else (404, the SPA's index.html) reads as SSO off
// and no session.
//
// The session is an HttpOnly cookie the page cannot read, so whether
// one is live has to be asked. The cookie rides on every same-origin
// request (GraphQL, the WebSocket), so nothing here stores a token.

import { useQuery } from '@tanstack/react-query';

export interface SSOState {
  /** The gateway offers SSO sign-in. */
  enabled: boolean;
  /** This browser holds a valid session. */
  authenticated: boolean;
  email?: string;
  role?: 'admin' | 'dev';
  /** How the session was made: 'sso', or 'token' (gwag login --web). */
  via?: 'sso' | 'token';
  /** Why SSO cannot be used right now (e.g. cluster level too low). */
  error?: string;
}

const off: SSOState = { enabled: false, authenticated: false };

export async function fetchSSO(): Promise<SSOState> {
  try {
    const res = await fetch('/auth/whoami', { cache: 'no-store', credentials: 'same-origin' });
    if (!res.ok || !(res.headers.get('Content-Type') ?? '').includes('application/json')) return off;
    const d = (await res.json()) as Record<string, unknown>;
    return {
      enabled: d.sso === true,
      authenticated: d.authenticated === true,
      email: typeof d.email === 'string' ? d.email : undefined,
      role: d.role === 'admin' || d.role === 'dev' ? d.role : undefined,
      via: d.via === 'sso' || d.via === 'token' ? d.via : undefined,
      error: typeof d.error === 'string' ? d.error : undefined,
    };
  } catch {
    return off;
  }
}

export function useSSO(): SSOState {
  const { data } = useQuery({ queryKey: ['sso'], queryFn: fetchSSO, staleTime: 60_000, retry: false });
  return data ?? off;
}

/** ssoSignIn starts the provider flow; the gateway redirects back to /. */
export function ssoSignIn(): void {
  window.location.assign('/auth/login');
}

/**
 * ssoSignOut clears the session cookie (only the gateway can) and
 * reloads. Ends an SSO session and a gwag login --web session alike.
 */
export async function ssoSignOut(): Promise<void> {
  try {
    await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' });
  } finally {
    window.location.assign('/');
  }
}

// gwag login --web prints http://HOST/#code=CODE. The code is in the
// fragment so it never reaches a server or proxy log; take it off the
// address at once (history, a shared screen), then redeem it.
let pendingCode: string | null = null;

/**
 * captureSignInCode moves a #code=… fragment out of the address bar.
 * Call before the router reads the location.
 */
export function captureSignInCode(): void {
  const hash = window.location.hash.replace(/^#/, '');
  if (!hash) return;
  const params = new URLSearchParams(hash);
  const code = params.get('code');
  if (!code) return;
  params.delete('code');
  const rest = params.toString();
  window.history.replaceState(
    window.history.state,
    '',
    window.location.pathname + window.location.search + (rest ? '#' + rest : '')
  );
  pendingCode = code;
}

/** takeSignInCode returns the captured code once. */
export function takeSignInCode(): string | null {
  const c = pendingCode;
  pendingCode = null;
  return c;
}

export interface CodeSignInResult {
  subject: string;
  role: string;
}

/** redeemSignInCode trades the code for a session cookie. */
export async function redeemSignInCode(code: string): Promise<CodeSignInResult> {
  const res = await fetch('/auth/session', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  let d: Record<string, unknown> = {};
  try {
    d = (await res.json()) as Record<string, unknown>;
  } catch {
    // not JSON: an older gateway, or a proxy page
  }
  if (!res.ok) {
    const msg = typeof d.error === 'string' ? d.error : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return {
    subject: typeof d.subject === 'string' ? d.subject : '',
    role: typeof d.role === 'string' ? d.role : '',
  };
}
