import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import { CssBaseline, ThemeProvider } from '@mui/material';
import { createAppTheme } from './theme';
import type { Mode } from './tokens';

const STORAGE_KEY = 'gwag.ui.themeMode';
export const DEFAULT_ENVIRONMENT = 'dev';

function readMode(): Mode {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === 'light' || v === 'dark') return v;
  } catch {
    // storage unavailable (private window, blocked): fall through
  }
  return 'dark';
}

function writeMode(m: Mode) {
  try {
    localStorage.setItem(STORAGE_KEY, m);
  } catch {
    // preference simply does not persist
  }
}

interface ModeCtx {
  mode: Mode;
  setMode: (m: Mode) => void;
  toggle: () => void;
  environment: string;
}

const Ctx = createContext<ModeCtx | null>(null);

export interface AppThemeProviderProps {
  children: ReactNode;
  /** Environment name ("dev", "staging", "prod", ...). Drives the header accent. */
  environment?: string;
}

export function AppThemeProvider({
  children,
  environment = DEFAULT_ENVIRONMENT,
}: AppThemeProviderProps) {
  const [mode, setModeState] = useState<Mode>(readMode);
  const setMode = useCallback((m: Mode) => {
    setModeState(m);
    writeMode(m);
  }, []);
  const toggle = useCallback(() => setMode(mode === 'dark' ? 'light' : 'dark'), [mode, setMode]);
  const theme = useMemo(() => createAppTheme(mode, environment), [mode, environment]);
  const value = useMemo(
    () => ({ mode, setMode, toggle, environment }),
    [mode, setMode, toggle, environment],
  );
  return (
    <Ctx.Provider value={value}>
      <ThemeProvider theme={theme}>
        <CssBaseline enableColorScheme />
        {children}
      </ThemeProvider>
    </Ctx.Provider>
  );
}

/** Nested provider with a fixed mode, for side-by-side previews. */
export function ScopedTheme({
  mode,
  environment = DEFAULT_ENVIRONMENT,
  children,
}: {
  mode: Mode;
  environment?: string;
  children: ReactNode;
}) {
  const theme = useMemo(() => createAppTheme(mode, environment), [mode, environment]);
  return <ThemeProvider theme={theme}>{children}</ThemeProvider>;
}

export function useThemeMode(): ModeCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useThemeMode must be used inside <AppThemeProvider>');
  return v;
}
