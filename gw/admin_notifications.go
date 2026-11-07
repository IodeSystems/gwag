package gateway

import (
	"context"
	"fmt"
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
//     out of retries (declaration_sinks.go). Per node: only the node
//     that accepted the write tries the delivery, so only it lists
//     the failure. Kept in memory, at most 20.
//
// Items are computed at read time; there is no read / dismiss state.
// An item goes away when its cause does.

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
		Summary:     "List items that need a person's attention: external dependents past their review date, and declarations hook deliveries that failed on this node. Oldest first.",
	}, func(ctx context.Context, _ *struct{}) (*notificationsOut, error) {
		out := &notificationsOut{}
		out.Body.Items = g.notifications(nowFunc())
		return out, nil
	})
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
	items = append(items, g.declSinks.failureItems()...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Since < items[j].Since })
	return items
}
