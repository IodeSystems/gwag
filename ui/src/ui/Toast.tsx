import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { Alert, Snackbar, Stack } from '@mui/material';

export type ToastSeverity = 'success' | 'info' | 'warning' | 'error';

interface ToastItem {
  id: number;
  severity: ToastSeverity;
  message: ReactNode;
}

export interface ToastApi {
  show: (message: ReactNode, severity?: ToastSeverity) => void;
  success: (message: ReactNode) => void;
  info: (message: ReactNode) => void;
  warning: (message: ReactNode) => void;
  error: (message: ReactNode) => void;
}

const Ctx = createContext<ToastApi | null>(null);

const MAX_VISIBLE = 4;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => setItems((xs) => xs.filter((x) => x.id !== id)), []);

  const api = useMemo<ToastApi>(() => {
    const show = (message: ReactNode, severity: ToastSeverity = 'info') =>
      setItems((xs) => [...xs, { id: nextId.current++, severity, message }].slice(-MAX_VISIBLE));
    return {
      show,
      success: (m) => show(m, 'success'),
      info: (m) => show(m, 'info'),
      warning: (m) => show(m, 'warning'),
      error: (m) => show(m, 'error'),
    };
  }, []);

  return (
    <Ctx.Provider value={api}>
      {children}
      <Stack
        spacing={1}
        sx={{ position: 'fixed', bottom: 16, left: 16, zIndex: (t) => t.zIndex.snackbar }}
      >
        {items.map((it) => (
          <Snackbar
            key={it.id}
            open
            // Errors stay longer; they usually need reading.
            autoHideDuration={it.severity === 'error' ? 8000 : 4000}
            onClose={(_e, reason) => {
              if (reason !== 'clickaway') dismiss(it.id);
            }}
            sx={{ position: 'static', transform: 'none' }}
          >
            <Alert
              severity={it.severity}
              variant="outlined"
              onClose={() => dismiss(it.id)}
              sx={(t) => ({ bgcolor: t.gwag.tokens.surfaceRaised, boxShadow: t.shadows[3], minWidth: 280 })}
            >
              {it.message}
            </Alert>
          </Snackbar>
        ))}
      </Stack>
    </Ctx.Provider>
  );
}

export function useToast(): ToastApi {
  const v = useContext(Ctx);
  if (!v) throw new Error('useToast must be used inside <ToastProvider>');
  return v;
}
