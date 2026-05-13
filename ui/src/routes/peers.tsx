import { createFileRoute } from '@tanstack/react-router';
import {
  Button,
  Paper,
  Snackbar,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { client } from '@/api/client';
import { PeersQuery, ForgetPeerMutation } from '@/api/operations';

export const Route = createFileRoute('/peers')({
  component: Peers,
});

function Peers() {
  const qc = useQueryClient();
  const [toast, setToast] = useState<string | null>(null);

  const { data, isLoading, error } = useQuery({
    queryKey: ['peers'],
    queryFn: () => client.request(PeersQuery),
    refetchInterval: 5_000,
  });

  const forget = useMutation({
    mutationFn: (nodeId: string) => client.request(ForgetPeerMutation, { nodeId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['peers'] });
      setToast('peer forgotten');
    },
    onError: (e: Error) => setToast(e.message),
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  const peers = data?.admin?.listPeers?.peers ?? [];

  return (
    <>
      <TableContainer component={Paper}>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Node ID</TableCell>
              <TableCell>Name</TableCell>
              <TableCell>Joined</TableCell>
              <TableCell align="right">Action</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {peers.map((p, i) => (
              <TableRow key={i}>
                <TableCell>
                  <code style={{ fontSize: '0.75rem' }}>{p?.nodeId}</code>
                </TableCell>
                <TableCell>{p?.name}</TableCell>
                <TableCell>
                  {p?.joinedUnixMs
                    ? new Date(Number(p.joinedUnixMs)).toLocaleString()
                    : ''}
                </TableCell>
                <TableCell align="right">
                  <Button
                    size="small"
                    color="warning"
                    disabled={forget.isPending}
                    onClick={() => p?.nodeId && forget.mutate(p.nodeId)}
                  >
                    Forget
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
      <Snackbar
        open={toast !== null}
        autoHideDuration={3000}
        onClose={() => setToast(null)}
        message={toast ?? ''}
      />
    </>
  );
}
