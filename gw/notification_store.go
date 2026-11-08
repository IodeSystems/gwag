package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Stored notifications: items that cannot be computed from other
// state, so they must be kept. Today that is declaration_hook_failed
// (declaration_sinks.go): a delivery that ran out of retries.
//
// Storage: in memory standalone; in cluster mode the
// gwag-notifications KV bucket, mirrored into g.notifs by a watch on
// every node, so GET /admin/notifications is the same on every node.
// The bucket has no TTL: an entry leaves only by an explicit delete
// (a TTL expiry sends watchers no event, so nodes would disagree).
//
// Lifecycle of a declaration_hook_failed item:
//   - created by the node whose delivery failed;
//   - full-mode hook: deleted by the next successful delivery from
//     any node, for every item of that mode whose seq the delivered
//     snapshot covers (the receiver has the whole set again);
//   - delta-mode hook: deleted only by an explicit dismiss
//     (POST /admin/notifications/{id}/dismiss, dev role). The receiver
//     missed a change; a person resyncs it and then dismisses.
//   - bounded: at most hookFailureItems items; the writer deletes the
//     oldest beyond that. Detail is cut at notificationDetailMax.

const (
	notificationsBucketName = "gwag-notifications"

	// hookFailureItems bounds the declaration_hook_failed items kept.
	hookFailureItems = 20
	// notificationDetailMax bounds an item's detail text, in bytes.
	notificationDetailMax = 1024
)

// storedNotification is an item as stored (KV JSON value).
type storedNotification struct {
	notificationItem
	// Seq is the declaration change the item is about; Mode is the
	// hook mode (full or delta) it failed in. Both drive the clear
	// rule above.
	Seq  uint64 `json:"seq"`
	Mode string `json:"mode"`
}

// notificationStore holds the stored items. mu is a leaf lock.
type notificationStore struct {
	mu sync.Mutex
	m  map[string]storedNotification
}

func (s *notificationStore) set(n storedNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]storedNotification{}
	}
	s.m[n.ID] = n
}

func (s *notificationStore) remove(id string) (storedNotification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[id]
	delete(s.m, id)
	return n, ok
}

func (s *notificationStore) get(id string) (storedNotification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[id]
	return n, ok
}

// all returns the items, oldest first (ties by id).
func (s *notificationStore) all() []storedNotification {
	s.mu.Lock()
	out := make([]storedNotification, 0, len(s.m))
	for _, n := range s.m {
		out = append(out, n)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// items is all() as list items.
func (s *notificationStore) items() []notificationItem {
	all := s.all()
	out := make([]notificationItem, len(all))
	for i, n := range all {
		out[i] = n.notificationItem
	}
	return out
}

func (g *Gateway) notificationsKV() jetstream.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers == nil {
		return nil
	}
	return g.peers.notifications
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("...")
	for cut > 0 && s[cut]&0xC0 == 0x80 { // not mid-rune
		cut--
	}
	return s[:cut] + "..."
}

// putNotification stores n (cluster: the KV first, so a failed write
// changes nothing), then trims the kind to its bound.
func (g *Gateway) putNotification(ctx context.Context, n storedNotification) error {
	n.Detail = truncateBytes(n.Detail, notificationDetailMax)
	if kv := g.notificationsKV(); kv != nil {
		b, err := json.Marshal(n)
		if err != nil {
			return err
		}
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		_, err = kvPutRevRetry(kctx, kv, n.ID, b)
		cancel()
		if err != nil {
			return fmt.Errorf("notification: kv put: %w", err)
		}
	}
	g.notifs.set(n)
	g.trimNotifications(ctx, n.Kind, hookFailureItems)
	return nil
}

// deleteNotification removes the item. ok=false when this node knows
// no item with that id.
func (g *Gateway) deleteNotification(ctx context.Context, id string) (bool, error) {
	if _, ok := g.notifs.get(id); !ok {
		return false, nil
	}
	if kv := g.notificationsKV(); kv != nil {
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		err := kvDeleteRetry(kctx, kv, id)
		cancel()
		if err != nil {
			return false, fmt.Errorf("notification: kv delete: %w", err)
		}
	}
	g.notifs.remove(id)
	return true, nil
}

// trimNotifications deletes the oldest items of kind beyond max.
// Nodes that trim at the same time delete the same items, which is
// the same state.
func (g *Gateway) trimNotifications(ctx context.Context, kind string, max int) {
	var of []storedNotification
	for _, n := range g.notifs.all() {
		if n.Kind == kind {
			of = append(of, n)
		}
	}
	for len(of) > max {
		if _, err := g.deleteNotification(ctx, of[0].ID); err != nil {
			g.notificationWarn("trim", err)
			return
		}
		of = of[1:]
	}
}

// clearHookFailures deletes every declaration_hook_failed item of
// mode whose seq is at most upto: a full-mode delivery of a snapshot
// at upto succeeded.
func (g *Gateway) clearHookFailures(ctx context.Context, mode DeclarationsSinkMode, upto uint64) {
	for _, n := range g.notifs.all() {
		if n.Kind != notificationHookFailed || n.Mode != string(mode) || n.Seq > upto {
			continue
		}
		if _, err := g.deleteNotification(ctx, n.ID); err != nil {
			g.notificationWarn("clear", err)
		}
	}
}

func (g *Gateway) notificationWarn(what string, err error) {
	if g.cfg.cluster != nil {
		g.cfg.cluster.Server.Warnf("gateway: notifications %s: %v", what, err)
	}
}

// openNotificationsBucket creates (at r replicas, see openKV) or opens the bucket. No TTL and
// no stream limits: entries leave by explicit delete, and the count
// and size are bounded by putNotification. Limits are left unset so
// setReplicas, which rewrites the config, does not change them.
func openNotificationsBucket(ctx context.Context, js jetstream.JetStream, r int) (jetstream.KeyValue, error) {
	return openKV(ctx, js, jetstream.KeyValueConfig{Bucket: notificationsBucketName}, r)
}

// notificationsWatchLoop mirrors the bucket into g.notifs. A put
// replaces, a delete or purge removes. IncludeHistory replays the
// current set, so a node that joins later sees every item.
func (t *peerTracker) notificationsWatchLoop(ctx context.Context) {
	defer close(t.notificationsDone)
	w, err := t.notifications.WatchAll(ctx, jetstream.IncludeHistory())
	if err != nil {
		t.gw.cfg.cluster.Server.Warnf("gateway: notifications watch start: %v", err)
		return
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Updates():
			if !ok {
				return
			}
			if ev == nil {
				continue // the initial replay is done
			}
			switch ev.Operation() {
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				t.gw.notifs.remove(ev.Key())
				continue
			}
			var n storedNotification
			if err := json.Unmarshal(ev.Value(), &n); err != nil || n.ID != ev.Key() {
				t.gw.cfg.cluster.Server.Warnf("gateway: notifications bucket: bad value at %s: %v", ev.Key(), err)
				continue
			}
			t.gw.notifs.set(n)
		}
	}
}

// nowRFC3339 is the Since stamp of a new item.
func nowRFC3339() string { return nowFunc().UTC().Format(time.RFC3339) }
