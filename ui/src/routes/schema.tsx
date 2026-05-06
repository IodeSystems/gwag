import { createFileRoute } from '@tanstack/react-router';
import { Box, Paper, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';

export const Route = createFileRoute('/schema')({
  component: Schema,
});

function Schema() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['schema-sdl'],
    queryFn: async () => {
      const r = await fetch('/api/schema/graphql');
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      return r.text();
    },
  });

  if (isLoading) return <Typography>Loading…</Typography>;
  if (error) return <Typography color="error">{(error as Error).message}</Typography>;

  return (
    <Paper sx={{ p: 2 }}>
      <Typography variant="h6" gutterBottom>
        SDL
      </Typography>
      <Box
        component="pre"
        sx={{
          fontFamily: 'monospace',
          fontSize: 12,
          overflow: 'auto',
          maxHeight: '70vh',
          m: 0,
        }}
      >
        {data}
      </Box>
    </Paper>
  );
}
