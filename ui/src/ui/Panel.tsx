import type { ReactNode } from 'react';
import { Box, Paper, Stack, Typography } from '@mui/material';

export interface PanelProps {
  title?: ReactNode;
  subtitle?: ReactNode;
  /** Header-right slot (buttons, filters). */
  actions?: ReactNode;
  children?: ReactNode;
  /** Remove body padding, e.g. when the body is a DataTable. */
  flush?: boolean;
  footer?: ReactNode;
}

export function Panel({ title, subtitle, actions, children, flush, footer }: PanelProps) {
  return (
    <Paper variant="outlined" sx={{ overflow: 'hidden' }}>
      {(title || actions) && (
        <Stack
          direction="row"
          sx={{
            px: 2,
            py: 1.5,
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 2,
            borderBottom: 1,
            borderColor: 'divider',
          }}
        >
          <Box sx={{ minWidth: 0 }}>
            {title && (
              <Typography variant="h4" component="h2" noWrap>
                {title}
              </Typography>
            )}
            {subtitle && (
              <Typography variant="caption" color="text.secondary">
                {subtitle}
              </Typography>
            )}
          </Box>
          {actions && (
            <Stack direction="row" spacing={1} sx={{ flexShrink: 0 }}>
              {actions}
            </Stack>
          )}
        </Stack>
      )}
      {children !== undefined && <Box sx={{ p: flush ? 0 : 2 }}>{children}</Box>}
      {footer && (
        <Box
          sx={{
            px: 2,
            py: 1,
            borderTop: 1,
            borderColor: 'divider',
            bgcolor: (t) => t.gwag.tokens.surfaceSunken,
          }}
        >
          {footer}
        </Box>
      )}
    </Paper>
  );
}
