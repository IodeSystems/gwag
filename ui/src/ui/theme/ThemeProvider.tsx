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
import { DEFAULT_PRESET, isPresetId, type PresetId } from './presets';
import type { Mode } from './tokens';

const STORAGE_KEY = 'gwag.ui.themeMode';
const PRESET_KEY = 'gwag.ui.themePreset';
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

function readPreset(): PresetId {
  try {
    const v = localStorage.getItem(PRESET_KEY);
    if (isPresetId(v)) return v;
  } catch {
    // storage unavailable: fall through
  }
  return DEFAULT_PRESET;
}

function writePreset(p: PresetId) {
  try {
    localStorage.setItem(PRESET_KEY, p);
  } catch {
    // preference simply does not persist
  }
}

interface ModeCtx {
  preset: PresetId;
  setPreset: (p: PresetId) => void;
  mode: Mode;
  setMode: (m: Mode) => void;
  toggle: () => void;
  environment: string;
}

const Ctx = createContext<ModeCtx | null>(null);

export interface AppThemeProviderProps {
  children: ReactNode;
  /** Environment name ("dev", "staging", "prod", ...). Drives the header accent.
   * Empty or omitted: none known; `useThemeMode().environment` is "" and the
   * theme uses the default accent. */
  environment?: string;
}

export function AppThemeProvider({
  children,
  environment = '',
}: AppThemeProviderProps) {
  // Empty means the server named none: no badge, default styling.
  const themeEnv = environment || DEFAULT_ENVIRONMENT;
  const [mode, setModeState] = useState<Mode>(readMode);
  const setMode = useCallback((m: Mode) => {
    setModeState(m);
    writeMode(m);
  }, []);
  const [preset, setPresetState] = useState<PresetId>(readPreset);
  const setPreset = useCallback((p: PresetId) => {
    setPresetState(p);
    writePreset(p);
  }, []);
  const toggle = useCallback(() => setMode(mode === 'dark' ? 'light' : 'dark'), [mode, setMode]);
  const theme = useMemo(() => createAppTheme(mode, themeEnv, preset), [mode, themeEnv, preset]);
  const value = useMemo(
    () => ({ preset, setPreset, mode, setMode, toggle, environment }),
    [preset, setPreset, mode, setMode, toggle, environment],
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

/** Nested provider with a fixed preset and mode, for previews. */
export function ScopedTheme({
  mode,
  preset = DEFAULT_PRESET,
  environment = DEFAULT_ENVIRONMENT,
  children,
}: {
  mode: Mode;
  preset?: PresetId;
  environment?: string;
  children: ReactNode;
}) {
  const theme = useMemo(
    () => createAppTheme(mode, environment, preset),
    [mode, environment, preset],
  );
  return <ThemeProvider theme={theme}>{children}</ThemeProvider>;
}

export function useThemeMode(): ModeCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('useThemeMode must be used inside <AppThemeProvider>');
  return v;
}
