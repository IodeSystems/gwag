// Browser SSO (OpenID Connect) state from GET /auth/whoami. The
// gateway serves it only when started with --sso-issuer; anything
// else (404, the SPA's index.html) reads as SSO off.
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

/** ssoSignOut clears the session cookie (only the gateway can) and reloads. */
export async function ssoSignOut(): Promise<void> {
  try {
    await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' });
  } finally {
    window.location.assign('/');
  }
}
