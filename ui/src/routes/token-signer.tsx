import { createFileRoute } from '@tanstack/react-router';
import { Button, Stack, TextField } from '@mui/material';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { client } from '@/api/client';
import { SignSubscriptionTokenMutation } from '@/api/operations';
import { KeyValue, Page, Panel, useToast } from '@/ui';

export const Route = createFileRoute('/token-signer')({
  component: TokenSigner,
});

function TokenSigner() {
  const toast = useToast();
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
    onSuccess: (res) => {
      const o = res.admin?.signSubscriptionToken;
      // The op returns a code; anything other than OK carries a reason.
      if (o && o.code !== 'OK') {
        toast.warning(`${o.code}${o.reason ? `: ${o.reason}` : ''}`);
      }
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const out = sign.data?.admin?.signSubscriptionToken;
  const denied = out && out.code !== 'OK';

  return (
    <Page
      title="Sign subscribe token"
      subtitle="Mint an HMAC token a client presents on a subscription (the GUI for `gwag sign`). Requires the admin bearer (Settings)."
      maxWidth={640}
    >
      <Panel>
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
      </Panel>

      {out && !denied && (
        <Panel
          title="Token"
          subtitle="Pass these as the subscription's hmac / timestamp (/ kid) arguments."
        >
          <KeyValue
            items={[
              { label: 'hmac', value: out.hmac ?? '', mono: true, copyable: true },
              {
                label: 'timestampUnix',
                value: out.timestampUnix != null ? String(out.timestampUnix) : '',
                mono: true,
                copyable: true,
              },
              { label: 'kid', value: out.kid ?? '(default)', mono: true, copyable: true },
            ]}
          />
        </Panel>
      )}
    </Page>
  );
}
