package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Declaration change sinks: tell something outside the gateway when an
// external dependent (external_dependents.go) is put, reviewed or
// deleted. Two sinks, each with a full or delta mode:
//
//   - hook (WithDeclarationsHook): an HTTP POST per change. In a
//     cluster only the node that accepted the write POSTs, from a
//     queue worker, so the admin response never waits on it. Retries
//     with exponential backoff up to a bound; when they run out the
//     node logs it, counts declarations_hook_failures_total and lists
//     a declaration_hook_failed notification. The queue is in memory:
//     a delivery still pending when the node stops is lost.
//   - file (WithDeclarationsFile): every node writes its own file from
//     the replicated state, on every change it sees through the KV
//     watch (standalone: on every write). Full mode replaces the file
//     atomically (temp file + rename, mode 0644); delta mode appends
//     one JSON line per change and never rotates.
//
// Payload schemas (docs/lifecycle.md#declaration-sinks) are stable:
// gwag.declarations.full/v1 and gwag.declarations.delta/v1.
//
// seq orders changes. In a cluster it is the KV revision of the change
// (the same on every node); standalone it is a counter from 1 that
// restarts with the process, like the in-memory set.

// DeclarationsSinkMode selects what a declarations sink receives.
//
// Stability: experimental
type DeclarationsSinkMode string

const (
	// DeclarationsFull sends the whole current set on every change.
	DeclarationsFull DeclarationsSinkMode = "full"
	// DeclarationsDelta sends one record per change: op, id, before,
	// after.
	DeclarationsDelta DeclarationsSinkMode = "delta"
)

// ParseDeclarationsSinkMode reads "full" or "delta".
//
// Stability: experimental
func ParseDeclarationsSinkMode(s string) (DeclarationsSinkMode, error) {
	switch m := DeclarationsSinkMode(s); m {
	case DeclarationsFull, DeclarationsDelta:
		return m, nil
	}
	return "", fmt.Errorf("declarations sink mode %q: want full or delta", s)
}

// DeclarationsHookOptions configures WithDeclarationsHook.
//
// Stability: experimental
type DeclarationsHookOptions struct {
	// URL receives a POST per change. Required.
	URL string
	// Secret, when set, signs each POST: X-Gwag-Signature is
	// "sha256=" + hex(HMAC-SHA256(Secret, timestamp + "." + body)),
	// with timestamp the X-Gwag-Timestamp header (Unix seconds).
	// Receivers reject an old timestamp to block replay. Checked by
	// CheckSecretStrength: New panics on a secret under 32 bytes or
	// under an estimated 128 bits of entropy. Make one with
	// GenerateSecret or `openssl rand -base64 48`.
	Secret []byte
	// Mode is full or delta. Default full.
	Mode DeclarationsSinkMode
	// Client sends the POSTs. Default: a client with a 10s timeout.
	Client *http.Client
	// MaxAttempts bounds the tries per change. Default 6.
	MaxAttempts int
	// InitialBackoff is the wait after the first failed try; it
	// doubles per try up to MaxBackoff. Defaults 500ms and 8s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// QueueSize bounds the changes waiting for delivery. A change that
	// finds the queue full fails at once. Default 256.
	QueueSize int
}

// DeclarationsFileOptions configures WithDeclarationsFile.
//
// Stability: experimental
type DeclarationsFileOptions struct {
	// Path is the file this node writes. Required. Its directory must
	// exist.
	Path string
	// Mode is full (the file is the whole set, replaced atomically)
	// or delta (one JSON line appended per change). Default full.
	Mode DeclarationsSinkMode
}

// WithDeclarationsHook POSTs every external-dependent change to a URL
// (see DeclarationsHookOptions). In a cluster the node that accepted
// the write sends it, once.
//
// Stability: experimental
func WithDeclarationsHook(o DeclarationsHookOptions) Option {
	return func(cfg *config) { cfg.declHook = &o }
}

// WithDeclarationsFile keeps a file in step with the external
// dependents (see DeclarationsFileOptions). In a cluster every node
// writes its own file.
//
// Stability: experimental
func WithDeclarationsFile(o DeclarationsFileOptions) Option {
	return func(cfg *config) { cfg.declFile = &o }
}

