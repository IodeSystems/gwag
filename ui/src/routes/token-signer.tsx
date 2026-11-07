import { createFileRoute } from '@tanstack/react-router';
import { Alert } from '@mui/material';
import { useMutation } from '@tanstack/react-query';
import { client } from '@/api/client';
import { SignSubscriptionTokenMutation } from '@/api/operations';
import { FormTextField, InlineForm, KeyValue, Page, Panel } from '@/ui';
import { useRoleGate } from '@/api/role';

export const Route = createFileRoute('/token-signer')({
  component: TokenSigner,
});

function TokenSigner() {
  const { allowed, tip } = useRoleGate('admin');
  const sign = useMutation({
    mutationFn: (v: { channel: string; ttl: number | string; kid: string }) =>
      client.request(SignSubscriptionTokenMutation, {
        channel: v.channel.trim(),
        ttlSeconds: Number(v.ttl) || 0,
        kid: v.kid.trim() || undefined,
      }),
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
        {!allowed && (
          <Alert severity="info" sx={{ mb: 2 }}>
            {tip}: signing is an admin operation.
          </Alert>
        )}
        {/* Request errors surface as the form-level alert (mutateAsync throws). */}
        <InlineForm
          defaultValues={{ channel: '', ttl: 60, kid: '' }}
          submitLabel="Sign"
          submittingLabel="Signing…"
          canSubmit={(v) => allowed && v.channel.trim().length > 0}
          onSubmit={async (v) => {
            await sign.mutateAsync(v);
          }}
        >
          <FormTextField
            name="channel"
            label="Channel"
            placeholder="e.g. orders.* or room.42"
            required
            helperText="The resolved subject the token authorizes."
          />
          <FormTextField
            name="ttl"
            label="TTL (seconds)"
            type="number"
            helperText="Informational lifetime echoed to the client."
          />
          <FormTextField
            name="kid"
            label="Key id (kid)"
            placeholder="optional — empty uses the default key"
            helperText="Names a rotated signing key; the gateway echoes the kid it signed under."
          />
        </InlineForm>
      </Panel>

      {denied && (
        <Panel title="Denied">
          <Alert severity="warning">
            {out.code}
            {out.reason ? `: ${out.reason}` : ''}
          </Alert>
        </Panel>
      )}

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
