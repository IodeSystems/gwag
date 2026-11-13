package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/nats-io/nats.go/jetstream"
)

// Shared state of admin sessions (admin_session.go): the key that
// signs session cookies and the revocation cutoffs. A cookie minted on
// one node must pass on every other, and a revocation must reach every
// node, so in a cluster both live in the gwag-admin-sessions bucket
// (no TTL), which exists from levelAdminSessions on. Below that level
// sessions are refused with errAdminSessionsLevel. Standalone, the key
// is <adminDataDir>/session-key and the cutoffs
// <adminDataDir>/admin-sessions.json (in memory without a data dir).

const (
	adminSessionsBucketName = "gwag-admin-sessions"
	adminSessionKeyKey      = "key"
	// adminSessionAllKey holds the cutoff for every subject.
	adminSessionAllKey = "all"
	// adminSessionDeniedKey holds the ids of sessions ended by logout
	// (DenyAdminSession), an adminExpiring map.
	adminSessionDeniedKey = "denied"
	// adminLoginCodesKey holds the pending one-time login codes, a
	// JSON map updated with compare-and-set.
	adminLoginCodesKey = "login-codes"
	// adminSessionKeyFile / adminSessionRevocationsFile are the
	// standalone files under adminDataDir.
	adminSessionKeyFile         = "session-key"
	adminSessionRevocationsFile = "admin-sessions.json"

	// AdminSessionRevokeSkew is added to the revocation time to make
	// the cutoff. A session's issued time comes from the clock of the
	// node that minted it, the cutoff from the revoking node's; the
	// allowance covers that much skew between them. Sign-in for the
	// subject is refused until the cutoff passes.
	//
	// Stability: experimental
	AdminSessionRevokeSkew = 30 * time.Second

	// AdminSessionsAll names every subject in RevokeAdminSessions.
	//
	// Stability: experimental
	AdminSessionsAll = "*"
)

// adminSessionState is the Gateway's admin session store.
type adminSessionState struct {
	mu  sync.Mutex
	kv  jetstream.KeyValue // cluster; set by applyAdminSessions
	key []byte             // cached once read or created
	// local is the standalone cutoff table, loaded on first use.
	local       *adminSessionRevocations
	localLoaded bool
	// codes are the standalone one-time login codes, in memory only:
	// a restart loses them, and a lost code fails, which is safe.
	codes adminExpiring
}

// adminExpiring maps an id to an entry that expires. It is the value
// of adminLoginCodesKey and adminSessionDeniedKey; expired entries are
// dropped on every write.
type adminExpiring map[string]adminExpiringEntry

type adminExpiringEntry struct {
	Value     []byte `json:"v"`
	ExpUnixMs int64  `json:"x"`
}

func (c adminExpiring) prune(nowMs int64) {
	if c == nil {
		return
	}
	for id, e := range c {
		if e.ExpUnixMs <= nowMs {
			delete(c, id)
		}
	}
}

// adminSessionRevocations is the standalone file format.
type adminSessionRevocations struct {
	AllUnixMs int64            `json:"all_unix_ms,omitempty"`
	Subjects  map[string]int64 `json:"subjects,omitempty"`
	// Denied are the session ids ended by logout, id → expiry unix ms.
	Denied map[string]int64 `json:"denied,omitempty"`
}

// adminSessionCutoff is the KV value of a cutoff key.
type adminSessionCutoff struct {
	Subject         string `json:"subject"`
	NotBeforeUnixMs int64  `json:"not_before_unix_ms"`
}

// errAdminSessionsLevel: the cluster runs below levelAdminSessions.
var errAdminSessionsLevel = fmt.Errorf("admin sessions need cluster feature level %d; raise it with gwag ctl cluster finalize once every node runs this version", levelAdminSessions)

// errAdminSessionsStarting: cluster tracking has not opened the bucket.
var errAdminSessionsStarting = errors.New("admin sessions: cluster tracking has not started yet; retry")