const (
	changeOpPut    = "put"
	changeOpReview = "review"
	changeOpDelete = "delete"

	declarationsFullSchema  = "gwag.declarations.full/v1"
	declarationsDeltaSchema = "gwag.declarations.delta/v1"

	// hookSnapshotWait bounds how long a full-mode hook delivery in a
	// cluster waits for this node's watch to reach the change.
	hookSnapshotWait = 10 * time.Second
)

// declarationChange is one put, review or delete.
type declarationChange struct {
	op     string
	id     string
	before *externalDependent // nil: created
	after  *externalDependent // nil: deleted
	seq    uint64             // 0: not known yet (fill from the view)
	at     time.Time
	by     string // authenticated identity that made the change; "" unknown
}

// declarationRecord is a declaration in a sink payload.
type declarationRecord struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Kind       string             `json:"kind"`
	Owner      *externalOwnerInfo `json:"owner"`
	Depends    []externalPinInfo  `json:"depends"`
	Note       string             `json:"note"`
	ReviewAt   string             `json:"reviewAt"`
	ReviewedAt string             `json:"reviewedAt"`
	ReviewedBy string             `json:"reviewedBy"`
	UpdatedAt  string             `json:"updatedAt"`
	UpdatedBy  string             `json:"updatedBy"`
}

type declarationsFullPayload struct {
	Schema       string              `json:"schema"`
	Seq          uint64              `json:"seq"`
	At           string              `json:"at"`
	Declarations []declarationRecord `json:"declarations"`
}

type declarationsDeltaPayload struct {
	Schema string             `json:"schema"`
	Seq    uint64             `json:"seq"`
	Op     string             `json:"op"`
	ID     string             `json:"id"`
	Before *declarationRecord `json:"before"`
	After  *declarationRecord `json:"after"`
	At     string             `json:"at"`
	By     string             `json:"by"`
}

func sinkTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func toDeclarationRecord(x *externalDependent) *declarationRecord {
	if x == nil {
		return nil
	}
	r := &declarationRecord{
		ID:         x.ID,
		Name:       x.Name,
		Kind:       x.Kind,
		Depends:    []externalPinInfo{},
		Note:       x.Note,
		ReviewAt:   rfc3339Ms(x.ReviewUnixMs),
		ReviewedAt: rfc3339Ms(x.ReviewedUnixMs),
		ReviewedBy: x.ReviewedBy,
		UpdatedAt:  sinkTime(time.UnixMilli(x.UpdatedUnixMs)),
		UpdatedBy:  x.UpdatedBy,
	}
	if x.Owner != nil {
		r.Owner = &externalOwnerInfo{Team: x.Owner.Team, Contact: x.Owner.Contact}
	}
	for _, d := range x.Depends {
		r.Depends = append(r.Depends, externalPinInfo{Service: d.Service, Lines: append([]string{}, d.Lines...)})
	}
	return r
}

func deltaPayload(c declarationChange) declarationsDeltaPayload {
	return declarationsDeltaPayload{
		Schema: declarationsDeltaSchema,
		Seq:    c.seq,
		Op:     c.op,
		ID:     c.id,
		Before: toDeclarationRecord(c.before),
		After:  toDeclarationRecord(c.after),
		At:     sinkTime(c.at),
		By:     c.by,
	}
}

// ---- replicated view --------------------------------------------------

// declarationView is the declaration set as of a revision: fed by the
// KV watch in a cluster (never by the local write, so its revision
// means "every change up to here") and by the write path standalone.
// The file sink writes from it; a full-mode hook reads its snapshot.
type declarationView struct {
	mu    sync.Mutex
	m     map[string]viewEntry
	rev   uint64
	ready bool          // standalone: from New; cluster: after the watch replay
	moved chan struct{} // closed and replaced when rev or m changes
}

type viewEntry struct {
	x   externalDependent
	rev uint64
}

// wake signals waiters. Caller holds v.mu.
func (v *declarationView) wakeLocked() {
	if v.moved != nil {
		close(v.moved)
	}
	v.moved = make(chan struct{})
}

