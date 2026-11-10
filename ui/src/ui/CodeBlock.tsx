import { useEffect, useMemo, useRef, useState } from 'react';
import { Box, IconButton, Tooltip, Typography } from '@mui/material';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import CheckIcon from '@mui/icons-material/Check';
import type { Theme } from '@mui/material/styles';
import hljs from 'highlight.js/lib/core';
import bash from 'highlight.js/lib/languages/bash';
import json from 'highlight.js/lib/languages/json';
import yaml from 'highlight.js/lib/languages/yaml';
import { graphql, protobuf } from './hljsLangs';

hljs.registerLanguage('bash', bash);
hljs.registerLanguage('graphql', graphql);
hljs.registerLanguage('json', json);
hljs.registerLanguage('protobuf', protobuf);
hljs.registerLanguage('yaml', yaml);

/** Map a source-kind label to a registered hljs language; undefined = plain. */
const aliases: Record<string, string> = {
  graphql: 'graphql',
  gql: 'graphql',
  sdl: 'graphql',
  proto: 'protobuf',
  protobuf: 'protobuf',
  json: 'json',
  openapi: 'json',
  yaml: 'yaml',
  yml: 'yaml',
  bash: 'bash',
  sh: 'bash',
};

/** Above this size highlighting is skipped so large SDL cannot freeze the page. */
export const HIGHLIGHT_MAX_BYTES = 200 * 1024;

/** Token colours come from the active theme, so every preset and mode is covered. */
function tokenStyles(t: Theme) {
  const { status, tokens } = t.gwag;
  // Distinct hues per token class. Graphite has no accent colour, so hues
  // come from the status palette (all >= 4.5:1 on the sunken surface).
  const c = {
    keyword: status.draining.fg,
    title: status.degraded.fg,
    type: status.waiting.fg,
    string: status.ready.fg,
    number: tokens.error,
    meta: status.sunsetable.fg,
    comment: tokens.textMuted,
  };
  return {
    '& .hljs-keyword, & .hljs-selector-tag': { color: c.keyword, fontWeight: 600 },
    '& .hljs-title': { color: c.title },
    '& .hljs-type': { color: c.type },
    '& .hljs-built_in': { color: c.type, fontStyle: 'italic' },
    '& .hljs-string, & .hljs-regexp, & .hljs-symbol': { color: c.string },
    '& .hljs-number': { color: c.number },
    '& .hljs-literal, & .hljs-bullet': { color: c.meta },
    '& .hljs-attr, & .hljs-attribute, & .hljs-variable, & .hljs-params, & .hljs-property': {
      color: tokens.text,
    },
    '& .hljs-meta, & .hljs-doctag, & .hljs-section, & .hljs-name': { color: c.meta },
    '& .hljs-comment, & .hljs-quote': { color: c.comment, fontStyle: 'italic' },
    '& .hljs-punctuation, & .hljs-operator': { color: c.comment },
    '& .hljs-deletion': { color: tokens.error },
    '& .hljs-emphasis': { fontStyle: 'italic' },
    '& .hljs-strong': { fontWeight: 700 },
  };
}

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
  /** Drop the frame and header; render just the highlighted code. */
  bare?: boolean;
}

export function CodeBlock({ code, language, maxHeight = 360, wrap, bare }: CodeBlockProps) {
  const html = useMemo(() => {
    const lang = language ? aliases[language.toLowerCase()] : undefined;
    if (!lang || code.length > HIGHLIGHT_MAX_BYTES) return undefined;
    try {
      return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value;
    } catch {
      return undefined;
    }
  }, [code, language]);
  const codeEl =
    html === undefined ? (
      <code>{code}</code>
    ) : (
      <code dangerouslySetInnerHTML={{ __html: html }} />
    );
  if (bare) {
    return (
      <Box
        component="pre"
        sx={(t) => ({
          m: 0,
          fontFamily: t.gwag.mono,
          fontSize: 'inherit',
          whiteSpace: wrap ? 'pre-wrap' : 'pre',
          overflowWrap: wrap ? 'anywhere' : undefined,
          ...tokenStyles(t),
        })}
      >
        {codeEl}
      </Box>
    );
  }
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
        sx={(t) => ({
          ...tokenStyles(t),
          m: 0,
          p: 1.5,
          maxHeight,
          overflow: 'auto',
          fontFamily: t.gwag.mono,
          fontSize: '0.75rem',
          lineHeight: 1.6,
          whiteSpace: wrap ? 'pre-wrap' : 'pre',
          overflowWrap: wrap ? 'anywhere' : undefined,
        })}
      >
        {codeEl}
      </Box>
    </Box>
  );
}