// normSubject is the form subjects are compared and stored in.
func normSubject(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func adminSessionSubjectKey(subject string) string {
	sum := sha256.Sum256([]byte(normSubject(subject)))
	return "sub." + hex.EncodeToString(sum[:])
}

// adminSessionsKV is the cluster bucket; nil standalone.
func (g *Gateway) adminSessionsKV() (jetstream.KeyValue, error) {
	if g.cfg.cluster == nil {
		return nil, nil
	}
	if !g.clusterFeatureOn(levelAdminSessions) {
		return nil, errAdminSessionsLevel
	}
	g.adminSessions.mu.Lock()
	kv := g.adminSessions.kv
	g.adminSessions.mu.Unlock()
	if kv == nil {
		return nil, errAdminSessionsStarting
	}
	return kv, nil
}

// applyAdminSessions opens (creating it if needed) the
// gwag-admin-sessions bucket. Feature level levelAdminSessions.
func applyAdminSessions(ctx context.Context, t *peerTracker) error {
	kv, err := openKV(ctx, t.js, jetstream.KeyValueConfig{Bucket: adminSessionsBucketName}, int(t.currentR.Load()))
	if err != nil {
		return err
	}
	t.gw.adminSessions.mu.Lock()
	t.gw.adminSessions.kv = kv
	t.gw.adminSessions.mu.Unlock()
	return nil
}

// AdminSessionKey returns the key that signs admin session cookies:
// one per cluster (created by the first node that asks), or per
// standalone gateway. Fails in a cluster below the feature level that
// adds the bucket, and while cluster tracking starts.
//
// Stability: experimental
func (g *Gateway) AdminSessionKey(ctx context.Context) ([]byte, error) {
	kv, err := g.adminSessionsKV()
	if err != nil {
		return nil, err
	}
	g.adminSessions.mu.Lock()
	if k := g.adminSessions.key; k != nil {
		g.adminSessions.mu.Unlock()
		return k, nil
	}
	g.adminSessions.mu.Unlock()

	var key []byte
	if kv == nil {
		if key, err = loadOrGenerateToken(g.cfg.adminDataDir, adminSessionKeyFile); err != nil {
			return nil, fmt.Errorf("admin session key: %w", err)
		}
	} else if key, err = clusterAdminSessionKey(ctx, kv); err != nil {
		return nil, err
	}
	g.adminSessions.mu.Lock()
	defer g.adminSessions.mu.Unlock()
	if g.adminSessions.key == nil {
		g.adminSessions.key = key
	}
	return g.adminSessions.key, nil
}

func clusterAdminSessionKey(ctx context.Context, kv jetstream.KeyValue) ([]byte, error) {
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	e, err := kv.Get(kctx, adminSessionKeyKey)
	if err == nil {
		return decodeSessionKey(e.Value())
	}
	if !errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("admin session key: %w", err)
	}
	key, err := generateAdminToken()
	if err != nil {
		return nil, err
	}
	if _, err := kv.Create(kctx, adminSessionKeyKey, []byte(hex.EncodeToString(key))); err != nil {
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return nil, fmt.Errorf("admin session key: %w", err)
		}
		// Another node created it first.
		e, err := kv.Get(kctx, adminSessionKeyKey)
		if err != nil {
			return nil, fmt.Errorf("admin session key: %w", err)
		}
		return decodeSessionKey(e.Value())
	}
	return key, nil
}

func decodeSessionKey(v []byte) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(string(v)))
	if err != nil || len(k) == 0 {
		return nil, errors.New("admin session key: stored value is not hex")
	}
	return k, nil
}

// AdminSessionNotBefore returns subject's revocation cutoff in unix
// ms: a session issued at or before it is not valid. 0 = none. The
// later of the subject's own cutoff and the one for every subject.
//
// Stability: experimental
func (g *Gateway) AdminSessionNotBefore(ctx context.Context, subject string) (int64, error) {
	kv, err := g.adminSessionsKV()
	if err != nil {
		return 0, err
	}
	if kv == nil {
		g.adminSessions.mu.Lock()
		defer g.adminSessions.mu.Unlock()
		rev, err := g.localRevocationsLocked()
		if err != nil {
			return 0, err
		}
		return max(rev.AllUnixMs, rev.Subjects[normSubject(subject)]), nil
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	var nb int64
	for _, k := range []string{adminSessionAllKey, adminSessionSubjectKey(subject)} {
		e, err := kv.Get(kctx, k)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("admin session cutoff: %w", err)
		}
		var c adminSessionCutoff
		if err := json.Unmarshal(e.Value(), &c); err != nil {
			return 0, fmt.Errorf("admin session cutoff: %w", err)
		}
		nb = max(nb, c.NotBeforeUnixMs)
	}
	return nb, nil
}

