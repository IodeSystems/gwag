package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Application keys.
//
// An app key lets a service act on the namespaces it lists: register,
// heartbeat, deregister and drain bindings in them over the control
// plane (enforced only with WithRequireAppKeys), and add or delete
// Schema Registry subjects of the channels those namespaces declare.
//
// The gateway mints a key (createAppKey, `gwag key create`), shows
// the secret once and stores only its SHA-256, with the namespaces
// and an expiry. A key is
//
//	gwk_<id>_<secret>
//
// with id 16 hex characters and secret 64. The id names the key in
// list, revoke, logs and metrics; it is not secret. Revoke deletes
// the record. Several keys may cover one namespace, so a new key can
// be deployed before the old one expires.
//
// Storage: in memory standalone; in cluster mode the gwag-app-keys
// KV bucket (no TTL, replicas raised with the other buckets). Every
// check reads the record, so a revoke applies on every node at the
// next call. Until the node has joined JetStream, checks answer
// unavailable.
//
// The AppKeyAuthorizer delegate (app_keys_delegate.go) is asked
// first when one is registered.

const appKeysBucketName = "gwag-app-keys"

// appKeyPrefix starts every key the gateway mints.
const appKeyPrefix = "gwk_"

// appKeyWarnBefore is how long before expiry a key is reported as
// expiring (log and metric).
const appKeyWarnBefore = 7 * 24 * time.Hour

// appKeyMaxTTL bounds a key's lifetime.
const appKeyMaxTTL = 10 * 366 * 24 * time.Hour

// appKeyIDPattern is the id form: 16 lower-case hex characters.
var appKeyIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// appKeyRecord is one key, as stored (KV JSON value).
type appKeyRecord struct {
	ID         string   `json:"id"`
	Namespaces []string `json:"namespaces"`
	// Hash is hex SHA-256 of the whole key string.
	Hash          string `json:"hash"`
	Note          string `json:"note,omitempty"`
	CreatedUnixMs int64  `json:"created_unix_ms"`
	ExpiresUnixMs int64  `json:"expires_unix_ms"`
	// CreatedBy is the authenticated identity that minted the key
	// (adminIdentityFromContext).
	CreatedBy string `json:"created_by,omitempty"`
}

func (r appKeyRecord) expires() time.Time { return time.UnixMilli(r.ExpiresUnixMs) }

// WithRequireAppKeys makes the control plane require an app key (or
// the admin token) from remote callers: Register, Heartbeat,
// Deregister and DrainInstance need a key that covers every
// namespace they touch; ForgetPeer, RetractStable, Deprecate and
// Undeprecate need the admin token. The key travels as gRPC metadata
// `authorization: Bearer <key>`. In-process calls (no gRPC peer) are
// not checked. Default: the control plane is open.
//
// Stability: experimental
func WithRequireAppKeys() Option {
	return func(cfg *config) { cfg.requireAppKeys = true }
}

// RequireAppKeys reports whether WithRequireAppKeys is set.
//
// Stability: experimental
func (g *Gateway) RequireAppKeys() bool { return g.cfg.requireAppKeys }

// Errors of an app-key check.
var (
	errAppKeyMissing     = errors.New("app key required")
	errAppKeyInvalid     = errors.New("app key is not valid")
	errAppKeyExpired     = errors.New("app key expired")
	errAppKeyDenied      = errors.New("app key denied")
	errAppKeyUnavailable = errors.New("app keys are not available yet (cluster store not ready)")
	errAppKeyNotFound    = errors.New("no app key with that id")
)

// appKeyNamespaceError is a valid key that does not cover a
// namespace of the call.
type appKeyNamespaceError struct {
	keyID   string
	missing []string
}

func (e *appKeyNamespaceError) Error() string {
	id := e.keyID
	if id == "" {
		id = "(delegate)"
	}
	return fmt.Sprintf("app key %s does not cover namespace %s", id, strings.Join(e.missing, ", "))
}

// appKeyGrant is what a valid key allows.
type appKeyGrant struct {
	id         string
	namespaces []string
	expires    time.Time // zero = unknown (delegate)
	delegate   bool
}

