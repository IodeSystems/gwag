import { useEffect, useRef, useState } from 'react';
import { Box, IconButton, Tooltip, Typography } from '@mui/material';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import CheckIcon from '@mui/icons-material/Check';

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Clipboard API needs a secure context; fall back to execCommand.
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  return (
    <Tooltip title={done ? 'Copied' : label}>
      <IconButton
        size="small"
        aria-label={done ? 'copied' : label.toLowerCase()}
        onClick={() => {
          void copyText(text).then((ok) => {
            if (!ok) return;
            setDone(true);
            clearTimeout(timer.current);
            timer.current = setTimeout(() => setDone(false), 1500);
          });
        }}
      >
        {done ? <CheckIcon fontSize="inherit" color="success" /> : <ContentCopyIcon fontSize="inherit" />}
      </IconButton>
    </Tooltip>
  );
}

export interface CodeBlockProps {
  code: string;
  /** Shown top-left, e.g. "graphql", "json", "bash". */
  language?: string;
  maxHeight?: number | string;
  /** Wrap long lines instead of scrolling horizontally. */
  wrap?: boolean;
}

export function CodeBlock({ code, language, maxHeight = 360, wrap }: CodeBlockProps) {
  return (
    <Box
      sx={(t) => ({
        position: 'relative',
        border: `1px solid ${t.gwag.tokens.border}`,
        borderRadius: `${t.gwag.radius.md}px`,
        bgcolor: t.gwag.tokens.surfaceSunken,
        overflow: 'hidden',
      })}
    >
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          px: 1.5,
          minHeight: 32,
          borderBottom: 1,
          borderColor: 'divider',
        }}
      >
        <Typography variant="overline" color="text.secondary">
          {language ?? 'text'}
        </Typography>
        <CopyButton text={code} />
      </Box>
      <Box
        component="pre"
        tabIndex={0}
        sx={{
          m: 0,
          p: 1.5,
          maxHeight,
          overflow: 'auto',
          fontFamily: (t) => t.gwag.mono,
          fontSize: '0.75rem',
          lineHeight: 1.6,
          whiteSpace: wrap ? 'pre-wrap' : 'pre',
          overflowWrap: wrap ? 'anywhere' : undefined,
        }}
      >
        <code>{code}</code>
      </Box>
    </Box>
  );
}
