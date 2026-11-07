import type { ReactNode } from 'react';
import { Box, Breadcrumbs, Stack, Typography } from '@mui/material';

export interface PageHeaderProps {
  title: ReactNode;
  subtitle?: ReactNode;
  /** Small trail above the title, e.g. ["Services", "greeter"]. */
  breadcrumbs?: ReactNode[];
  /** Right-aligned buttons. */
  actions?: ReactNode;
  /** Inline next to the title (status chip, env badge). */
  meta?: ReactNode;
}

export function PageHeader({ title, subtitle, breadcrumbs, actions, meta }: PageHeaderProps) {
  return (
    <Stack
      direction={{ xs: 'column', sm: 'row' }}
      spacing={2}
      sx={{ alignItems: { sm: 'flex-end' }, justifyContent: 'space-between', mb: 3 }}
    >
      <Box sx={{ minWidth: 0 }}>
        {breadcrumbs && breadcrumbs.length > 0 && (
          <Breadcrumbs sx={{ mb: 0.5, fontSize: '0.75rem' }}>
            {breadcrumbs.map((b, i) => (
              <Typography key={i} variant="caption" color="text.secondary">
                {b}
              </Typography>
            ))}
          </Breadcrumbs>
        )}
        <Stack direction="row" spacing={1.5} sx={{ alignItems: 'center' }}>
          <Typography variant="h2" component="h1" noWrap>
            {title}
          </Typography>
          {meta}
        </Stack>
        {subtitle && (
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5, maxWidth: 720 }}>
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
  );
}

export interface PageProps extends PageHeaderProps {
  children: ReactNode;
  /** Cap content width (px). Default: full width. */
  maxWidth?: number;
}

/** Page frame: header + vertically stacked content. */
export function Page({ children, maxWidth, ...header }: PageProps) {
  return (
    <Box sx={{ maxWidth, mx: maxWidth ? 'auto' : undefined }}>
      <PageHeader {...header} />
      <Stack spacing={3}>{children}</Stack>
    </Box>
  );
}