// snapshotLocked is the set as a full payload. Caller holds v.mu.
func (v *declarationView) snapshotLocked(at time.Time) declarationsFullPayload {
	p := declarationsFullPayload{Schema: declarationsFullSchema, Seq: v.rev, At: sinkTime(at), Declarations: []declarationRecord{}}
	ids := make([]string, 0, len(v.m))
	for id := range v.m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := v.m[id].x
		p.Declarations = append(p.Declarations, *toDeclarationRecord(&e))
	}
	return p
}

// applyLocal applies a standalone change: assigns its seq and writes
// the file. Returns the change with seq set.
func (v *declarationView) applyLocal(g *Gateway, c declarationChange) declarationChange {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil {
		v.m = map[string]viewEntry{}
	}
	v.rev++
	c.seq = v.rev
	if c.after != nil {
		v.m[c.id] = viewEntry{x: *c.after, rev: c.seq}
	} else {
		delete(v.m, c.id)
	}
	v.wakeLocked()
	g.declSinks.writeFileLocked(v, c)
	return c
}

// observeDeclaration applies one KV watch event (cluster). after nil = deleted.
// During the initial replay it only loads the state; caughtUp writes
// the first file.
func (g *Gateway) observeDeclaration(id string, after *externalDependent, rev uint64, created time.Time) {
	v := &g.declView
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil {
		v.m = map[string]viewEntry{}
	}
	prev, had := v.m[id]
	if rev > v.rev {
		v.rev = rev
	}
	if had && rev <= prev.rev {
		return // a replay of a value already applied
	}
	if after == nil && !had {
		v.wakeLocked()
		return // deleting what this node never had (replayed marker)
	}
	c := declarationChange{op: changeOpDelete, id: id, seq: rev, at: created}
	if had {
		b := prev.x
		c.before = &b
	}
	if after != nil {
		a := *after
		c.after = &a
		c.op = a.LastOp
		if c.op == "" {
			c.op = changeOpPut
		}
		c.at = time.UnixMilli(a.UpdatedUnixMs)
		c.by = a.UpdatedBy
		v.m[id] = viewEntry{x: a, rev: rev}
	} else {
		delete(v.m, id)
	}
	v.wakeLocked()
	if v.ready {
		g.declSinks.writeFileLocked(v, c)
	}
}

// caughtUp marks the watch replay done and writes the full file.
func (v *declarationView) caughtUp(g *Gateway) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ready {
		return
	}
	v.ready = true
	v.wakeLocked()
	g.declSinks.writeFullFileLocked(v)
}

// waitFor blocks until ok() holds (checked under v.mu), d passes or
// ctx ends. Returns the full snapshot either way.
func (v *declarationView) waitFor(ctx context.Context, d time.Duration, ok func() bool) declarationsFullPayload {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		v.mu.Lock()
		if v.moved == nil {
			v.moved = make(chan struct{})
		}
		if ok() {
			p := v.snapshotLocked(nowFunc())
			v.mu.Unlock()
			return p
		}
		ch := v.moved
		v.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			v.mu.Lock()
			p := v.snapshotLocked(nowFunc())
			v.mu.Unlock()
			return p
		case <-ctx.Done():
			v.mu.Lock()
			p := v.snapshotLocked(nowFunc())
			v.mu.Unlock()
			return p
		}
	}
}

// ---- sinks ------------------------------------------------------------

// declarationSinks holds the configured sinks. A nil *declarationSinks
// is "none configured": every method is a no-op.
type declarationSinks struct {
	g    *Gateway
	hook *DeclarationsHookOptions
	file *DeclarationsFileOptions

	queue chan declarationChange
}

