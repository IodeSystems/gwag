import { useState } from 'react';
import { Button, Stack, Typography } from '@mui/material';
import { FormDialog, FormRow, FormSelect, FormTextField, useToast } from '@/ui';
import { client } from '@/api/client';
import { PutExternalDependentMutation } from '@/api/operations';
import type { ExternalDependent } from './model';

// Add or edit a declared external dependent: a consumer gwag cannot
// see (customer, partner, app) that pins lines. The gateway validates;
// its field errors land on the matching inputs (mapFieldErrors), e.g.
// body.depends.1.lines on the second row's lines.

type Pin = { service: string; lines: string };
type Values = {
  id: string;
  name: string;
  kind: string;
  owner: { team: string; contact: string };
  depends: Pin[];
  note: string;
  expiresAt: string;
};

const kinds = [
  { value: 'customer', label: 'Customer' },
  { value: 'partner', label: 'Partner' },
  { value: 'app', label: 'App release' },
  { value: 'other', label: 'Other' },
];

const midnight = 'T00:00:00Z';

/** A date alone means midnight UTC; anything else goes to the server as typed. */
function toExpiresAt(v: string): string | null {
  const s = v.trim();
  if (s === '') return null;
  return /^\d{4}-\d{2}-\d{2}$/.test(s) ? s + midnight : s;
}

function fromExpiresAt(v: string): string {
  return v.endsWith(midnight) ? v.slice(0, -midnight.length) : v;
}

function splitLines(v: string): string[] {
  return v
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

export interface ExternalDependentDialogProps {
  /** null = add a new one. */
  initial: ExternalDependent | null;
  onClose: () => void;
  onSaved: () => void;
}

/** Mount it to open it: each mount starts from `initial`. */
export function ExternalDependentDialog({ initial, onClose, onSaved }: ExternalDependentDialogProps) {
  const toast = useToast();
  const pins: Pin[] = (initial?.depends ?? []).map((d) => ({ service: d.service, lines: (d.lines ?? []).join(', ') }));
  const [rows, setRows] = useState(Math.max(1, pins.length));
  const defaults: Values = {
    id: initial?.id ?? '',
    name: initial?.name ?? '',
    kind: initial?.kind ?? 'customer',
    owner: { team: initial?.owner?.team ?? '', contact: initial?.owner?.contact ?? '' },
    depends: pins.length > 0 ? pins : [{ service: '', lines: '' }],
    note: initial?.note ?? '',
    expiresAt: fromExpiresAt(initial?.expiresAt ?? ''),
  };

  return (
    <FormDialog<Values>
      open
      maxWidth="md"
      title={initial ? `Edit ${initial.name || initial.id}` : 'Add external dependent'}
      description="A consumer the gateway cannot see, such as an API customer, a partner integration or a mobile app release, that pins lines. It counts as a dependent in the sunset rule, dependency_unmet and drain coverage until it expires or is deleted. It does not affect the start gate."
      submitLabel={initial ? 'Save' : 'Add'}
      defaultValues={defaults}
      onClose={onClose}
      onSubmit={async (v) => {
        const depends = Array.from({ length: rows }, (_, i) => v.depends[i] ?? { service: '', lines: '' })
          .map((p) => ({ service: (p.service ?? '').trim(), lines: splitLines(p.lines ?? '') }));
        const team = v.owner.team.trim();
        const contact = v.owner.contact.trim();
        const res = await client.request(PutExternalDependentMutation, {
          id: initial?.id ?? v.id.trim(),
          body: {
            name: v.name.trim() || null,
            kind: v.kind,
            owner: team || contact ? { team: team || null, contact: contact || null } : null,
            depends,
            note: v.note.trim() || null,
            expiresAt: toExpiresAt(v.expiresAt),
          },
        });
        const warnings = res.admin?.putExternalDependent?.warnings ?? [];
        const id = res.admin?.putExternalDependent?.externalDependent.id ?? v.id;
        if (warnings.length > 0) toast.warning(`Saved ${id}. ${warnings.join('; ')}`);
        else toast.success(`Saved ${id}`);
        onSaved();
      }}
    >
      <FormRow>
        {initial ? (
          <Typography variant="body2" sx={{ pt: 1, fontFamily: (t) => t.gwag.mono }}>
            id: {initial.id}
          </Typography>
        ) : (
          <FormTextField name="id" label="Id" placeholder="acme" helperText="Lower-case slug. Cannot be changed." autoFocus monospace />
        )}
        <FormTextField name="name" label="Name" placeholder="Acme Corp" />
        <FormSelect name="kind" label="Kind" options={kinds} />
      </FormRow>
      <FormRow>
        <FormTextField name="owner.team" label="Owner team" placeholder="sales-eng" helperText="The team on this side that owns the relationship." />
        <FormTextField name="owner.contact" label="Contact" placeholder="acme-api@example.com" />
      </FormRow>
      <Typography variant="h6">Pinned lines</Typography>
      {Array.from({ length: rows }, (_, i) => (
        <FormRow key={i}>
          <FormTextField name={`depends[${i}].service`} label="Service" placeholder="library" monospace />
          <FormTextField name={`depends[${i}].lines`} label="Lines" placeholder="v1, v2" helperText="vN or unstable, comma separated." monospace />
        </FormRow>
      ))}
      <Stack direction="row" spacing={1}>
        <Button size="small" onClick={() => setRows((n) => n + 1)}>
          Add service
        </Button>
        <Button size="small" color="inherit" disabled={rows <= 1} onClick={() => setRows((n) => Math.max(1, n - 1))}>
          Remove last
        </Button>
      </Stack>
      <FormRow>
        <FormTextField
          name="expiresAt"
          label="Expires"
          placeholder="2027-06-30"
          helperText="YYYY-MM-DD (midnight UTC) or an RFC 3339 time. Empty = no expiry. Expired declarations stay listed but stop counting."
          monospace
        />
      </FormRow>
      <FormTextField name="note" label="Note" placeholder="Contract 2026-14, review at renewal" multiline rows={2} />
    </FormDialog>
  );
}
