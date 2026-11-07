package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Notifications: things that need a person's attention, as one list
// for the admin UI's bell, the CLI and other tools. Each item has a
// kind; kinds join the list as features need them:
//
//   - declaration_review_due: an external dependent past its review
//     date (external_dependents.go). Cluster-wide: every node reads
//     the replicated set.
//   - declaration_hook_failed: a declarations hook delivery that ran
//     out of retries (declaration_sinks.go). Stored (notification_store.go):
//     in a cluster every node lists it, standalone it is in memory.
//     At most 20. Full-mode hook: cleared by the next successful
//     delivery from any node. Delta-mode hook: stays until a dev or
//     admin dismisses it (POST /admin/notifications/{id}/dismiss).
//
// declaration_review_due is computed at read time and goes away when
// its cause does; it cannot be dismissed.

const (
	notificationReviewDue  = "declaration_review_due"
	notificationHookFailed = "declaration_hook_failed"
)

type notificationItem struct {
	Kind   string `json:"kind" doc:"declaration_review_due or declaration_hook_failed; more kinds may be added."`
	ID     string `json:"id" doc:"Stable id of the item, unique in the list."`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Since  string `json:"since" doc:"RFC 3339: when the item started needing attention."`
	Href   string `json:"href" doc:"Admin UI route that shows the subject, e.g. /?select=ext:acme."`
	// Dismissable is true for stored items a person may clear.
	Dismissable bool `json:"dismissable" doc:"True when POST /admin/notifications/{id}/dismiss may clear the item."`
}

type notificationsOut struct {
	Body struct {
		Items []notificationItem `json:"items"`
	}
}

// externalSelectID is the graph page's selection id for a declaration
// (ui: components/graph/model.ts externalId).
func externalSelectID(id string) string { return "ext:" + id }

func registerNotifications(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "listNotifications",
		Method:      http.MethodGet,
		Path:        "/admin/notifications",
		Summary:     "List items that need a person's attention: external dependents past their review date, and declarations hook deliveries that failed (on any node). Oldest first.",
	}, func(ctx context.Context, _ *struct{}) (*notificationsOut, error) {
		out := &notificationsOut{}
		out.Body.Items = g.notifications(nowFunc())
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "dismissNotification",
		Method:      http.MethodPost,
		Path:        "/admin/notifications/{id}/dismiss",
		Summary:     "Dismiss a stored notification (a declaration_hook_failed item) on every node. Computed items (declaration_review_due) cannot be dismissed: they go away when their cause does. Dev role or admin.",
	}, func(ctx context.Context, in *dismissNotificationIn) (*dismissNotificationOut, error) {
		n, ok := g.notifs.get(in.ID)
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no dismissable notification %q", in.ID))
		}
		if _, err := g.deleteNotification(ctx, in.ID); err != nil {
			return nil, err
		}
		slog.Info("gwag: notification dismissed", "id", n.ID, "kind", n.Kind, "by", adminIdentityFromContext(ctx))
		out := &dismissNotificationOut{}
		out.Body.Dismissed = true
		return out, nil
	})
}

type dismissNotificationIn struct {
	ID string `path:"id" doc:"Item id from the list, e.g. hook-failed-12-acme."`
}

type dismissNotificationOut struct {
	Body struct {
		Dismissed bool `json:"dismissed"`
	}
}

func (g *Gateway) notifications(now time.Time) []notificationItem {
	items := []notificationItem{}
	for _, x := range g.externals.all() {
		if !x.reviewDue(now) {
			continue
		}
		since := x.ReviewUnixMs
		detail := fmt.Sprintf("Review date %s has passed.", rfc3339Ms(x.ReviewUnixMs))
		if since == 0 {
			since = x.UpdatedUnixMs
			detail = "No review date is set."
		}
		var pins []string
		for _, d := range x.Depends {
			pins = append(pins, fmt.Sprintf("%s [%s]", d.Service, strings.Join(d.Lines, ", ")))
		}
		detail += " It still pins " + strings.Join(pins, ", ") + ". Confirm the pins and set a new review date, or delete it."
		if x.Owner != nil && (x.Owner.Team != "" || x.Owner.Contact != "") {
			detail += " Owner: " + strings.TrimSpace(x.Owner.Team+" "+x.Owner.Contact) + "."
		}
		name := x.Name
		if name == "" {
			name = x.ID
		}
		items = append(items, notificationItem{
			Kind:   notificationReviewDue,
			ID:     "review-due/" + x.ID,
			Title:  "Review external dependent " + name,
			Detail: detail,
			Since:  rfc3339Ms(since),
			Href:   "/?select=" + externalSelectID(x.ID),
		})
	}
	items = append(items, g.notifs.items()...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Since < items[j].Since })
	return items
}