func newDeclarationSinks(g *Gateway, hook *DeclarationsHookOptions, file *DeclarationsFileOptions) *declarationSinks {
	if hook == nil && file == nil {
		return nil
	}
	s := &declarationSinks{g: g}
	if hook != nil {
		h := *hook
		if h.URL == "" {
			panic("gateway: WithDeclarationsHook: URL is required")
		}
		if h.Mode == "" {
			h.Mode = DeclarationsFull
		}
		if _, err := ParseDeclarationsSinkMode(string(h.Mode)); err != nil {
			panic("gateway: WithDeclarationsHook: " + err.Error())
		}
		if len(h.Secret) > 0 {
			if err := CheckSecretStrength(h.Secret); err != nil {
				panic("gateway: WithDeclarationsHook: Secret: " + err.Error())
			}
		}
		if h.Client == nil {
			h.Client = &http.Client{Timeout: 10 * time.Second}
		}
		if h.MaxAttempts <= 0 {
			h.MaxAttempts = 6
		}
		if h.InitialBackoff <= 0 {
			h.InitialBackoff = 500 * time.Millisecond
		}
		if h.MaxBackoff <= 0 {
			h.MaxBackoff = 8 * time.Second
		}
		if h.QueueSize <= 0 {
			h.QueueSize = 256
		}
		s.hook = &h
		s.queue = make(chan declarationChange, h.QueueSize)
	}
	if file != nil {
		f := *file
		if f.Path == "" {
			panic("gateway: WithDeclarationsFile: Path is required")
		}
		if f.Mode == "" {
			f.Mode = DeclarationsFull
		}
		if _, err := ParseDeclarationsSinkMode(string(f.Mode)); err != nil {
			panic("gateway: WithDeclarationsFile: " + err.Error())
		}
		s.file = &f
	}
	return s
}

// start runs the hook worker until ctx ends; standalone, it also
// writes the first full file.
func (s *declarationSinks) start(ctx context.Context, standalone bool) {
	if s == nil {
		return
	}
	if standalone {
		v := &s.g.declView
		v.mu.Lock()
		v.ready = true
		s.writeFullFileLocked(v)
		v.mu.Unlock()
	}
	if s.hook != nil {
		go s.hookLoop(ctx)
	}
}

// declarationChanged hands a write accepted on this node to the sinks.
// clustered: the write went through the KV; the file follows the
// watch, and only the hook is fed here.
func (g *Gateway) declarationChanged(c declarationChange, clustered bool) {
	if !clustered {
		c = g.declView.applyLocal(g, c)
	}
	g.declSinks.enqueue(c)
}

func (s *declarationSinks) enqueue(c declarationChange) {
	if s == nil || s.hook == nil {
		return
	}
	select {
	case s.queue <- c:
	default:
		s.failed(c, 0, fmt.Errorf("delivery queue full (%d)", cap(s.queue)))
	}
}

// ---- file -------------------------------------------------------------

// writeFileLocked writes c to the file sink. Caller holds v.mu, which
// orders file writes.
func (s *declarationSinks) writeFileLocked(v *declarationView, c declarationChange) {
	if s == nil || s.file == nil {
		return
	}
	if s.file.Mode == DeclarationsFull {
		s.writeFullFileLocked(v)
		return
	}
	b, err := json.Marshal(deltaPayload(c))
	if err == nil {
		err = appendLine(s.file.Path, b)
	}
	if err != nil {
		slog.Warn("gwag: declarations file: append", "path", s.file.Path, "err", err)
	}
}

func (s *declarationSinks) writeFullFileLocked(v *declarationView) {
	if s == nil || s.file == nil || s.file.Mode != DeclarationsFull {
		return
	}
	b, err := json.MarshalIndent(v.snapshotLocked(nowFunc()), "", "  ")
	if err == nil {
		err = writeFileAtomic(s.file.Path, append(b, '\n'))
	}
	if err != nil {
		slog.Warn("gwag: declarations file: write", "path", s.file.Path, "err", err)
	}
}

// writeFileAtomic replaces path with b: a temp file in the same
// directory, fsynced, mode 0644, renamed over path. A reader sees the
// old file or the new one, never a part.
func writeFileAtomic(path string, b []byte) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func appendLine(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---- hook -------------------------------------------------------------

func (s *declarationSinks) hookLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if n := len(s.queue); n > 0 {
				slog.Warn("gwag: declarations hook: stopping with deliveries pending; they are lost", "pending", n)
			}
			return
		case c := <-s.queue:
			s.deliverChange(ctx, c)
		}
	}
}

