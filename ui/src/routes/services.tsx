import { createFileRoute } from '@tanstack/react-router';
import {
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { sdk } from '@/api/client';

export const Route = createFileRoute('/services')({
  component: Services,
});

function Services() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => sdk.Services(),
    refetchInterval: 10_000,
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  const services = data?.admin_listServices?.services ?? [];

  return (
    <TableContainer component={Paper}>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Namespace</TableCell>
            <TableCell>Version</TableCell>
            <TableCell>Hash</TableCell>
            <TableCell align="right">Replicas</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {services.map((s, i) => (
            <TableRow key={i}>
              <TableCell>{s?.namespace}</TableCell>
              <TableCell>{s?.version}</TableCell>
              <TableCell>
                <code style={{ fontSize: '0.75rem' }}>{s?.hashHex?.slice(0, 16)}…</code>
              </TableCell>
              <TableCell align="right">{s?.replicaCount}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
