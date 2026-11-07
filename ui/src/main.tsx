import React, { useEffect, type ReactNode } from 'react';
import ReactDOM from 'react-dom/client';
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { RouterProvider, createRouter } from '@tanstack/react-router';
import { routeTree } from './routeTree.gen';
import { AppThemeProvider, ToastProvider } from '@/ui';
import { client } from '@/api/client';
import { EnvironmentQuery } from '@/api/operations';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 30_000, refetchOnWindowFocus: false },
  },
});

const router = createRouter({ routeTree });

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}

// The environment name comes from the server (admin reads need no token).
// Empty until loaded, or when the server names none: no badge then.
function Themed({ children }: { children: ReactNode }) {
  const { data } = useQuery({
    queryKey: ['environment'],
    queryFn: () => client.request(EnvironmentQuery),
    staleTime: Infinity,
    retry: 2,
  });
  const environment = data?.admin?.listServices?.environment ?? '';
  useEffect(() => {
    document.title = environment ? `gwag · ${environment}` : 'gwag';
  }, [environment]);
  return <AppThemeProvider environment={environment}>{children}</AppThemeProvider>;
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <Themed>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </Themed>
    </QueryClientProvider>
  </React.StrictMode>
);
