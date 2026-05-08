import { createFileRoute } from '@tanstack/react-router';
import { Card, CardContent, Grid2 as Grid, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { sdk } from '@/api/client';

export const Route = createFileRoute('/')({
  component: Dashboard,
});

function Dashboard() {
  // The Dashboard query is defined in queries.graphql alongside the
  // other operations; pnpm run codegen produces sdk.Dashboard.
  const { data, isLoading, error } = useQuery({
    queryKey: ['dashboard'],
    queryFn: () => sdk.Dashboard(),
    refetchInterval: 5_000,
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  const peers = data?.admin?.listPeers?.peers ?? [];
  const services = data?.admin?.listServices?.services ?? [];
  const env = data?.admin?.listServices?.environment ?? '(unset)';

  return (
    <Grid container spacing={2}>
      <Grid size={{ xs: 12, md: 4 }}>
        <Card>
          <CardContent>
            <Typography color="text.secondary">Environment</Typography>
            <Typography variant="h4">{env}</Typography>
          </CardContent>
        </Card>
      </Grid>
      <Grid size={{ xs: 12, md: 4 }}>
        <Card>
          <CardContent>
            <Typography color="text.secondary">Cluster peers</Typography>
            <Typography variant="h4">{peers.length}</Typography>
          </CardContent>
        </Card>
      </Grid>
      <Grid size={{ xs: 12, md: 4 }}>
        <Card>
          <CardContent>
            <Typography color="text.secondary">Registered services</Typography>
            <Typography variant="h4">{services.length}</Typography>
          </CardContent>
        </Card>
      </Grid>
    </Grid>
  );
}
