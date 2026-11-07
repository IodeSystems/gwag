import type { ReactElement } from 'react';
import { Box } from '@mui/material';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import HourglassTopIcon from '@mui/icons-material/HourglassTop';
import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import ArrowCircleDownIcon from '@mui/icons-material/ArrowCircleDown';
import WbTwilightIcon from '@mui/icons-material/WbTwilight';
import type { StatusKey } from './theme';

const meta: Record<StatusKey, { label: string; icon: ReactElement }> = {
  ready: { label: 'Ready', icon: <CheckCircleIcon /> },
  waiting: { label: 'Waiting', icon: <HourglassTopIcon /> },
  degraded: { label: 'Degraded', icon: <WarningAmberIcon /> },
  draining: { label: 'Draining', icon: <ArrowCircleDownIcon /> },
  sunsetable: { label: 'Sunset-able', icon: <WbTwilightIcon /> },
};

export const statusKeys = Object.keys(meta) as StatusKey[];

export interface StatusChipProps {
  status: StatusKey;
  /** Override the default label. The icon still identifies the state. */
  label?: string;
  size?: 'small' | 'medium';
}

/** State chip. Each state has its own hue, icon and text label. */
export function StatusChip({ status, label, size = 'medium' }: StatusChipProps) {
  const m = meta[status];
  const small = size === 'small';
  return (
    <Box
      component="span"
      data-status={status}
      sx={(t) => {
        const c = t.gwag.status[status];
        return {
          display: 'inline-flex',
          alignItems: 'center',
          gap: 0.5,
          height: small ? 20 : 24,
          px: small ? 0.75 : 1,
          borderRadius: `${t.gwag.radius.sm}px`,
          border: `1px solid ${c.border}`,
          bgcolor: c.bg,
          color: c.fg,
          fontSize: small ? '0.6875rem' : '0.75rem',
          fontWeight: 600,
          lineHeight: 1,
          whiteSpace: 'nowrap',
          '& svg': { fontSize: small ? 13 : 15 },
        };
      }}
    >
      {m.icon}
      {label ?? m.label}
    </Box>
  );
}
