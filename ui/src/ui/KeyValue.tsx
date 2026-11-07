import type { ReactNode } from 'react';
import { Box, Typography } from '@mui/material';
import { CopyButton } from './CodeBlock';

export interface KeyValueItem {
  label: ReactNode;
  value: ReactNode;
  /** Monospace value (ids, addresses). */
  mono?: boolean;
  /** Show a copy button; value must be a string. */
  copyable?: boolean;
}

export interface KeyValueProps {
  items: KeyValueItem[];
  /** Label column width in px. */
  labelWidth?: number;
}

// Height of a row holding a copy button, so plain rows match it.
const ROW_HEIGHT = 28;

/** Definition-list style label/value pairs. */
export function KeyValue({ items, labelWidth = 160 }: KeyValueProps) {
  return (
    <Box
      component="dl"
      sx={{
        m: 0,
        display: 'grid',
        gridTemplateColumns: { xs: '1fr', sm: `${labelWidth}px 1fr` },
        columnGap: 2,
        rowGap: { xs: 0, sm: 1 },
      }}
    >
      {items.map((it, i) => (
        <Box key={i} sx={{ display: 'contents' }}>
          <Typography
            component="dt"
            variant="body2"
            color="text.secondary"
            sx={{ pt: { xs: 1, sm: 0 }, minHeight: ROW_HEIGHT, display: 'flex', alignItems: 'center' }}
          >
            {it.label}
          </Typography>
          <Typography
            component="dd"
            variant="body2"
            sx={{
              m: 0,
              minWidth: 0,
              overflowWrap: 'anywhere',
              display: 'flex',
              alignItems: 'center',
              minHeight: ROW_HEIGHT,
              gap: 0.5,
              fontFamily: it.mono ? (t) => t.gwag.mono : undefined,
              fontSize: it.mono ? '0.75rem' : undefined,
            }}
          >
            {it.value}
            {it.copyable && typeof it.value === 'string' && <CopyButton text={it.value} />}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}
