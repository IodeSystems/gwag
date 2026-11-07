import { useState } from 'react';
import { Badge, Box, Button, Divider, IconButton, ListItemText, Menu, MenuItem, Tooltip, Typography } from '@mui/material';
import NotificationsIcon from '@mui/icons-material/Notifications';

export interface NotificationBellItem {
  id: string;
  title: string;
  detail: string;
  /** RFC 3339: when it started needing attention. */
  since: string;
  /** A person may clear the item with onDismiss. */
  dismissable?: boolean;
}

export interface NotificationBellProps {
  items: NotificationBellItem[];
  onSelect: (item: NotificationBellItem) => void;
  /** Clears a dismissable item. Without it the dismiss action is hidden. */
  onDismiss?: (item: NotificationBellItem) => void;
  /** Disables the dismiss action when the role cannot use it. */
  canDismiss?: boolean;
  /** Tooltip for a disabled dismiss action. */
  dismissTip?: string;
  /** Shown in the menu when the list is empty. */
  emptyText?: string;
}

function sinceText(since: string): string {
  const t = Date.parse(since);
  return Number.isNaN(t) ? since : `since ${new Date(t).toLocaleString()}`;
}

/**
 * App-bar bell with a count badge. Opens a menu of items that need a
 * person's attention; choosing one calls onSelect and closes the menu.
 */
export function NotificationBell({
  items,
  onSelect,
  onDismiss,
  canDismiss = true,
  dismissTip = '',
  emptyText = 'Nothing needs attention.',
}: NotificationBellProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const n = items.length;
  const tip = n === 0 ? 'Notifications' : `Notifications (${n} need attention)`;
  return (
    <>
      <Tooltip title={tip}>
        <IconButton
          color="inherit"
          onClick={(e) => setAnchor(e.currentTarget)}
          aria-label={tip}
          aria-haspopup="menu"
        >
          <Badge color="warning" badgeContent={n} max={99} overlap="circular">
            <NotificationsIcon />
          </Badge>
        </IconButton>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => setAnchor(null)}
        slotProps={{ paper: { sx: { width: 420, maxWidth: 'calc(100vw - 32px)' } } }}
      >
        <Box sx={{ px: 2, py: 1 }}>
          <Typography variant="h6">Needs attention</Typography>
        </Box>
        <Divider />
        {n === 0 && (
          <Box sx={{ px: 2, py: 1.5 }}>
            <Typography variant="body2" color="text.secondary">
              {emptyText}
            </Typography>
          </Box>
        )}
        {items.map((it) => (
          <MenuItem
            key={it.id}
            onClick={() => {
              setAnchor(null);
              onSelect(it);
            }}
            sx={{ alignItems: 'flex-start', whiteSpace: 'normal' }}
          >
            <ListItemText
              primary={it.title}
              secondary={
                <>
                  {it.detail}
                  <Box component="span" sx={{ display: 'block', mt: 0.5, color: 'text.disabled' }}>
                    {sinceText(it.since)}
                  </Box>
                </>
              }
            />
            {it.dismissable && onDismiss && (
              <Tooltip title={dismissTip}>
                <span>
                  <Button
                    size="small"
                    disabled={!canDismiss}
                    onClick={(e) => {
                      e.stopPropagation();
                      onDismiss(it);
                    }}
                    sx={{ ml: 1, flexShrink: 0 }}
                  >
                    Dismiss
                  </Button>
                </span>
              </Tooltip>
            )}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}
