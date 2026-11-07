import { createFileRoute } from '@tanstack/react-router';
import { Button } from '@mui/material';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { client } from '@/api/client';
import { PeersQuery, ForgetPeerMutation } from '@/api/operations';
import { DataTable, EmptyState, Page, Panel, useToast, type Column } from '@/ui';

export const Route = createFileRoute('/peers')({
  component: Peers,
});

interface Peer {
  nodeId?: string | null;
  name?: string | null;
  joinedUnixMs?: unknown;
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

  const peers: Peer[] = (data?.admin?.listPeers?.peers ?? []).filter(
    (p): p is NonNullable<typeof p> => p != null,
  );

  const columns: Column<Peer>[] = [
    { id: 'nodeId', header: 'Node ID', mono: true, cell: (p) => p.nodeId },
    { id: 'name', header: 'Name', cell: (p) => p.name },
    {
      id: 'joined',
      header: 'Joined',
      cell: (p) =>
        p.joinedUnixMs ? new Date(Number(p.joinedUnixMs)).toLocaleString() : '',
    },
    {
      id: 'action',
      header: 'Action',
      align: 'right',
      cell: (p) => (
        <Button
          size="small"
          color="warning"
          disabled={forget.isPending}
          onClick={() => p.nodeId && forget.mutate(p.nodeId)}
        >
          Forget
        </Button>
      ),
    },
  ];

  return (
    <Page title="Peers" subtitle="Gateways in this cluster.">
      <Panel flush>
        {error ? (
          <EmptyState compact title="Failed to load peers" description={error.message} />
        ) : (
          <DataTable
            columns={columns}
            rows={peers}
            loading={isLoading}
            getRowId={(p) => p.nodeId ?? ''}
            emptyTitle="No peers"
          />
        )}
      </Panel>
    </Page>
  );
}
