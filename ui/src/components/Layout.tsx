import { useState, type ReactNode } from 'react';
import {
  AppBar,
  Badge,
  Box,
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
import HubIcon from '@mui/icons-material/Hub';
import AccountTreeIcon from '@mui/icons-material/AccountTree';
import SchemaIcon from '@mui/icons-material/Schema';
import SettingsIcon from '@mui/icons-material/Settings';
import { Link } from '@tanstack/react-router';
import SettingsDrawer, { useAdminToken } from './SettingsDrawer';

const drawerWidth = 220;

const nav = [
  { to: '/', label: 'Dashboard', icon: <DashboardIcon /> },
  { to: '/services', label: 'Services', icon: <AccountTreeIcon /> },
  { to: '/peers', label: 'Peers', icon: <HubIcon /> },
  { to: '/schema', label: 'Schema', icon: <SchemaIcon /> },
];

export default function Layout({ children }: { children: ReactNode }) {
  const [settingsOpen, setSettingsOpen] = useState(false);
  const token = useAdminToken();
  const tooltip = token ? 'Settings — admin token set' : 'Settings — no admin token set';

  return (
    <Box sx={{ display: 'flex' }}>
      <AppBar position="fixed" sx={{ zIndex: (t) => t.zIndex.drawer + 1 }}>
        <Toolbar>
          <Typography variant="h6" noWrap sx={{ flexGrow: 1 }}>
            go-api-gateway
          </Typography>
          <Tooltip title={tooltip}>
            <IconButton color="inherit" onClick={() => setSettingsOpen(true)} aria-label="settings">
              <Badge
                color="warning"
                variant="dot"
                invisible={token !== null}
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
      <Box component="main" sx={{ flexGrow: 1, p: 3 }}>
        <Toolbar />
        {children}
      </Box>
      <SettingsDrawer open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </Box>
  );
}
