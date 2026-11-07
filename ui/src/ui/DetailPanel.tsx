import type { ReactNode } from 'react';
import { Box, Divider, Drawer, IconButton, Stack, Typography } from '@mui/material';
import CloseIcon from '@mui/icons-material/Close';

export interface DetailPanelProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  /** Chip or badge next to the title. */
  meta?: ReactNode;
  /** Sticky footer: buttons. */
  actions?: ReactNode;
  width?: number;
  children: ReactNode;
}

/** Right-hand drawer for inspecting a row or entity. */
export function DetailPanel({
  open,
  onClose,
  title,
  subtitle,
  meta,
  actions,
  width = 480,
  children,
}: DetailPanelProps) {
  return (
    <Drawer
      anchor="right"
      open={open}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: { xs: '100%', sm: width }, maxWidth: '100%' } } }}
    >
      <Stack direction="row" sx={{ p: 2, gap: 1, alignItems: 'flex-start' }}>
        <Box sx={{ flexGrow: 1, minWidth: 0 }}>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
            <Typography variant="h3" noWrap>
              {title}
            </Typography>
            {meta}
          </Stack>
          {subtitle && (
            <Typography variant="body2" color="text.secondary">
              {subtitle}
            </Typography>
          )}
        </Box>
        <IconButton aria-label="close" onClick={onClose} size="small">
          <CloseIcon fontSize="small" />
        </IconButton>
      </Stack>
      <Divider />
      <Box sx={{ p: 2, flexGrow: 1, overflow: 'auto' }}>{children}</Box>
      {actions && (
        <>
          <Divider />
          <Stack direction="row" spacing={1} sx={{ p: 2, justifyContent: 'flex-end' }}>
            {actions}
          </Stack>
        </>
      )}
    </Drawer>
  );
}
