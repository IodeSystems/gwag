package gat

// Combining the response headers of fused operations.
//
// Over REST each operation answers alone, with its own Set-Cookie and
// cache headers. A GraphQL response carries several operations in one
// body, so gat combines what they returned instead of dropping it:
// cookies are all emitted (cookieSink), and the cache headers merge.
//
// gat caches nothing itself — caching belongs to the client and to
// whatever sits in front of the server. The merge only has to keep the
// combined response from claiming more than its parts allow: when the
// operations agree their headers pass through, otherwise the most
// restrictive combination goes out.

import (
	"encoding/json"
	"hash/fnv"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// outputHeaderPlan locates, once per output type, the top-level fields
// huma writes as response headers (`header:"..."`). Nil when the type
// has none, so operations without header fields cost nothing per call.
type outputHeaderPlan struct {
	setCookie    []int
	cacheControl int // field index, -1 when absent
	etag         int
	lastModified int
	vary         int
}

func newOutputHeaderPlan(t reflect.Type) *outputHeaderPlan {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	p := &outputHeaderPlan{cacheControl: -1, etag: -1, lastModified: -1, vary: -1}
	found := false
	for i := 0; i < t.NumField(); i++ {
		tag, ok := t.Field(i).Tag.Lookup("header")
		if !ok {
			continue
		}
		switch name := strings.SplitN(tag, ",", 2)[0]; {
		case strings.EqualFold(name, "Set-Cookie"):
			p.setCookie = append(p.setCookie, i)
		case strings.EqualFold(name, "Cache-Control"):
			p.cacheControl = i
		case strings.EqualFold(name, "ETag"):
			p.etag = i
		case strings.EqualFold(name, "Last-Modified"):
			p.lastModified = i
		case strings.EqualFold(name, "Vary"):
			p.vary = i
		default:
			continue
		}
		found = true
	}
	if !found {
		return nil
	}
	return p
}

// structOf dereferences out to its struct value, or reports false.
func structOf(out any) (reflect.Value, bool) {
	v := reflect.ValueOf(out)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	return v, v.Kind() == reflect.Struct
}

func (p *outputHeaderPlan) cookies(v reflect.Value) []*http.Cookie {
	var out []*http.Cookie
	for _, i := range p.setCookie {
		out = append(out, cookiesFromValue(v.Field(i))...)
	}
	return out
}

func (p *outputHeaderPlan) str(v reflect.Value, i int) string {
	if i < 0 {
		return ""
	}
	switch f := v.Field(i).Interface().(type) {
	case string:
		return f
	case *string:
		if f != nil {
			return *f
		}
	case time.Time:
		if !f.IsZero() {
			return f.UTC().Format(http.TimeFormat)
		}
	}
	return ""
}

// cacheMerge accumulates the cache headers of one request's operations.
// It lives in the request scope; operations may report concurrently
// (ParallelQueries).
type cacheMerge struct {
	mu sync.Mutex

	ops    int  // operations dispatched
	failed bool // an operation returned an error

	withCC  int    // operations that set Cache-Control
	firstCC string // the first one seen
	sameCC  bool   // every Cache-Control so far equals firstCC
	cc      ccDirectives

	withETag  int
	firstETag string
	etagSum   uint64 // order-independent: operations finish in any order

	withLM  int
	lastMod time.Time

	vary []string
}

// observe records one operation's outcome. p is the output type's
// header plan (nil: no header fields), out its output.
func (m *cacheMerge) observe(p *outputHeaderPlan, out any, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops++
	if err != nil {
		m.failed = true
		return
	}
	if p == nil {
		return
	}
	v, ok := structOf(out)
	if !ok {
		return
	}
	if cc := p.str(v, p.cacheControl); cc != "" {
		if m.withCC == 0 {
			m.firstCC, m.sameCC, m.cc = cc, true, parseCacheControl(cc)
		} else {
			m.sameCC = m.sameCC && cc == m.firstCC
			m.cc.restrict(parseCacheControl(cc))
		}
		m.withCC++
	}
	if et := p.str(v, p.etag); et != "" {
		if m.withETag == 0 {
			m.firstETag = et
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(et))
		m.etagSum += h.Sum64()
		m.withETag++
	}
	if lm := p.str(v, p.lastModified); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			if t.After(m.lastMod) {
				m.lastMod = t
			}
			m.withLM++
		}
	}
	if vy := p.str(v, p.vary); vy != "" {
		for _, f := range strings.Split(vy, ",") {
			if f = strings.TrimSpace(f); f != "" && !containsFold(m.vary, f) {
				m.vary = append(m.vary, f)
			}
		}
	}
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// apply writes the combined headers onto h and returns the ETag it set
// ("" for none). document and variables make a combined ETag specific
// to this query: the same operations selected differently are a
// different response.
func (m *cacheMerge) apply(h http.Header, document string, variables map[string]any) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops == 0 || m.withCC == 0 && m.withETag == 0 && m.withLM == 0 && len(m.vary) == 0 {
		return ""
	}
	if len(m.vary) > 0 {
		h.Set("Vary", strings.Join(m.vary, ", "))
	}
	switch {
	case m.failed:
		// A partial result must not be cached as if it were whole.
		h.Set("Cache-Control", "no-store")
		return ""
	case m.withCC > 0 && m.withCC < m.ops:
		// Some operation said nothing about caching; silence is not
		// permission, so the combination cannot be stored.
		h.Set("Cache-Control", "no-store")
	case m.withCC > 0 && m.sameCC:
		h.Set("Cache-Control", m.firstCC)
	case m.withCC > 0:
		h.Set("Cache-Control", m.cc.String())
	}
	if m.withLM == m.ops {
		h.Set("Last-Modified", m.lastMod.UTC().Format(http.TimeFormat))
	}
	if m.withETag != m.ops {
		return ""
	}
	etag := m.firstETag
	if m.ops > 1 {
		hs := fnv.New64a()
		_, _ = hs.Write([]byte(document))
		if len(variables) > 0 {
			b, _ := json.Marshal(variables) // map keys marshal sorted
			_, _ = hs.Write(b)
		}
		etag = `W/"` + strconv.FormatUint(m.etagSum^hs.Sum64(), 16) + `"`
	}
	h.Set("ETag", etag)
	return etag
}

