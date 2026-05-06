import { useSyncExternalStore, useState, useEffect } from 'react';
import {
  Box,
  Button,
  Drawer,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import {
  getAdminToken,
  setAdminToken,
  onAdminTokenChange,
} from '@/api/auth';

const subscribe = (cb: () => void) => onAdminTokenChange(cb);
const snapshot = () => getAdminToken();
const ssrSnapshot = () => null;

/**
 * useAdminToken returns the current admin token (or null) and
 * re-renders consumers whenever it changes via setAdminToken.
 */
export function useAdminToken(): string | null {
  return useSyncExternalStore(subscribe, snapshot, ssrSnapshot);
}

interface SettingsDrawerProps {
  open: boolean;
  onClose: () => void;
}

/**
 * SettingsDrawer is the operator's entry point for pasting the admin
 * bearer token. The gateway logs `admin token = <hex>` at boot; copy
 * that string into the field. Stored in sessionStorage only.
 */
export default function SettingsDrawer({ open, onClose }: SettingsDrawerProps) {
  const current = useAdminToken();
  const [draft, setDraft] = useState(current ?? '');

  // Keep the draft in sync when the drawer opens against a changed
  // value (e.g. another tab cleared it).
  useEffect(() => {
    if (open) setDraft(current ?? '');
  }, [open, current]);

  const save = () => {
    setAdminToken(draft.trim() || null);
    onClose();
  };
  const clear = () => {
    setAdminToken(null);
    setDraft('');
  };

  return (
    <Drawer anchor="right" open={open} onClose={onClose}>
      <Box sx={{ width: 380, p: 3 }}>
        <Typography variant="h6" gutterBottom>
          Settings
        </Typography>

        <Typography variant="subtitle2" sx={{ mt: 2 }}>
          Admin bearer token
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          Required for write operations (forget peer, sign tokens, etc.).
          The gateway logs <code>admin token = …</code> at boot — paste that
          value here.
        </Typography>

        <TextField
          fullWidth
          multiline
          minRows={2}
          maxRows={4}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="paste hex token from boot log"
          autoComplete="off"
          spellCheck={false}
          sx={{ fontFamily: 'monospace', '& textarea': { fontFamily: 'monospace', fontSize: '0.85rem' } }}
        />

        <Stack direction="row" spacing={1} sx={{ mt: 2 }}>
          <Button variant="contained" onClick={save} disabled={draft.trim() === (current ?? '')}>
            Save
          </Button>
          <Button onClick={clear} color="warning" disabled={!current}>
            Clear
          </Button>
          <Button onClick={onClose} sx={{ ml: 'auto' }}>
            Close
          </Button>
        </Stack>

        <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mt: 3 }}>
          Stored in this tab's sessionStorage only. Closing the browser
          tab discards it; refresh keeps it.
        </Typography>

        <Typography variant="caption" color={current ? 'success.main' : 'warning.main'} sx={{ display: 'block', mt: 1 }}>
          {current ? 'Token is set.' : 'No token set — write operations will return 401.'}
        </Typography>
      </Box>
    </Drawer>
  );
}