// RevokeAdminSessions ends every session of subject issued until now
// plus AdminSessionRevokeSkew (AdminSessionsAll: every subject's), on
// every node. Returns the cutoff in unix ms.
//
// Stability: experimental
func (g *Gateway) RevokeAdminSessions(ctx context.Context, subject string) (int64, error) {
	subject = normSubject(subject)
	if subject == "" {
		return 0, errors.New("subject is empty")
	}
	kv, err := g.adminSessionsKV()
	if err != nil {
		return 0, err
	}
	now := time.Now().Add(AdminSessionRevokeSkew).UnixMilli()
	if kv == nil {
		g.adminSessions.mu.Lock()
		defer g.adminSessions.mu.Unlock()
		rev, err := g.localRevocationsLocked()
		if err != nil {
			return 0, err
		}
		if subject == AdminSessionsAll {
			rev.AllUnixMs = now
		} else {
			if rev.Subjects == nil {
				rev.Subjects = map[string]int64{}
			}
			rev.Subjects[subject] = now
		}
		return now, g.saveLocalRevocationsLocked(rev)
	}
	key := adminSessionSubjectKey(subject)
	if subject == AdminSessionsAll {
		key = adminSessionAllKey
	}
	b, _ := json.Marshal(adminSessionCutoff{Subject: subject, NotBeforeUnixMs: now})
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	if _, err := kv.Put(kctx, key, b); err != nil {
		return 0, fmt.Errorf("admin session cutoff: %w", err)
	}
	return now, nil
}

// errAdminLoginCodeConflict: another node changed the code table
// between read and write too many times in a row.
var errAdminLoginCodeConflict = errors.New("admin login code: the code table kept changing; retry")

// PutAdminLoginCode stores value under id for ttl, for one
// TakeAdminLoginCode on any node. id must be unguessable (the caller
// hashes a random code). For short-lived handoffs such as the code of
// gwag login --sso; the table is one KV value, so keep values small
// and ttl short.
//
// Stability: experimental
func (g *Gateway) PutAdminLoginCode(ctx context.Context, id string, value []byte, ttl time.Duration) error {
	if id == "" || ttl <= 0 {
		return errors.New("admin login code: empty id or ttl")
	}
	exp := time.Now().Add(ttl).UnixMilli()
	return g.editAdminLoginCodes(ctx, func(c adminExpiring) (bool, error) {
		if _, dup := c[id]; dup {
			return false, errors.New("admin login code: id in use")
		}
		c[id] = adminExpiringEntry{Value: value, ExpUnixMs: exp}
		return true, nil
	})
}

// TakeAdminLoginCode removes the code id and returns its value. ok is
// false when there is no such code or it expired. A code is returned
// at most once, cluster-wide.
//
// Stability: experimental
func (g *Gateway) TakeAdminLoginCode(ctx context.Context, id string) (value []byte, ok bool, err error) {
	err = g.editAdminLoginCodes(ctx, func(c adminExpiring) (bool, error) {
		e, found := c[id]
		if !found {
			return false, nil
		}
		delete(c, id)
		value, ok = e.Value, true
		return true, nil
	})
	if err != nil {
		return nil, false, err
	}
	return value, ok, nil
}

// editAdminLoginCodes applies edit to the code table with expired
// entries already dropped (editExpiringKV in a cluster).
func (g *Gateway) editAdminLoginCodes(ctx context.Context, edit func(adminExpiring) (bool, error)) error {
	kv, err := g.adminSessionsKV()
	if err != nil {
		return err
	}
	if kv == nil {
		g.adminSessions.mu.Lock()
		defer g.adminSessions.mu.Unlock()
		if g.adminSessions.codes == nil {
			g.adminSessions.codes = adminExpiring{}
		}
		g.adminSessions.codes.prune(time.Now().UnixMilli())
		_, err := edit(g.adminSessions.codes)
		return err
	}
	return editExpiringKV(ctx, kv, adminLoginCodesKey, edit)
}

// editExpiringKV applies edit to the adminExpiring map at key with
// expired entries dropped. edit returns whether to write. The write is
// a compare-and-set on the KV revision, retried on conflict; edit then
// runs again on the fresh map.
func editExpiringKV(ctx context.Context, kv jetstream.KeyValue, key string, edit func(adminExpiring) (bool, error)) error {
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	for range 8 {
		codes := adminExpiring{}
		var rev uint64
		e, err := kv.Get(kctx, key)
		switch {
		case err == nil:
			rev = e.Revision()
			if err := json.Unmarshal(e.Value(), &codes); err != nil {
				return fmt.Errorf("admin sessions %s: %w", key, err)
			}
		case errors.Is(err, jetstream.ErrKeyNotFound):
		default:
			return fmt.Errorf("admin sessions %s: %w", key, err)
		}
		codes.prune(time.Now().UnixMilli())
		write, err := edit(codes)
		if err != nil || !write {
			return err
		}
		b, _ := json.Marshal(codes)
		if rev == 0 {
			_, err = kv.Create(kctx, key, b)
		} else {
			_, err = kv.Update(kctx, key, b, rev)
		}
		if err == nil {
			return nil
		}
		// A lost compare-and-set is an API error (key exists, wrong
		// last sequence); anything else is final.
		var apiErr *jetstream.APIError
		if !errors.As(err, &apiErr) && !errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("admin sessions %s: %w", key, err)
		}
	}
	return errAdminLoginCodeConflict
}

