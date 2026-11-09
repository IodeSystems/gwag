package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for rate limits, quotas, costs and the usage report
// (docs/quota.md). Writes are admin only and refused during a cutover
// freeze; reads need the dev role.

type limitsOut struct {
	Body struct {
		Peers         int         `json:"peers" doc:"Peer count this node divides rate limits and short-window quotas by."`
		QuotaDelegate bool        `json:"quotaDelegate" doc:"WithQuota is configured: the _quota_auth/v1 delegate decides quotas and built-in quotas are not checked."`
		Usage         bool        `json:"usage" doc:"The usage report is on (WithUsage)."`
		Limits        []limitView `json:"limits"`
		Pauses        []pauseView `json:"pauses" doc:"Services paused on this node after an upstream 429."`
	}
}

type rateLimitIn struct {
	Body struct {
		Service   string  `json:"service" doc:"ns or ns:vN. Without a version the limit covers every version together."`
		Operation string  `json:"operation,omitempty" doc:"Operation name (OpenAPI operationId, proto method, GraphQL field, MCP tool). Empty: every operation together."`
		RPS       float64 `json:"rps" doc:"Calls per second, cluster-wide."`
		Burst     int     `json:"burst" doc:"Bucket size, cluster-wide."`
	}
}

type quotaIn struct {
	Body struct {
		Caller    string `json:"caller,omitempty" doc:"Caller id. Exactly one of caller and default."`
		Default   bool   `json:"default,omitempty" doc:"The quota of every caller without one of its own at this scope."`
		Service   string `json:"service,omitempty" doc:"ns or ns:vN; empty: every service together."`
		Operation string `json:"operation,omitempty"`
		Limit     uint64 `json:"limit"`
		Per       string `json:"per" doc:"Window: 1s, 1m, 1h, 1d (Go duration or Nd), at most 24h."`
		Units     bool   `json:"units,omitempty" doc:"Count operation cost units instead of calls."`
	}
}

type costIn struct {
	Body struct {
		Service   string `json:"service"`
		Operation string `json:"operation"`
		Units     uint32 `json:"units"`
	}
}

type limitDeleteIn struct {
	Service   string `query:"service"`
	Operation string `query:"operation"`
	Caller    string `query:"caller" doc:"Quotas: the caller id."`
	Default   bool   `query:"default" doc:"Quotas: the default quota."`
}

type limitPutOut struct {
	Body limitRecord
}

type limitDeleteOut struct {
	Body struct {
		Deleted bool `json:"deleted"`
	}
}

type usageIn struct {
	From    string `query:"from" doc:"First UTC day, YYYY-MM-DD. Default: 6 days before to."`
	To      string `query:"to" doc:"Last UTC day, YYYY-MM-DD. Default: today."`
	Caller  string `query:"caller"`
	Service string `query:"service" doc:"ns or ns:vN."`
	Format  string `query:"format" enum:"json,csv" default:"json" doc:"csv: text/csv, one row per line with a header."`
}

// usageReport is the usage response body. As text/csv it is the rows
// (usageCSVHeader).
type usageReport struct {
	From string     `json:"from"`
	To   string     `json:"to"`
	Rows []usageRow `json:"rows"`
}

type usageOut struct {
	ContentType string `header:"Content-Type"`
	Body        usageReport
}

// usageCSVFormat is the admin API's text/csv format; only the usage
// report has a CSV form.
var usageCSVFormat = huma.Format{
	Marshal: func(w io.Writer, v any) error {
		// huma's schema-link transform hands over a copy of the body
		// with a $schema field added, so look for the rows by name.
		rv := reflect.Indirect(reflect.ValueOf(v))
		if rv.Kind() == reflect.Struct {
			if f := rv.FieldByName("Rows"); f.IsValid() {
				if rows, ok := f.Interface().([]usageRow); ok {
					return writeUsageCSV(w, rows)
				}
			}
		}
		return fmt.Errorf("no CSV form for %T", v)
	},
	Unmarshal: func([]byte, any) error { return errors.New("text/csv bodies are not accepted") },
}

