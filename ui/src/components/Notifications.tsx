import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { NotificationBell } from '@/ui';
import { client } from '@/api/client';
import { NotificationsQuery } from '@/api/operations';
import { useEvents } from '@/providers/EventsProvider';

// The app-bar bell: the admin listNotifications op (overdue
// external-dependent reviews, failed declarations hook deliveries),
// polled and refetched on service change events. An item's href is an
// admin UI route; choosing the item opens it.

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

  const items = data?.admin?.listNotifications?.items ?? [];
  return (
    <NotificationBell
      items={items}
      onSelect={(it) => {
        const href = items.find((x) => x.id === it.id)?.href ?? '';
        // Only routes of this UI.
        if (href.startsWith('/') && !href.startsWith('//')) router.history.push(href);
      }}
    />
  );
}
