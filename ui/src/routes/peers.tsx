import { createFileRoute } from '@tanstack/react-router';
import { Button } from '@mui/material';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { client } from '@/api/client';
import { PeersQuery, ForgetPeerMutation } from '@/api/operations';
import { DataTable, EmptyState, Page, Panel, useToast, type Column } from '@/ui';
import { RoleGate } from '@/components/RoleGate';

export const Route = createFileRoute('/peers')({
  component: Peers,
});

interface Peer {
  nodeId?: string | null;
  name?: string | null;
  joinedUnixMs?: unknown;
  version?: string | null;
  minLevel?: unknown;
  maxLevel?: unknown;
}

function Peers() {
  const qc = useQueryClient();
  const toast = useToast();

  const { data, isLoading, error } = useQuery({
    queryKey: ['peers'],
    queryFn: () => client.request(PeersQuery),
    refetchInterval: 5_000,
  });

  const forget = useMutation({
    mutationFn: (nodeId: string) => client.request(ForgetPeerMutation, { nodeId }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['peers'] });
      toast.success('peer forgotten');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const list = data?.admin?.listPeers;
  const peers: Peer[] = (list?.peers ?? []).filter(
    (p): p is NonNullable<typeof p> => p != null,
  );
  const offline = (list?.metaOffline ?? []).filter((n) => n != null);
  const subtitle = list
    ? `Gateways in this cluster. Feature level ${Number(list.activeLevel ?? 0)}.` +
      (Number(list.metaSize ?? 0) > 0
        ? ` JetStream meta: leader ${list.metaLeader ?? '-'}, size ${Number(list.metaSize)}, offline ${offline.length > 0 ? offline.join(', ') : 'none'}.`
        : '')
    : 'Gateways in this cluster.';

  const columns: Column<Peer>[] = [
    { id: 'nodeId', header: 'Node ID', mono: true, cell: (p) => p.nodeId },
    { id: 'name', header: 'Name', cell: (p) => p.name },
    {
      id: 'joined',
      header: 'Joined',
      cell: (p) =>
        p.joinedUnixMs ? new Date(Number(p.joinedUnixMs)).toLocaleString() : '',
    },
    { id: 'version', header: 'Version', mono: true, cell: (p) => p.version || 'no level support' },
    {
      id: 'levels',
      header: 'Levels',
      cell: (p) => `${Number(p.minLevel ?? 0)}-${Number(p.maxLevel ?? 0)}`,
    },
    {
      id: 'action',
      header: 'Action',
      align: 'right',
      cell: (p) => (
        <RoleGate need="admin">
          {(allowed) => (
            <Button
              size="small"
              color="warning"
              disabled={!allowed || forget.isPending}
              onClick={() => p.nodeId && forget.mutate(p.nodeId)}
            >
              Forget
            </Button>
          )}
        </RoleGate>
      ),
    },
  ];

  return (
    <Page title="Peers" subtitle={subtitle}>
      <Panel flush>
        {error ? (
          <EmptyState compact title="Failed to load peers" description={error.message} />
        ) : (
          <DataTable
            columns={columns}
            rows={peers}
            loading={isLoading}
            getRowId={(p) => p.nodeId ?? ''}
            emptyTitle="Standalone, not clustered"
            emptyDescription="This gateway runs without a cluster, so it has no peers. Nodes appear here once they join over embedded NATS."
          />
        )}
      </Panel>
    </Page>
  );
}