// etagMatches reports whether an If-None-Match header lists etag,
// comparing weakly as RFC 9110 requires for If-None-Match.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, c := range strings.Split(ifNoneMatch, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == want {
			return true
		}
	}
	return false
}

// ccDirectives is a parsed Cache-Control, merged toward the most
// restrictive reading. Ages are -1 when absent.
type ccDirectives struct {
	noStore, noCache, private, public       bool
	mustRevalidate, proxyRevalidate, noTran bool
	immutable                               bool
	maxAge, sMaxAge, swr, sie               int
}

func parseCacheControl(s string) ccDirectives {
	d := ccDirectives{maxAge: -1, sMaxAge: -1, swr: -1, sie: -1}
	for _, part := range strings.Split(s, ",") {
		name, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		age := func() int {
			n, err := strconv.Atoi(strings.Trim(val, `"`))
			if err != nil || n < 0 {
				return 0 // unparsable age: the strict reading
			}
			return n
		}
		switch strings.ToLower(name) {
		case "no-store":
			d.noStore = true
		case "no-cache":
			d.noCache = true
		case "private":
			d.private = true
		case "public":
			d.public = true
		case "must-revalidate":
			d.mustRevalidate = true
		case "proxy-revalidate":
			d.proxyRevalidate = true
		case "no-transform":
			d.noTran = true
		case "immutable":
			d.immutable = true
		case "max-age":
			d.maxAge = age()
		case "s-maxage":
			d.sMaxAge = age()
		case "stale-while-revalidate":
			d.swr = age()
		case "stale-if-error":
			d.sie = age()
		}
	}
	return d
}

// restrict merges o into d, keeping the stricter side of each
// directive: prohibitions from either, permissions only from both,
// the shorter of two ages.
func (d *ccDirectives) restrict(o ccDirectives) {
	d.noStore = d.noStore || o.noStore
	d.noCache = d.noCache || o.noCache
	d.private = d.private || o.private
	d.public = d.public && o.public
	d.mustRevalidate = d.mustRevalidate || o.mustRevalidate
	d.proxyRevalidate = d.proxyRevalidate || o.proxyRevalidate
	d.noTran = d.noTran || o.noTran
	d.immutable = d.immutable && o.immutable
	d.maxAge = minAge(d.maxAge, o.maxAge)
	d.sMaxAge = minAge(d.sMaxAge, o.sMaxAge)
	// Grace periods are permissions: kept only when both grant them.
	d.swr = bothAge(d.swr, o.swr)
	d.sie = bothAge(d.sie, o.sie)
}

// minAge is the shorter of two ages, ignoring an absent one: an absent
// max-age leaves freshness to heuristics, which is no stricter than a
// stated age.
func minAge(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	}
	return min(a, b)
}

func bothAge(a, b int) int {
	if a < 0 || b < 0 {
		return -1
	}
	return min(a, b)
}

func (d ccDirectives) String() string {
	if d.noStore {
		return "no-store"
	}
	var parts []string
	add := func(on bool, s string) {
		if on {
			parts = append(parts, s)
		}
	}
	addAge := func(n int, name string) {
		if n >= 0 {
			parts = append(parts, name+"="+strconv.Itoa(n))
		}
	}
	add(d.noCache, "no-cache")
	add(d.private, "private")
	add(d.public && !d.private, "public")
	addAge(d.maxAge, "max-age")
	if !d.private {
		addAge(d.sMaxAge, "s-maxage")
	}
	add(d.mustRevalidate, "must-revalidate")
	add(d.proxyRevalidate && !d.private, "proxy-revalidate")
	add(d.noTran, "no-transform")
	add(d.immutable, "immutable")
	addAge(d.swr, "stale-while-revalidate")
	addAge(d.sie, "stale-if-error")
	return strings.Join(parts, ", ")
}