// deliverChange builds the body for c and POSTs it with retries.
func (s *declarationSinks) deliverChange(ctx context.Context, c declarationChange) {
	v := &s.g.declView
	// In a cluster the view follows the watch: wait until it has the
	// change, so a full body includes every earlier write from any
	// node and seq is known.
	snap := v.waitFor(ctx, hookSnapshotWait, func() bool {
		if c.seq > 0 {
			return v.rev >= c.seq
		}
		_, has := v.m[c.id]
		return has == (c.after != nil)
	})
	if c.seq == 0 {
		c.seq = snap.Seq
	}
	var body any = deltaPayload(c)
	if s.hook.Mode == DeclarationsFull {
		body = snap
	}
	b, err := json.Marshal(body)
	if err != nil {
		s.failed(c, 0, err)
		return
	}
	attempts, err := s.post(ctx, c.op, b)
	if err != nil {
		s.failed(c, attempts, err)
		return
	}
	if s.hook.Mode == DeclarationsFull {
		// The receiver has the whole set again: clear the failures
		// this snapshot covers, from any node.
		s.g.clearHookFailures(ctx, DeclarationsFull, snap.Seq)
	}
}

// errHookPermanent marks a response retrying cannot fix (4xx but 408
// and 429).
type errHookPermanent struct{ status int }

func (e errHookPermanent) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// post sends b, retrying with exponential backoff. Returns the number
// of tries made and the last error.
func (s *declarationSinks) post(ctx context.Context, op string, b []byte) (int, error) {
	h := s.hook
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	delivery := hex.EncodeToString(id)
	backoff := h.InitialBackoff
	var err error
	for attempt := 1; ; attempt++ {
		err = s.postOnce(ctx, op, delivery, b)
		if err == nil {
			return attempt, nil
		}
		if _, perm := err.(errHookPermanent); perm || attempt >= h.MaxAttempts {
			return attempt, err
		}
		select {
		case <-ctx.Done():
			return attempt, err
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, h.MaxBackoff)
	}
}

func (s *declarationSinks) postOnce(ctx context.Context, op, delivery string, b []byte) error {
	h := s.hook
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(b))
	if err != nil {
		return errHookPermanent{}
	}
	ts := strconv.FormatInt(nowFunc().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gwag-declarations-hook")
	req.Header.Set("X-Gwag-Delivery", delivery)
	req.Header.Set("X-Gwag-Event", op)
	req.Header.Set("X-Gwag-Timestamp", ts)
	if len(h.Secret) > 0 {
		req.Header.Set("X-Gwag-Signature", "sha256="+signDeclarationsHook(h.Secret, ts, b))
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests:
		return errHookPermanent{status: resp.StatusCode}
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// signDeclarationsHook is hex(HMAC-SHA256(secret, ts + "." + body)).
func signDeclarationsHook(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// failed records a delivery that will not happen: log, counter,
// notification item.
func (s *declarationSinks) failed(c declarationChange, attempts int, err error) {
	slog.Warn("gwag: declarations hook: delivery failed", "op", c.op, "id", c.id, "seq", c.seq, "attempts", attempts, "err", err)
	s.g.lifecycle.recordHookFailure()
	detail := fmt.Sprintf("%s %s (seq %d) was not delivered to %s after %d tries: %v.", c.op, c.id, c.seq, s.hook.URL, attempts, err)
	if s.hook.Mode == DeclarationsFull {
		detail += " The next delivery that succeeds, from any node, sends the whole set and clears this."
	} else {
		detail += " The receiver missed this change. Resync it from GET /admin/external-dependents, then dismiss this item."
	}
	href := "/?select=" + externalSelectID(c.id)
	if c.after == nil {
		href = "/"
	}
	n := storedNotification{
		notificationItem: notificationItem{
			Kind:        notificationHookFailed,
			ID:          fmt.Sprintf("hook-failed-%d-%s", c.seq, c.id),
			Title:       "Declarations hook delivery failed",
			Detail:      detail,
			Since:       nowRFC3339(),
			Href:        href,
			Dismissable: true,
		},
		Seq:  c.seq,
		Mode: string(s.hook.Mode),
	}
	// Not the hook context: a stop in progress should still record it.
	ctx, cancel := context.WithTimeout(context.Background(), 3*kvCallTimeout)
	defer cancel()
	if perr := s.g.putNotification(ctx, n); perr != nil {
		slog.Warn("gwag: declarations hook: could not store the failure notification", "id", n.ID, "err", perr)
		s.g.notifs.set(n) // still show it on this node
	}
}
