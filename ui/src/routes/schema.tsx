import { createFileRoute } from '@tanstack/react-router';
import {
  Box,
  Button,
  FormControl,
  FormControlLabel,
  Radio,
  RadioGroup,
  Typography,
} from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';

export const Route = createFileRoute('/schema')({
  component: Schema,
});

type Format = 'graphql' | 'openapi' | 'proto';

interface FormatSpec {
  label: string;
  url: string;
  contentType: 'text' | 'json' | 'binary';
  filename: string;
}

const formats: Record<Format, FormatSpec> = {
  graphql: {
    label: 'GraphQL SDL',
    url: '/api/schema/graphql',
    contentType: 'text',
    filename: 'schema.graphql',
  },
  openapi: {
    label: 'OpenAPI',
    url: '/api/schema/openapi',
    contentType: 'json',
    filename: 'schema.openapi.json',
  },
  proto: {
    label: 'Protobuf FDS',
    url: '/api/schema/proto',
    contentType: 'binary',
    filename: 'schema.fds',
  },
};

function Schema() {
  const [format, setFormat] = useState<Format>('graphql');
  const spec = formats[format];

  const { data, isLoading, error } = useQuery({
    queryKey: ['schema', format],
    queryFn: async (): Promise<{ text: string; bytes: Blob }> => {
      const r = await fetch(spec.url);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const bytes = await r.blob();
      let text: string;
      if (spec.contentType === 'binary') {
        // FileDescriptorSet bytes don't render as text. Show size +
        // a download CTA below; the textarea body stays empty for
        // this format.
        text = `(binary FileDescriptorSet — ${bytes.size} bytes)`;
      } else if (spec.contentType === 'json') {
        try {
          text = JSON.stringify(JSON.parse(await bytes.text()), null, 2);
        } catch {
          text = await bytes.text();
        }
      } else {
        text = await bytes.text();
      }
      return { text, bytes };
    },
  });

  const downloadCurrent = () => {
    if (!data) return;
    const url = URL.createObjectURL(data.bytes);
    const a = document.createElement('a');
    a.href = url;
    a.download = spec.filename;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    // Bleed past Layout's p:3 main padding and fill the viewport
    // below the AppBar (64px). Two-row flex column: format selector
    // on top, scrollable monospace pane below.
    <Box
      sx={{
        m: -3,
        height: 'calc(100vh - 64px)',
        display: 'flex',
        flexDirection: 'column',
      }}
    >
      <Box
        sx={{
          px: 2,
          py: 1,
          borderBottom: 1,
          borderColor: 'divider',
          display: 'flex',
          alignItems: 'center',
          gap: 2,
        }}
      >
        <FormControl>
          <RadioGroup
            row
            value={format}
            onChange={(e) => setFormat(e.target.value as Format)}
          >
            {(Object.keys(formats) as Format[]).map((k) => (
              <FormControlLabel
                key={k}
                value={k}
                control={<Radio size="small" />}
                label={formats[k].label}
              />
            ))}
          </RadioGroup>
        </FormControl>
        <Box sx={{ flex: 1 }} />
        <Button
          size="small"
          startIcon={<DownloadIcon />}
          disabled={!data}
          onClick={downloadCurrent}
        >
          Download
        </Button>
      </Box>
      <Box
        component="pre"
        sx={{
          flex: 1,
          m: 0,
          p: 2,
          overflow: 'auto',
          fontFamily: 'monospace',
          fontSize: 12,
          whiteSpace: 'pre',
        }}
      >
        {isLoading && <Typography>Loading…</Typography>}
        {error && (
          <Typography color="error">
            {(error as Error).message}
          </Typography>
        )}
        {!isLoading && !error && data?.text}
      </Box>
    </Box>
  );
}