// DenyAdminSession ends the one session id (a logout), on every node,
// until it expires at until; other sessions of the same subject are
// not touched. Entries past until are dropped on the next write.
//
// Stability: experimental
func (g *Gateway) DenyAdminSession(ctx context.Context, id string, until time.Time) error {
	if id == "" {
		return errors.New("session id is empty")
	}
	kv, err := g.adminSessionsKV()
	if err != nil {
		return err
	}
	now, exp := time.Now().UnixMilli(), until.UnixMilli()
	if exp <= now {
		return nil
	}
	if kv == nil {
		g.adminSessions.mu.Lock()
		defer g.adminSessions.mu.Unlock()
		rev, err := g.localRevocationsLocked()
		if err != nil {
			return err
		}
		for k, x := range rev.Denied {
			if x <= now {
				delete(rev.Denied, k)
			}
		}
		if rev.Denied == nil {
			rev.Denied = map[string]int64{}
		}
		rev.Denied[id] = exp
		return g.saveLocalRevocationsLocked(rev)
	}
	return editExpiringKV(ctx, kv, adminSessionDeniedKey, func(m adminExpiring) (bool, error) {
		m[id] = adminExpiringEntry{ExpUnixMs: exp}
		return true, nil
	})
}

// AdminSessionDenied reports whether DenyAdminSession ended id.
//
// Stability: experimental
func (g *Gateway) AdminSessionDenied(ctx context.Context, id string) (bool, error) {
	kv, err := g.adminSessionsKV()
	if err != nil {
		return false, err
	}
	now := time.Now().UnixMilli()
	if kv == nil {
		g.adminSessions.mu.Lock()
		defer g.adminSessions.mu.Unlock()
		rev, err := g.localRevocationsLocked()
		if err != nil {
			return false, err
		}
		return rev.Denied[id] > now, nil
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	e, err := kv.Get(kctx, adminSessionDeniedKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("admin session denylist: %w", err)
	}
	m := adminExpiring{}
	if err := json.Unmarshal(e.Value(), &m); err != nil {
		return false, fmt.Errorf("admin session denylist: %w", err)
	}
	return m[id].ExpUnixMs > now, nil
}

// localRevocationsLocked loads the standalone table once. Caller
// holds g.adminSessions.mu.
func (g *Gateway) localRevocationsLocked() (*adminSessionRevocations, error) {
	st := &g.adminSessions
	if st.localLoaded {
		return st.local, nil
	}
	rev := &adminSessionRevocations{}
	if dir := g.cfg.adminDataDir; dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, adminSessionRevocationsFile))
		switch {
		case err == nil:
			if err := json.Unmarshal(b, rev); err != nil {
				return nil, fmt.Errorf("%s: %w", adminSessionRevocationsFile, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("read %s: %w", adminSessionRevocationsFile, err)
		}
	}
	st.local, st.localLoaded = rev, true
	return rev, nil
}

func (g *Gateway) saveLocalRevocationsLocked(rev *adminSessionRevocations) error {
	dir := g.cfg.adminDataDir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(rev)
	path := filepath.Join(dir, adminSessionRevocationsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// ---- admin operation ----------------------------------------------

type revokeAdminSessionsIn struct {
	Body struct {
		Subject string `json:"subject" minLength:"1" doc:"Email the sessions belong to, or * for every subject."`
	}
}

type revokeAdminSessionsOut struct {
	Body struct {
		Subject         string `json:"subject"`
		NotBeforeUnixMs int64  `json:"notBeforeUnixMs" doc:"Sessions issued at or before this time (unix ms) are refused."`
	}
}

func registerAdminSessionRoutes(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "revokeAdminSessions",
		Method:      http.MethodPost,
		Path:        "/admin/sessions/revoke",
		Summary:     "End every SSO session of a subject (an email; * for everyone) issued until 30 s from now (allowance for clock skew between nodes), on every node. Their next request is anonymous; signing in works again after the 30 s. Admin only.",
	}, func(ctx context.Context, in *revokeAdminSessionsIn) (*revokeAdminSessionsOut, error) {
		nb, err := g.RevokeAdminSessions(ctx, in.Body.Subject)
		if err != nil {
			switch {
			case errors.Is(err, errAdminSessionsLevel):
				return nil, huma.Error409Conflict(err.Error())
			case errors.Is(err, errAdminSessionsStarting):
				return nil, huma.Error503ServiceUnavailable(err.Error())
			}
			return nil, err
		}
		out := &revokeAdminSessionsOut{}
		out.Body.Subject = normSubject(in.Body.Subject)
		out.Body.NotBeforeUnixMs = nb
		return out, nil
	})
}
