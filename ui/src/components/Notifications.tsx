import { useEffect } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { NotificationBell, useToast } from '@/ui';
import { client } from '@/api/client';
import { DismissNotificationMutation, NotificationsQuery } from '@/api/operations';
import { useRoleGate } from '@/api/role';
import { useEvents } from '@/providers/EventsProvider';

// The app-bar bell: the admin listNotifications op (overdue
// external-dependent reviews, failed declarations hook deliveries),
// polled and refetched on service change events. An item's href is an
// admin UI route; choosing the item opens it. A failed hook delivery
// can be dismissed (dev or admin role); the gateway clears it on every
// node.

const POLL_MS = 30_000;
const EVENT_DEBOUNCE_MS = 300;

export const notificationsQueryKey = ['notifications'];

export default function Notifications() {
  const router = useRouter();
  const queryClient = useQueryClient();
  const { data } = useQuery({
    queryKey: notificationsQueryKey,
    queryFn: () => client.request(NotificationsQuery),
    refetchInterval: POLL_MS,
  });

  const { serviceChangeSeq } = useEvents();
  useEffect(() => {
    if (serviceChangeSeq === 0) return;
    const t = setTimeout(() => void queryClient.invalidateQueries({ queryKey: notificationsQueryKey }), EVENT_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [serviceChangeSeq, queryClient]);

  const toast = useToast();
  const { allowed: canDismiss, tip: dismissTip } = useRoleGate('dev');
  const dismiss = useMutation({
    mutationFn: (id: string) => client.request(DismissNotificationMutation, { id }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: notificationsQueryKey }),
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e)),
  });

  const items = data?.admin?.listNotifications?.items ?? [];
  return (
    <NotificationBell
      items={items}
      canDismiss={canDismiss && !dismiss.isPending}
      dismissTip={dismissTip}
      onDismiss={(it) => dismiss.mutate(it.id)}
      onSelect={(it) => {
        const href = items.find((x) => x.id === it.id)?.href ?? '';
        // Only routes of this UI.
        if (href.startsWith('/') && !href.startsWith('//')) router.history.push(href);
      }}
    />
  );
}
