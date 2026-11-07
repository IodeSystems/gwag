// Admin role of the stored bearer token. The gateway knows three:
//
//   anonymous  no token: the graph, status and services reads, public
//              services only (no owner contact, no external dependents)
//   dev        the dev token: every read, plus external-dependent edits
//   admin      the admin token: everything
//
// The role comes from admin.whoami, so the UI shows what the gateway
// decides (including a pluggable AdminAuthorizer), not a guess from
// the token's shape. A token the gateway rejects reads as `invalid`.

import { useSyncExternalStore } from 'react';
import { useQuery } from '@tanstack/react-query';
import { getAdminToken, onAdminTokenChange } from './auth';
import { client } from './client';
import { WhoamiQuery } from './operations';

export type AdminRole = 'anonymous' | 'dev' | 'admin';

const rank: Record<AdminRole, number> = { anonymous: 0, dev: 1, admin: 2 };

export interface AdminRoleState {
  role: AdminRole;
  /** A token is stored but the gateway rejected it. */
  invalid: boolean;
  loading: boolean;
}

export function useAdminRole(): AdminRoleState {
  const token = useSyncExternalStore(onAdminTokenChange, getAdminToken, () => null);
  const { data, error, isLoading } = useQuery({
    queryKey: ['whoami', token],
    queryFn: () => client.request(WhoamiQuery),
    staleTime: Infinity,
    retry: false,
  });
  const role = data?.admin?.whoami?.role ?? 'anonymous';
  return { role: error ? 'anonymous' : role, invalid: token !== null && error !== null, loading: isLoading };
}

/** Whether role may use a control that needs `need`. */
export function roleAllows(role: AdminRole, need: AdminRole): boolean {
  return rank[role] >= rank[need];
}

/** Tooltip for a control the role cannot use; '' when it can. */
export function roleTip(role: AdminRole, need: AdminRole): string {
  if (roleAllows(role, need)) return '';
  return need === 'admin' ? 'Needs the admin token' : 'Sign in with the dev or admin token';
}

/** useRoleGate is roleAllows + roleTip for the current role. */
export function useRoleGate(need: AdminRole): { allowed: boolean; tip: string } {
  const { role } = useAdminRole();
  return { allowed: roleAllows(role, need), tip: roleTip(role, need) };
}
