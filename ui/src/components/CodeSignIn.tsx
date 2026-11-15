import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
} from '@mui/material';
import { useToast } from '@/ui';
import { fetchSSO, redeemSignInCode, SignInError, takeSignInCode } from '@/api/sso';
import { getAdminToken, setAdminToken } from '@/api/auth';
import { client } from '@/api/client';
import { WhoamiQuery } from '@/api/operations';

// currentSignIns names who this browser is signed in as, by cookie session
// or pasted token; [] when neither.
async function currentSignIns(): Promise<string[]> {
  const out: string[] = [];
  const sso = await fetchSSO();
  if (sso.authenticated) {
    const how = sso.via === 'token' ? 'gwag login --web' : 'SSO';
    out.push(`${sso.email ?? 'unknown'} (${sso.role ?? 'no role'}, ${how})`);
  }
  if (getAdminToken()) {
    let role = 'unknown role';
    try {
      role = (await client.request(WhoamiQuery)).admin?.whoami?.role ?? role;
    } catch {
      // a token the gateway refuses: still a token this replaces
    }
    out.push(`a pasted token (${role})`);
  }
  return out;
}

/**
 * CodeSignIn asks before it redeems the sign-in code gwag login --web
 * put in the URL (captured, and removed from the address bar, by
 * captureSignInCode before the router ran). Anyone who can mint a code
 * can send such a link; redeeming it signs this browser in as them. So
 * nothing is sent until the person confirms. Cancel drops the code.
 */
export default function CodeSignIn() {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [code, setCode] = useState<string | null>(null);
  const [current, setCurrent] = useState<string[]>([]);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // takeSignInCode returns the code once, so StrictMode's second
    // effect run finds nothing.
    const c = takeSignInCode();
    if (!c) return;
    void currentSignIns().then((cur) => {
      setCurrent(cur);
      setCode(c);
    });
  }, []);

  const close = () => {
    if (pending) return;
    setCode(null);
    setError(null);
  };

  const signIn = async () => {
    if (!code) return;
    setPending(true);
    setError(null);
    try {
      // replace only when the dialog named the session it replaces.
      const r = await redeemSignInCode(code, current.length > 0);
      // A pasted token wins over the cookie on every request; drop it
      // so the new session applies.
      if (getAdminToken()) setAdminToken(null);
      setCode(null);
      toast.success(`Signed in as ${r.subject} (${r.role}).`);
      void queryClient.invalidateQueries();
    } catch (e: unknown) {
      if (e instanceof SignInError && e.status === 409) {
        // The gateway sees a session the dialog did not name (one
        // appeared since it opened, or one with no role now): name
        // it, and the next click replaces it.
        const cur = await currentSignIns();
        setCurrent(cur.length > 0 ? cur : [e.subject ?? 'an existing session']);
        setError('This browser holds a session already. Read the text above again; Sign in replaces it.');
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setPending(false);
    }
  };

  return (
    <Dialog open={code !== null} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>Sign in with a gwag login --web link?</DialogTitle>
      <DialogContent>
        <DialogContentText component="div">
          <p style={{ marginTop: 0 }}>
            This link signs this browser in as whoever ran <code>gwag login --web</code>, with
            their role. Continue only if you ran it just now. If someone sent you this link,
            cancel.
          </p>
          {current.length > 0 && (
            <p style={{ marginBottom: 0 }}>
              This browser is signed in as {current.join(' and ')}. Signing in replaces that.
            </p>
          )}
        </DialogContentText>
        {error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {error}
          </Alert>
        )}
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2 }}>
        <Button color="inherit" onClick={close} disabled={pending}>
          Cancel
        </Button>
        <Button
          variant="contained"
          color={current.length > 0 ? 'error' : 'primary'}
          onClick={() => void signIn()}
          disabled={pending}
        >
          {pending ? 'Signing in…' : 'Sign in'}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