// covers returns nil when the grant covers every namespace in ns.
func (a appKeyGrant) covers(ns []string) error {
	var missing []string
	for _, n := range ns {
		if !containsString(a.namespaces, n) && !containsString(missing, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &appKeyNamespaceError{keyID: a.id, missing: missing}
}

// appKeys is the standalone store. mu is a leaf lock.
type appKeys struct {
	mu sync.Mutex
	m  map[string]appKeyRecord

	// warned maps a key id to when its expiry was last logged.
	warnMu sync.Mutex
	warned map[string]time.Time
}

// appKeyKV returns the cluster bucket; ok=false when there is no
// cluster. kv is nil with ok=true while the node has not joined
// JetStream yet.
func (g *Gateway) appKeyKV() (kv jetstream.KeyValue, cluster bool) {
	if g.cfg.cluster == nil {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers == nil {
		return nil, true
	}
	return g.peers.appKeys, true
}

func (g *Gateway) getAppKey(ctx context.Context, id string) (appKeyRecord, error) {
	kv, cluster := g.appKeyKV()
	if !cluster {
		g.appKeys.mu.Lock()
		defer g.appKeys.mu.Unlock()
		r, ok := g.appKeys.m[id]
		if !ok {
			return appKeyRecord{}, errAppKeyNotFound
		}
		return r, nil
	}
	if kv == nil {
		return appKeyRecord{}, errAppKeyUnavailable
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	e, err := kv.Get(kctx, id)
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return appKeyRecord{}, errAppKeyNotFound
	}
	if err != nil {
		return appKeyRecord{}, fmt.Errorf("%w: %v", errAppKeyUnavailable, err)
	}
	var r appKeyRecord
	if err := json.Unmarshal(e.Value(), &r); err != nil {
		return appKeyRecord{}, fmt.Errorf("app key %s: %w", id, err)
	}
	return r, nil
}

func (g *Gateway) putAppKey(ctx context.Context, r appKeyRecord) error {
	kv, cluster := g.appKeyKV()
	if !cluster {
		g.appKeys.mu.Lock()
		defer g.appKeys.mu.Unlock()
		if g.appKeys.m == nil {
			g.appKeys.m = map[string]appKeyRecord{}
		}
		g.appKeys.m[r.ID] = r
		return nil
	}
	if kv == nil {
		return errAppKeyUnavailable
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// Two call timeouts: room for kvPutRetry to retry a put lost to a
	// replica change. The id is new, so a repeated put is the same.
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	if err := kvPutRetry(kctx, kv, r.ID, b); err != nil {
		return fmt.Errorf("app key: kv put: %w", err)
	}
	return nil
}

// deleteAppKey removes the key. errAppKeyNotFound when there is none.
func (g *Gateway) deleteAppKey(ctx context.Context, id string) error {
	if _, err := g.getAppKey(ctx, id); err != nil {
		return err
	}
	kv, cluster := g.appKeyKV()
	if !cluster {
		g.appKeys.mu.Lock()
		defer g.appKeys.mu.Unlock()
		delete(g.appKeys.m, id)
		return nil
	}
	if kv == nil {
		return errAppKeyUnavailable
	}
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	if err := kvDeleteRetry(kctx, kv, id); err != nil {
		return fmt.Errorf("app key: kv delete: %w", err)
	}
	return nil
}

// listAppKeys returns every key, sorted by expiry then id.
func (g *Gateway) listAppKeys(ctx context.Context) ([]appKeyRecord, error) {
	var out []appKeyRecord
	kv, cluster := g.appKeyKV()
	switch {
	case !cluster:
		g.appKeys.mu.Lock()
		for _, r := range g.appKeys.m {
			out = append(out, r)
		}
		g.appKeys.mu.Unlock()
	case kv == nil:
		return nil, errAppKeyUnavailable
	default:
		kctx, cancel := kvCtx(ctx)
		defer cancel()
		keys, err := kv.Keys(kctx)
		if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, fmt.Errorf("app keys: list: %w", err)
		}
		for _, k := range keys {
			e, err := kv.Get(kctx, k)
			if err != nil {
				continue // deleted since the listing
			}
			var r appKeyRecord
			if json.Unmarshal(e.Value(), &r) == nil {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresUnixMs != out[j].ExpiresUnixMs {
			return out[i].ExpiresUnixMs < out[j].ExpiresUnixMs
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// createAppKey mints a key for namespaces, valid for ttl. Returns the
// stored record and the key; the key is not kept.
func (g *Gateway) createAppKey(ctx context.Context, namespaces []string, ttl time.Duration, note, by string) (appKeyRecord, string, error) {
	ns, err := normalizeAppKeyNamespaces(namespaces)
	if err != nil {
		return appKeyRecord{}, "", err
	}
	if ttl <= 0 {
		return appKeyRecord{}, "", errors.New("ttl must be positive")
	}
	if ttl > appKeyMaxTTL {
		return appKeyRecord{}, "", errors.New("ttl must be at most 10 years")
	}
	var idb [8]byte
	var sec [32]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return appKeyRecord{}, "", err
	}
	if _, err := rand.Read(sec[:]); err != nil {
		return appKeyRecord{}, "", err
	}
	id := hex.EncodeToString(idb[:])
	key := appKeyPrefix + id + "_" + hex.EncodeToString(sec[:])
	now := nowFunc()
	r := appKeyRecord{
		ID:            id,
		Namespaces:    ns,
		Hash:          appKeyHash(key),
		Note:          sanitizeSubject(note),
		CreatedUnixMs: now.UnixMilli(),
		ExpiresUnixMs: now.Add(ttl).UnixMilli(),
		CreatedBy:     by,
	}
	if err := g.putAppKey(ctx, r); err != nil {
		return appKeyRecord{}, "", err
	}
	return r, key, nil
}

func normalizeAppKeyNamespaces(in []string) ([]string, error) {
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if err := validateNS(n); err != nil {
			return nil, err
		}
		if !containsString(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one namespace is required")
	}
	sort.Strings(out)
	return out, nil
}

func appKeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// parseAppKey splits a minted key into its id. ok=false when tok is
// not in the minted form.
func parseAppKey(tok string) (id string, ok bool) {
	if !strings.HasPrefix(tok, appKeyPrefix) {
		return "", false
	}
	rest := tok[len(appKeyPrefix):]
	id, sec, found := strings.Cut(rest, "_")
	if !found || !appKeyIDPattern.MatchString(id) || len(sec) != 64 {
		return "", false
	}
	return id, true
}

// checkBuiltinAppKey verifies a key the gateway minted.
func (g *Gateway) checkBuiltinAppKey(ctx context.Context, tok string) (appKeyGrant, error) {
	id, ok := parseAppKey(tok)
	if !ok {
		return appKeyGrant{}, errAppKeyInvalid
	}
	r, err := g.getAppKey(ctx, id)
	if errors.Is(err, errAppKeyNotFound) {
		return appKeyGrant{}, errAppKeyInvalid
	}
	if err != nil {
		return appKeyGrant{}, err
	}
	want, err := hex.DecodeString(r.Hash)
	if err != nil {
		return appKeyGrant{}, errAppKeyInvalid
	}
	got := sha256.Sum256([]byte(tok))
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return appKeyGrant{}, errAppKeyInvalid
	}
	if !nowFunc().Before(r.expires()) {
		return appKeyGrant{}, errAppKeyExpired
	}
	return appKeyGrant{id: r.ID, namespaces: r.Namespaces, expires: r.expires()}, nil
}

// checkAppKey resolves tok: the AppKeyAuthorizer delegate first, then
// the gateway's own keys. op and namespaces are passed to the
// delegate; coverage is the caller's check (appKeyGrant.covers).
func (g *Gateway) checkAppKey(ctx context.Context, tok, op string, namespaces []string, remoteAddr string) (appKeyGrant, error) {
	if tok == "" {
		return appKeyGrant{}, errAppKeyMissing
	}
	switch outcome, grant := g.consultAppKeyDelegate(ctx, tok, op, namespaces, remoteAddr); outcome {
	case appKeyDelegateAccept:
		return grant, nil
	case appKeyDelegateReject:
		return appKeyGrant{}, errAppKeyDenied
	}
	return g.checkBuiltinAppKey(ctx, tok)
}

// appKeyOutcome is the app_key_auth_total outcome label of err.
func appKeyOutcome(err error) string {
	var nsErr *appKeyNamespaceError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &nsErr):
		return "namespace_forbidden"
	case errors.Is(err, errAppKeyMissing):
		return "missing"
	case errors.Is(err, errAppKeyExpired):
		return "expired"
	case errors.Is(err, errAppKeyDenied):
		return "denied_delegate"
	case errors.Is(err, errAppKeyUnavailable):
		return "unavailable"
	}
	return "invalid"
}

// openAppKeysBucket creates (at r replicas, see openKV) or opens the bucket. No TTL: a key
// lives until revoked; expiry is a field.
func openAppKeysBucket(ctx context.Context, js jetstream.JetStream, r int) (jetstream.KeyValue, error) {
	return openKV(ctx, js, jetstream.KeyValueConfig{Bucket: appKeysBucketName}, r)
}

// ---- expiry warning -------------------------------------------------

// appKeyWarnPeriod is how often the warning loop lists the keys.
var appKeyWarnPeriod = time.Hour

// appKeyWarnLoop logs keys that expire within appKeyWarnBefore, and
// keys that have expired, at most once a day per key on this node.
func (g *Gateway) appKeyWarnLoop() {
	t := time.NewTicker(appKeyWarnPeriod)
	defer t.Stop()
	for {
		g.warnExpiringAppKeys(g.life)
		select {
		case <-g.life.Done():
			return
		case <-t.C:
		}
	}
}

// warnExpiringAppKeys logs each key that expires within
// appKeyWarnBefore (or has expired) and was not logged in the last
// 24h. Returns the ids it logged.
func (g *Gateway) warnExpiringAppKeys(ctx context.Context) []string {
	keys, err := g.listAppKeys(ctx)
	if err != nil || len(keys) == 0 {
		return nil
	}
	now := nowFunc()
	g.appKeys.warnMu.Lock()
	defer g.appKeys.warnMu.Unlock()
	if g.appKeys.warned == nil {
		g.appKeys.warned = map[string]time.Time{}
	}
	seen := map[string]bool{}
	var logged []string
	for _, r := range keys {
		seen[r.ID] = true
		left := r.expires().Sub(now)
		if left > appKeyWarnBefore {
			continue
		}
		if last, ok := g.appKeys.warned[r.ID]; ok && now.Sub(last) < 24*time.Hour {
			continue
		}
		g.appKeys.warned[r.ID] = now
		logged = append(logged, r.ID)
		ns := strings.Join(r.Namespaces, ",")
		if left <= 0 {
			g.appKeyWarnf("app key %s (namespaces %s) expired at %s; calls with it fail. Revoke it once its replacement is deployed", r.ID, ns, r.expires().UTC().Format(time.RFC3339))
			continue
		}
		g.appKeyWarnf("app key %s (namespaces %s) expires in %s, at %s; deploy a new key before then", r.ID, ns, roundDays(left), r.expires().UTC().Format(time.RFC3339))
	}
	for id := range g.appKeys.warned {
		if !seen[id] {
			delete(g.appKeys.warned, id)
		}
	}
	return logged
}

// roundDays renders d as "3d4h" (hours rounded down), or minutes
// under an hour.
func roundDays(d time.Duration) string {
	if d < time.Hour {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	if days == 0 {
		return strconv.Itoa(hours) + "h"
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

func (g *Gateway) appKeyWarnf(format string, args ...any) {
	if cl := g.cfg.cluster; cl != nil && cl.Server != nil {
		cl.Server.Warnf("gateway: "+format, args...)
		return
	}
	log.Printf("gateway: "+format, args...)
}

// ParseAppKeyTTL parses a key lifetime: a Go duration ("720h") or a
// number of days ("30d").
//
// Stability: experimental
func ParseAppKeyTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("ttl %q: want a positive number of days (30d) or a Go duration (720h)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("ttl %q: want a positive number of days (30d) or a Go duration (720h)", s)
	}
	return d, nil
}
