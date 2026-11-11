import { useState, type ReactNode } from 'react';
import {
  Alert,
  AppBar,
  Badge,
  Box,
  Button,
  Chip,
  Drawer,
  IconButton,
  List,
  ListItem,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Toolbar,
  Tooltip,
  Typography,
} from '@mui/material';
import DashboardIcon from '@mui/icons-material/Dashboard';
import DeviceHubIcon from '@mui/icons-material/DeviceHub';
import HubIcon from '@mui/icons-material/Hub';
import AccountTreeIcon from '@mui/icons-material/AccountTree';
import HistoryToggleOffIcon from '@mui/icons-material/HistoryToggleOff';
import SchemaIcon from '@mui/icons-material/Schema';
import InputIcon from '@mui/icons-material/Input';
import SmartToyIcon from '@mui/icons-material/SmartToy';
import VpnKeyIcon from '@mui/icons-material/VpnKey';
import SettingsIcon from '@mui/icons-material/Settings';
import DynamicFeedIcon from '@mui/icons-material/DynamicFeed';
import { Link } from '@tanstack/react-router';
import SettingsDrawer, { useAdminToken } from './SettingsDrawer';
import EventsTray from './EventsTray';
import Notifications from './Notifications';
import { EnvBadge, PresetPicker, ThemeToggle, useThemeMode } from '@/ui';
import { EventsProvider, useEvents } from '@/providers/EventsProvider';
import { useAdminRole } from '@/api/role';

const drawerWidth = 220;

const nav = [
  { to: '/', label: 'Graph', icon: <DeviceHubIcon /> },
  { to: '/status', label: 'Status', icon: <DashboardIcon /> },
  { to: '/services', label: 'Services', icon: <AccountTreeIcon /> },
  { to: '/deprecated', label: 'Deprecated', icon: <HistoryToggleOffIcon /> },
  { to: '/injectors', label: 'Injectors', icon: <InputIcon /> },
  { to: '/peers', label: 'Peers', icon: <HubIcon /> },
  { to: '/schema', label: 'Schema', icon: <SchemaIcon /> },
  { to: '/mcp-allowlist', label: 'MCP', icon: <SmartToyIcon /> },
  { to: '/token-signer', label: 'Sign Token', icon: <VpnKeyIcon /> },
];

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <EventsProvider>
      <LayoutInner>{children}</LayoutInner>
    </EventsProvider>
  );
}

function LayoutInner({ children }: { children: ReactNode }) {
  const { environment } = useThemeMode();
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [eventsOpen, setEventsOpen] = useState(false);
  const token = useAdminToken();
  const { role, invalid } = useAdminRole();
  const { unread, markRead } = useEvents();
  const settingsTooltip = invalid
    ? 'Settings — the gateway rejected this token'
    : token
      ? `Settings — signed in as ${role}`
      : 'Settings — no token set';
  const eventsTooltip = unread > 0 ? `Events (${unread} new)` : 'Events';

  const openEvents = () => {
    markRead();
    setEventsOpen(true);
  };

  // Without a token the UI runs in "status" mode: the graph and
  // status pages show the services the gateway marks public, but the
  // side-nav, events tray, and the deeper pages (dev or admin reads,
  // mutations) stay out of view. A "Sign in" button and a banner
  // explain how to see more.
  if (!token) {
    return (
      <Box sx={{ display: 'flex', flexDirection: 'column' }}>
        <AppBar position="fixed">
          <Toolbar>
            <Typography variant="h6" noWrap sx={{ flexGrow: 1 }}>
              gwag
            </Typography>
            <Button color="inherit" component={Link} to="/">
              Graph
            </Button>
            <Button color="inherit" component={Link} to="/status">
              Status
            </Button>
            {environment && <EnvBadge name={environment} />}
            <PresetPicker compact />
            <ThemeToggle />
            <Button
              color="inherit"
              startIcon={<SettingsIcon />}
              onClick={() => setSettingsOpen(true)}
              aria-label="sign in"
            >
              Sign in
            </Button>
          </Toolbar>
        </AppBar>
        <Box component="main" sx={{ flexGrow: 1, minWidth: 0, p: 3 }}>
          <Toolbar />
          <Alert severity="info" sx={{ mb: 2 }}>
            Public view: only services marked public are shown, without
            owner contacts or external dependents. Sign in with the dev
            token to see every service and edit external dependents, or
            with the admin token for every operation (drain, deprecate).
            The gateway prints both at startup and keeps them under its
            data dir.
          </Alert>
          {children}
        </Box>
        <SettingsDrawer open={settingsOpen} onClose={() => setSettingsOpen(false)} />
      </Box>
    );
  }

  return (
    <Box sx={{ display: 'flex' }}>
      <AppBar position="fixed" sx={{ zIndex: (t) => t.zIndex.drawer + 1 }}>
        <Toolbar>
          <Typography variant="h6" noWrap sx={{ flexGrow: 1 }}>
            gwag
          </Typography>
          {environment && <EnvBadge name={environment} />}
          <Chip
            size="small"
            sx={{ mx: 1 }}
            color={invalid ? 'warning' : role === 'admin' ? 'secondary' : 'default'}
            label={invalid ? 'Token rejected' : role === 'admin' ? 'Admin' : role === 'dev' ? 'Dev' : 'Public'}
          />
          <PresetPicker compact />
          <ThemeToggle />
          <Notifications />
          <Tooltip title={eventsTooltip}>
            <IconButton color="inherit" onClick={openEvents} aria-label="events">
              <Badge color="error" badgeContent={unread} max={99} overlap="circular">
                <DynamicFeedIcon />
              </Badge>
            </IconButton>
          </Tooltip>
          <Tooltip title={settingsTooltip}>
            <IconButton color="inherit" onClick={() => setSettingsOpen(true)} aria-label="settings">
              <Badge
                color="warning"
                variant="dot"
                invisible={token !== null && !invalid}
                overlap="circular"
              >
                <SettingsIcon />
              </Badge>
            </IconButton>
          </Tooltip>
        </Toolbar>
      </AppBar>
      <Drawer
        variant="permanent"
        sx={{
          width: drawerWidth,
          flexShrink: 0,
          '& .MuiDrawer-paper': { width: drawerWidth, boxSizing: 'border-box' },
        }}
      >
        <Toolbar />
        <Box sx={{ overflow: 'auto' }}>
          <List>
            {nav.map((n) => (
              <ListItem key={n.to} disablePadding>
                <ListItemButton component={Link} to={n.to}>
                  <ListItemIcon>{n.icon}</ListItemIcon>
                  <ListItemText primary={n.label} />
                </ListItemButton>
              </ListItem>
            ))}
          </List>
        </Box>
      </Drawer>
      <Box component="main" sx={{ flexGrow: 1, minWidth: 0, p: 3 }}>
        <Toolbar />
        {children}
      </Box>
      <SettingsDrawer open={settingsOpen} onClose={() => setSettingsOpen(false)} />
      <EventsTray open={eventsOpen} onClose={() => setEventsOpen(false)} />
    </Box>
  );
}
