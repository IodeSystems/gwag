import { createFileRoute } from '@tanstack/react-router';
import {
  Alert,
  Button,
  Card,
  CardContent,
  CardHeader,
  Stack,
  TextField,
} from '@mui/material';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { client } from '@/api/client';
import { SignSubscriptionTokenMutation } from '@/api/operations';

export const Route = createFileRoute('/token-signer')({
  component: TokenSigner,
});

function TokenSigner() {
  const [channel, setChannel] = useState('');
  const [ttl, setTtl] = useState('60');
  const [kid, setKid] = useState('');

  const sign = useMutation({
    mutationFn: () =>
      client.request(SignSubscriptionTokenMutation, {
        channel: channel.trim(),
        ttlSeconds: Number(ttl) || 0,
        kid: kid.trim() || undefined,
      }),
  });

  const out = sign.data?.admin?.signSubscriptionToken;
  // The op returns a code; anything other than OK carries a reason.
  const denied = out && out.code !== 'OK';

  return (
    <Stack spacing={2} sx={{ maxWidth: 640 }}>
      <Card>
        <CardHeader
          title="Sign subscribe token"
          subheader="Mint an HMAC token a client presents on a subscription (the GUI for `gwag sign`). Requires the admin bearer (Settings)."
        />
        <CardContent>
          <Stack spacing={2}>
            <TextField
              label="Channel"
              placeholder="e.g. orders.* or room.42"
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
              disabled={sign.isPending}
              fullWidth
              required
              helperText="The resolved subject the token authorizes."
            />
            <TextField
              label="TTL (seconds)"
              type="number"
              value={ttl}
              onChange={(e) => setTtl(e.target.value)}
              disabled={sign.isPending}
              fullWidth
              helperText="Informational lifetime echoed to the client."
            />
            <TextField
              label="Key id (kid)"
              placeholder="optional — empty uses the default key"
              value={kid}
              onChange={(e) => setKid(e.target.value)}
              disabled={sign.isPending}
              fullWidth
              helperText="Names a rotated signing key; the gateway echoes the kid it signed under."
            />
            <Button
              variant="contained"
              disabled={sign.isPending || channel.trim().length === 0}
              onClick={() => sign.mutate()}
            >
              Sign
            </Button>
          </Stack>
        </CardContent>
      </Card>

      {sign.error && (
        <Alert severity="error">{(sign.error as Error).message}</Alert>
      )}

      {denied && (
        <Alert severity="warning">
          {out.code}
          {out.reason ? `: ${out.reason}` : ''}
        </Alert>
      )}

      {out && !denied && (
        <Card>
          <CardHeader
            title="Token"
            subheader="Pass these as the subscription's hmac / timestamp (/ kid) arguments."
          />
          <CardContent>
            <Stack spacing={2}>
              <ReadOnly label="hmac" value={out.hmac ?? ''} />
              <ReadOnly
                label="timestampUnix"
                value={out.timestampUnix != null ? String(out.timestampUnix) : ''}
              />
              <ReadOnly label="kid" value={out.kid ?? '(default)'} />
            </Stack>
          </CardContent>
        </Card>
      )}
    </Stack>
  );
}

// ReadOnly renders a selectable, copyable value field.
function ReadOnly({ label, value }: { label: string; value: string }) {
  return (
    <TextField
      label={label}
      value={value}
      fullWidth
      slotProps={{ input: { readOnly: true, sx: { fontFamily: 'monospace' } } }}
      onFocus={(e) => e.target.select()}
      variant="outlined"
      size="small"
    />
  );
}
