import {
  Box,
  Button,
  Chip,
  Drawer,
  Stack,
  Typography,
} from '@mui/material';
import { useEvents } from '@/providers/EventsProvider';

interface EventsTrayProps {
  open: boolean;
  onClose: () => void;
}

/**
 * EventsTray is a slide-out drawer (right side) that renders the
 * EventsProvider's ring buffer as a reverse-chronological feed. The
 * Layout opens it from the AppBar bell icon.
 */
export default function EventsTray({ open, onClose }: EventsTrayProps) {
  const { recent, clear } = useEvents();

  return (
    <Drawer anchor="right" open={open} onClose={onClose}>
      <Box sx={{ width: 440, p: 3 }}>
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 2 }}>
          <Typography variant="h6" sx={{ flexGrow: 1 }}>
            Events
          </Typography>
          <Chip size="small" label={`${recent.length}`} />
          <Button size="small" onClick={clear} disabled={recent.length === 0}>
            Clear
          </Button>
          <Button size="small" onClick={onClose} sx={{ ml: 1 }}>
            Close
          </Button>
        </Stack>

        {recent.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            No events yet. Pages that opt into <code>useSubscribe(...)</code>{' '}
            from <code>@/providers/EventsProvider</code> will populate this
            feed.
          </Typography>
        ) : (
          <Stack spacing={1.25}>
            {recent.map((e, i) => (
              <Box
                key={`${e.receivedAt}-${i}`}
                sx={{
                  border: 1,
                  borderColor: e.error ? 'error.light' : 'divider',
                  borderRadius: 1,
                  p: 1,
                }}
              >
                <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                  <Chip
                    size="small"
                    label={e.id}
                    color={e.error ? 'error' : 'default'}
                  />
                  <Typography variant="caption" color="text.secondary">
                    {new Date(e.receivedAt).toLocaleTimeString()}
                  </Typography>
                </Stack>
                <Box
                  component="pre"
                  sx={{
                    fontFamily: 'monospace',
                    fontSize: 11,
                    m: 0,
                    mt: 0.5,
                    whiteSpace: 'pre-wrap',
                    wordBreak: 'break-word',
                  }}
                >
                  {previewPayload(e.payload)}
                </Box>
              </Box>
            ))}
          </Stack>
        )}
      </Box>
    </Drawer>
  );
}

function previewPayload(p: unknown): string {
  try {
    return JSON.stringify(p, null, 2);
  } catch {
    return String(p);
  }
}
