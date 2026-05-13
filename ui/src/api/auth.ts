// Admin bearer-token store for the gateway UI. The gateway logs an
// `admin token = <hex>` line at boot; the operator pastes that token
// here so admin_* mutations (Forget, future Drain, sign, etc.) carry
// the right Authorization header.
//
// Storage policy: sessionStorage only. This is a UI for ops, not an
// app the user lives in — pinning the token to a tab keeps it out of
// localStorage and means closing the browser drops it. Operators who
// want it across reloads still benefit because sessionStorage survives
// page refresh; full logout = close tab.
//
// Subscribers are notified via a window-level CustomEvent so the
// Authorization header in graphql-request's lazy headers callback
// always reads the current value (the callback fires per request).

const KEY = 'gwag:admin-token';
const EVENT = 'gwag:admin-token-changed';

/**
 * Returns the currently-stored token, or null if unset / SSR.
 */
export function getAdminToken(): string | null {
  if (typeof window === 'undefined') return null;
  const v = window.sessionStorage.getItem(KEY);
  return v && v.length > 0 ? v : null;
}

/**
 * Persists the token (or clears when value is null/empty) and notifies
 * subscribers. Token strings are stored verbatim — typically the hex
 * the gateway logged at boot.
 */
export function setAdminToken(value: string | null): void {
  if (typeof window === 'undefined') return;
  if (value && value.length > 0) {
    window.sessionStorage.setItem(KEY, value);
  } else {
    window.sessionStorage.removeItem(KEY);
  }
  window.dispatchEvent(new CustomEvent(EVENT));
}

/**
 * Subscribe to token-changed events. Returns an unsubscribe function.
 * Used by the React hook below; rarely called directly.
 */
export function onAdminTokenChange(cb: () => void): () => void {
  if (typeof window === 'undefined') return () => {};
  const handler = () => cb();
  window.addEventListener(EVENT, handler);
  // Cross-tab sync: storage events fire when another tab writes, but
  // sessionStorage is per-tab so we only catch our own writes via the
  // CustomEvent above. Listening for `storage` here would be a no-op
  // for sessionStorage but is harmless if we ever switch to local.
  return () => window.removeEventListener(EVENT, handler);
}