func limitsHumaErr(err error) error {
	var ce *cutoverError
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, errLimitsUnavailable):
		return huma.Error503ServiceUnavailable(err.Error())
	}
	return err
}

func registerLimits(api huma.API, g *Gateway) {
	l := g.limits
	put := func(ctx context.Context, what string, r limitRecord) (*limitPutOut, error) {
		if err := r.validate(); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err := g.checkNotFrozen(ctx, what); err != nil {
			return nil, err
		}
		r.SetBy = adminIdentityFromContext(ctx)
		r.SetUnixMs = nowFunc().UnixMilli()
		if err := l.putRecord(ctx, r); err != nil {
			return nil, limitsHumaErr(err)
		}
		return &limitPutOut{Body: r}, nil
	}
	del := func(ctx context.Context, what string, r limitRecord) (*limitDeleteOut, error) {
		if err := g.checkNotFrozen(ctx, what); err != nil {
			return nil, err
		}
		ok, err := l.deleteRecord(ctx, r.key())
		if err != nil {
			return nil, limitsHumaErr(err)
		}
		out := &limitDeleteOut{}
		out.Body.Deleted = ok
		return out, nil
	}
	scope := func(service, op string) (limitRecord, error) {
		ns, ver, err := parseServiceArg(service)
		if err != nil {
			return limitRecord{}, huma.Error422UnprocessableEntity("service: " + err.Error())
		}
		return limitRecord{Namespace: ns, Version: ver, Operation: op}, nil
	}

	huma.Register(api, huma.Operation{
		OperationID: "listLimits",
		Method:      http.MethodGet,
		Path:        "/admin/limits",
		Summary:     "Rate limits, quotas and costs in force (set in code or by an operator; an operator value replaces a code value for the same scope), and this node's upstream 429 pauses.",
	}, func(ctx context.Context, _ *struct{}) (*limitsOut, error) {
		admin, err := l.readRecords(ctx)
		if err != nil {
			return nil, limitsHumaErr(err)
		}
		out := &limitsOut{}
		out.Body.Peers = int(l.peers.Load())
		out.Body.QuotaDelegate = l.quotaOff
		out.Body.Usage = l.usage != nil
		out.Body.Limits, out.Body.Pauses = l.listEffective(admin)
		if out.Body.Limits == nil {
			out.Body.Limits = []limitView{}
		}
		if out.Body.Pauses == nil {
			out.Body.Pauses = []pauseView{}
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setRateLimit",
		Method:      http.MethodPut,
		Path:        "/admin/limits/rate",
		Summary:     "Set a cluster-wide rate limit on a service or operation. Each node applies rps and burst divided by the live peer count. Admin only.",
	}, func(ctx context.Context, in *rateLimitIn) (*limitPutOut, error) {
		r, err := scope(in.Body.Service, in.Body.Operation)
		if err != nil {
			return nil, err
		}
		r.Kind, r.RPS, r.Burst = limitKindRate, in.Body.RPS, in.Body.Burst
		return put(ctx, "setRateLimit", r)
	})
	huma.Register(api, huma.Operation{
		OperationID: "deleteRateLimit",
		Method:      http.MethodDelete,
		Path:        "/admin/limits/rate",
		Summary:     "Delete an operator rate limit (a limit set in code for the same scope applies again). Admin only.",
	}, func(ctx context.Context, in *limitDeleteIn) (*limitDeleteOut, error) {
		r, err := scope(in.Service, in.Operation)
		if err != nil {
			return nil, err
		}
		r.Kind = limitKindRate
		return del(ctx, "deleteRateLimit", r)
	})

	huma.Register(api, huma.Operation{
		OperationID: "setQuota",
		Method:      http.MethodPut,
		Path:        "/admin/limits/quota",
		Summary:     "Set a quota for one caller id, or the default quota of every caller, over a fixed window. Admin only.",
	}, func(ctx context.Context, in *quotaIn) (*limitPutOut, error) {
		r, err := scope(in.Body.Service, in.Body.Operation)
		if err != nil {
			return nil, err
		}
		per, err := parsePer(in.Body.Per)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		r.Kind, r.Caller, r.Default = limitKindQuota, in.Body.Caller, in.Body.Default
		r.Limit, r.PerMs, r.Units = in.Body.Limit, per.Milliseconds(), in.Body.Units
		return put(ctx, "setQuota", r)
	})
	huma.Register(api, huma.Operation{
		OperationID: "deleteQuota",
		Method:      http.MethodDelete,
		Path:        "/admin/limits/quota",
		Summary:     "Delete a quota. Admin only.",
	}, func(ctx context.Context, in *limitDeleteIn) (*limitDeleteOut, error) {
		r, err := scope(in.Service, in.Operation)
		if err != nil {
			return nil, err
		}
		if in.Default == (in.Caller != "") {
			return nil, huma.Error422UnprocessableEntity("give exactly one of caller and default")
		}
		r.Kind, r.Caller, r.Default = limitKindQuota, in.Caller, in.Default
		return del(ctx, "deleteQuota", r)
	})

	huma.Register(api, huma.Operation{
		OperationID: "setCost",
		Method:      http.MethodPut,
		Path:        "/admin/limits/cost",
		Summary:     "Set the units one call to an operation counts against unit quotas and in the usage report. Admin only.",
	}, func(ctx context.Context, in *costIn) (*limitPutOut, error) {
		r, err := scope(in.Body.Service, in.Body.Operation)
		if err != nil {
			return nil, err
		}
		r.Kind, r.Cost = limitKindCost, in.Body.Units
		return put(ctx, "setCost", r)
	})
	huma.Register(api, huma.Operation{
		OperationID: "deleteCost",
		Method:      http.MethodDelete,
		Path:        "/admin/limits/cost",
		Summary:     "Delete an operator cost (a cost set in code applies again). Admin only.",
	}, func(ctx context.Context, in *limitDeleteIn) (*limitDeleteOut, error) {
		r, err := scope(in.Service, in.Operation)
		if err != nil {
			return nil, err
		}
		r.Kind = limitKindCost
		return del(ctx, "deleteCost", r)
	})

	huma.Register(api, huma.Operation{
		OperationID: "getUsage",
		Method:      http.MethodGet,
		Path:        "/admin/usage",
		Summary:     "Usage per (UTC day, caller, namespace, version, operation): calls, units, rate-limit and quota refusals, upstream 429s, errors. Cluster totals as of each node's last flush. format=csv answers text/csv. Needs WithUsage.",
	}, func(ctx context.Context, in *usageIn) (*usageOut, error) {
		u := l.usage
		if u == nil {
			return nil, huma.Error409Conflict("the usage report is off: start the gateway with WithUsage (gwag up --usage)")
		}
		f := usageFilter{Caller: in.Caller}
		var err error
		f.From, f.To = defaultUsageRange(nowFunc())
		if in.To != "" {
			if f.To, err = parseUsageDay(in.To); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			if in.From == "" {
				t, _ := time.Parse("2006-01-02", f.To)
				f.From = t.Add(-6 * 24 * time.Hour).Format("2006-01-02")
			}
		}
		if in.From != "" {
			if f.From, err = parseUsageDay(in.From); err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
		}
		if f.From > f.To {
			return nil, huma.Error422UnprocessableEntity("from is after to")
		}
		t1, _ := time.Parse("2006-01-02", f.From)
		t2, _ := time.Parse("2006-01-02", f.To)
		if t2.Sub(t1) > 400*24*time.Hour {
			return nil, huma.Error422UnprocessableEntity("range is longer than 400 days")
		}
		if f.Namespace, f.Version, err = parseServiceArg(in.Service); err != nil {
			return nil, huma.Error422UnprocessableEntity("service: " + err.Error())
		}
		rows, err := u.report(ctx, f)
		if err != nil {
			return nil, limitsHumaErr(err)
		}
		if rows == nil {
			rows = []usageRow{}
		}
		out := &usageOut{Body: usageReport{From: f.From, To: f.To, Rows: rows}}
		if in.Format == "csv" {
			out.ContentType = "text/csv; charset=utf-8"
		}
		return out, nil
	})
}
