import { useSyncExternalStore, useState } from 'react';
import {
  Box,
  Button,
  Drawer,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import { useQueryClient } from '@tanstack/react-query';
import { PresetPicker } from '@/ui';
import { useAdminRole } from '@/api/role';
import { useSSO } from '@/api/sso';
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
 * SettingsDrawer is the operator's entry point for pasting a bearer
 * token: the admin token or the dev token. The gateway logs both at
 * boot (`admin token = <hex>`, `dev token = <hex>`) and decides the
 * role. Stored in sessionStorage only.
 */
export default function SettingsDrawer({ open, onClose }: SettingsDrawerProps) {
  const current = useAdminToken();
  const { role, invalid } = useAdminRole();
  const sso = useSSO();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState(current ?? '');

  // Keep the draft in sync when the drawer opens against a changed
  // value (e.g. another tab cleared it). Adjusted during render, not in
  // an effect, so it does not cost a second render.
  const [synced, setSynced] = useState({ open, current });
  if (synced.open !== open || synced.current !== current) {
    setSynced({ open, current });
    if (open) setDraft(current ?? '');
  }

  // Every admin read depends on the role, so refetch them all when
  // the token changes.
  const save = () => {
    setAdminToken(draft.trim() || null);
    void queryClient.invalidateQueries();
    onClose();
  };
  const clear = () => {
    setAdminToken(null);
    void queryClient.invalidateQueries();
    setDraft('');
  };

  return (
    <Drawer anchor="right" open={open} onClose={onClose}>
      <Box sx={{ width: 380, p: 3 }}>
        <Typography variant="h6" gutterBottom>
          Settings
        </Typography>

        <Typography variant="subtitle2" sx={{ mt: 2, mb: 1 }}>
          Appearance
        </Typography>
        <PresetPicker />

        <Typography variant="subtitle2" sx={{ mt: 2 }}>
          Bearer token
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          The dev token shows every service and lets you edit external
          dependents. The admin token also allows every write (drain,
          deprecate, forget peer, sign tokens). The gateway logs{' '}
          <code>admin token = …</code> and <code>dev token = …</code> at
          boot. Paste either one here.
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

        <Typography
          variant="caption"
          color={invalid ? 'error.main' : current || sso.authenticated ? 'success.main' : 'warning.main'}
          sx={{ display: 'block', mt: 1 }}
        >
          {invalid
            ? 'The gateway rejected this token (rotated, or from another gateway).'
            : current
              ? `Signed in as ${role === 'admin' ? 'Admin' : role === 'dev' ? 'Dev' : role}.`
              : sso.authenticated
                ? `No token set. Signed in with SSO as ${sso.email ?? '?'} (${role}).`
                : 'No token set: public services only, no writes.'}
        </Typography>
      </Box>
    </Drawer>
  );
}
