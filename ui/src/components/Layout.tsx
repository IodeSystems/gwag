import type { ReactNode } from 'react';
import {
  AppBar,
  Box,
  Drawer,
  List,
  ListItem,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Toolbar,
  Typography,
} from '@mui/material';
import DashboardIcon from '@mui/icons-material/Dashboard';
import HubIcon from '@mui/icons-material/Hub';
import AccountTreeIcon from '@mui/icons-material/AccountTree';
import SchemaIcon from '@mui/icons-material/Schema';
import { Link } from '@tanstack/react-router';

const drawerWidth = 220;

const nav = [
  { to: '/', label: 'Dashboard', icon: <DashboardIcon /> },
  { to: '/services', label: 'Services', icon: <AccountTreeIcon /> },
  { to: '/peers', label: 'Peers', icon: <HubIcon /> },
  { to: '/schema', label: 'Schema', icon: <SchemaIcon /> },
];

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <Box sx={{ display: 'flex' }}>
      <AppBar position="fixed" sx={{ zIndex: (t) => t.zIndex.drawer + 1 }}>
        <Toolbar>
          <Typography variant="h6" noWrap>
            go-api-gateway
          </Typography>
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
    </Box>
  );
}
