import type { ReactElement } from 'react';
import { Tooltip } from '@mui/material';
import { useRoleGate, type AdminRole } from '@/api/role';

interface RoleGateProps {
  /** Role the control needs. */
  need: AdminRole;
  /** Renders the control; `allowed` false = render it disabled. */
  children: (allowed: boolean) => ReactElement;
}

/**
 * RoleGate wraps a control the current role may not be able to use:
 * disabled, with a tooltip naming the token it needs.
 */
export function RoleGate({ need, children }: RoleGateProps) {
  const { allowed, tip } = useRoleGate(need);
  return (
    <Tooltip title={tip}>
      <span>{children(allowed)}</span>
    </Tooltip>
  );
}
