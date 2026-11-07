import type { ReactNode } from 'react';
import { Box, Typography } from '@mui/material';
import InboxOutlinedIcon from '@mui/icons-material/InboxOutlined';

export interface EmptyStateProps {
  title: ReactNode;
  description?: ReactNode;
  icon?: ReactNode;
  /** Call to action, usually a Button. */
  action?: ReactNode;
  /** Tighter padding for use inside tables and small panels. */
  compact?: boolean;
}

export function EmptyState({ title, description, icon, action, compact }: EmptyStateProps) {
  return (
    <Box
      role="status"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        textAlign: 'center',
        gap: 1,
        py: compact ? 3 : 7,
        px: 2,
        color: 'text.secondary',
      }}
    >
      <Box
        sx={(t) => ({
          display: 'grid',
          placeItems: 'center',
          width: compact ? 40 : 52,
          height: compact ? 40 : 52,
          borderRadius: '50%',
          bgcolor: t.gwag.tokens.surfaceSunken,
          border: `1px solid ${t.gwag.tokens.border}`,
          '& svg': { fontSize: compact ? 22 : 28 },
        })}
      >
        {icon ?? <InboxOutlinedIcon />}
      </Box>
      <Typography variant="h4" color="text.primary">
        {title}
      </Typography>
      {description && (
        <Typography variant="body2" sx={{ maxWidth: 420 }}>
          {description}
        </Typography>
      )}
      {action && <Box sx={{ mt: 1 }}>{action}</Box>}
    </Box>
  );
}
