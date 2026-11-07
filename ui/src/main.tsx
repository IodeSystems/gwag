import React from 'react';
import ReactDOM from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createRouter } from '@tanstack/react-router';
import { routeTree } from './routeTree.gen';
import { AppThemeProvider, ToastProvider } from '@/ui';

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

// The environment name comes from the server in a later slice;
// AppThemeProvider defaults to "dev" until then.
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <AppThemeProvider>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </AppThemeProvider>
    </QueryClientProvider>
  </React.StrictMode>
);
