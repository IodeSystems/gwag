package srserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Keys of the default context ("."):
//
//	id.<n>          schemaRec of schema id n (immutable once written)
//	key.<hex>       the first id holding a schema with that key
//	subj.<b64>      subjectRec: the subject's versions
//	cfg.<b64>       the subject's compatibility level
//	config          the global compatibility level
//	next            the highest id allocated or imported (decimal)
//
// Another context (".corp") has the same keys under
// ctx.<b64 of the context name>. (config there is the context's
// level). Each context is its own id space, as in Confluent: id 1 of
// ".corp" and id 1 of "." are different schemas.
//
// <b64> is unpadded URL-safe base64: subjects and context names may
// hold any character, KV keys may not.
const (
	keyConfig = "config"
	keyNext   = "next"
	ctxPrefix = "ctx."
)

// DefaultContext is the context of unqualified subjects.
const DefaultContext = "."

func (s *Store) key(k string) string           { return s.pfx + k }
func (s *Store) idKey(id int) string           { return s.pfx + "id." + strconv.Itoa(id) }
func (s *Store) hashKey(k string) string       { return s.pfx + "key." + k }
func (s *Store) subjKey(subject string) string { return s.pfx + "subj." + b64(subject) }
func (s *Store) cfgKey(subject string) string  { return s.pfx + "cfg." + b64(subject) }
func b64(s string) string                      { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

type subjectRec struct {
	Versions []versionRec `json:"versions"`
}

type versionRec struct {
	Version int  `json:"version"`
	ID      int  `json:"id"`
	Deleted bool `json:"deleted,omitempty"`
}

// latest returns the highest version (deleted too when deleted).
func (s *subjectRec) latest(deleted bool) (versionRec, bool) {
	var out versionRec
	found := false
	for _, v := range s.Versions {
		if v.Deleted && !deleted {
			continue
		}
		if !found || v.Version > out.Version {
			out, found = v, true
		}
	}
	return out, found
}

func (s *subjectRec) version(n int, deleted bool) (versionRec, bool) {
	for _, v := range s.Versions {
		if v.Version == n && (deleted || !v.Deleted) {
			return v, true
		}
	}
	return versionRec{}, false
}

func (s *subjectRec) live(deleted bool) []versionRec {
	var out []versionRec
	for _, v := range s.Versions {
		if deleted || !v.Deleted {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// Store is the registry state of one context over a KV. Context
// returns the store of another context over the same KV.
type Store struct {
	kv   KV
	name string // context name: "." or ".corp"
	pfx  string // key prefix: "" or "ctx.<b64>."
	c    *storeCache
}

type storeCache struct {
	mu  sync.Mutex
	ids map[string]*schemaRec // by id key; a schema id never changes
}

// NewStore returns the default context's store over kv.
func NewStore(kv KV) *Store {
	return &Store{kv: kv, name: DefaultContext, c: &storeCache{ids: map[string]*schemaRec{}}}
}

// NormContext normalizes a context name as Confluent does ("corp" is
// ".corp"; "" and "." are the default context). A name may not hold
// ':' (it ends a qualified subject's context), '*' or '/'.
func NormContext(name string) (string, error) {
	if name == "" || name == DefaultContext {
		return DefaultContext, nil
	}
	if !strings.HasPrefix(name, ".") {
		name = "." + name
	}
	if strings.ContainsAny(name, ":*/") || name == ".." {
		return "", errf(codeInvalidSubject, "Invalid context name %q", name)
	}
	return name, nil
}

// Context returns the store of context name (normalized and valid:
// see NormContext).
func (s *Store) Context(name string) *Store {
	if name == "" || name == DefaultContext {
		return &Store{kv: s.kv, name: DefaultContext, c: s.c}
	}
	return &Store{kv: s.kv, name: name, pfx: ctxPrefix + b64(name) + ".", c: s.c}
}

// Name is the context's name.
func (s *Store) Name() string { return s.name }

// Default reports whether this is the default context.
func (s *Store) Default() bool { return s.name == DefaultContext }

// Qualify returns subject as clients outside the context name it:
// ":.corp:subject" in context .corp, subject in the default one.
func (s *Store) Qualify(subject string) string {
	if s.Default() {
		return subject
	}
	return ":" + s.name + ":" + subject
}

// At splits a qualified subject (":.corp:s", ":.:s") into the store
// of its context and the bare subject. An unqualified subject is in
// s's context.
func (s *Store) At(subject string) (*Store, string, error) {
	if !strings.HasPrefix(subject, ":") {
		return s, subject, nil
	}
	name, rest, ok := strings.Cut(subject[1:], ":")
	if !ok {
		return s, subject, nil
	}
	name, err := NormContext(name)
	if err != nil {
		return nil, "", err
	}
	return s.Context(name), rest, nil
}

// Contexts lists "." and every context that holds a key, sorted.
func (s *Store) Contexts(ctx context.Context) ([]string, error) {
	keys, err := s.kv.Keys(ctx, ctxPrefix)
	if err != nil {
		return nil, storeErr(err)
	}
	seen := map[string]bool{DefaultContext: true}
	out := []string{DefaultContext}
	for _, k := range keys {
		enc, _, _ := strings.Cut(strings.TrimPrefix(k, ctxPrefix), ".")
		b, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || seen[string(b)] {
			continue
		}
		seen[string(b)] = true
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out, nil
}

// Error is a registry error with a Confluent error code.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Status is the HTTP status of the code.
func (e *Error) Status() int { return e.Code / 100 }

// Confluent error codes.
const (
	codeUnauthorized          = 40101
	codeForbidden             = 40301
	codeSubjectNotFound       = 40401
	codeVersionNotFound       = 40402
	codeSchemaNotFound        = 40403
	codeSubjectSoftDeleted    = 40404
	codeSubjectNotSoftDeleted = 40405
	codeVersionSoftDeleted    = 40406
	codeVersionNotSoftDeleted = 40407
	codeSubjectLevelNoConfig  = 40408
	codeConflict              = 40901
	codeInvalidSchema         = 42201
	codeInvalidVersion        = 42202
	codeInvalidSubject        = 42208
	codeInvalidCompatibility  = 42203
	codeOperationNotPermitted = 42205
	codeReferenceExists       = 42206
	codeStore                 = 50001
	codeNotReady              = 50003
)

func errf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func storeErr(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return errf(codeStore, "store: %v", err)
}

func (s *Store) getJSON(ctx context.Context, key string, v any) (uint64, error) {
	b, rev, err := s.kv.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return rev, nil
}

// createOrSame creates key with val. A key that already holds exactly
// val is success: a create whose answer was lost and was sent again.
// Otherwise ErrConflict.
func (s *Store) createOrSame(ctx context.Context, key string, val []byte) error {
	_, err := s.kv.Create(ctx, key, val)
	if !errors.Is(err, ErrConflict) {
		return err
	}
	cur, _, gerr := s.kv.Get(ctx, key)
	if gerr == nil && string(cur) == string(val) {
		return nil
	}
	return ErrConflict
}

// schema returns schema id, or ErrNotFound.
func (s *Store) schema(ctx context.Context, id int) (*schemaRec, error) {
	s.c.mu.Lock()
	r := s.c.ids[s.idKey(id)]
	s.c.mu.Unlock()
	if r != nil {
		return r, nil
	}
	r = &schemaRec{}
	if _, err := s.getJSON(ctx, s.idKey(id), r); err != nil {
		return nil, err
	}
	s.c.mu.Lock()
	s.c.ids[s.idKey(id)] = r
	s.c.mu.Unlock()
	return r, nil
}

func (s *Store) subject(ctx context.Context, subject string) (*subjectRec, uint64, error) {
	rec := &subjectRec{}
	rev, err := s.getJSON(ctx, s.subjKey(subject), rec)
	return rec, rev, err
}

// localRefs drops the qualifier of references to s's own context
// (":.corp:x" is "x" in .corp), so a schema matches however a client
// names them.
func (s *Store) localRefs(refs []sr.Reference) []sr.Reference {
	if len(refs) == 0 {
		return refs
	}
	out := make([]sr.Reference, len(refs))
	for i, r := range refs {
		if at, subj, err := s.At(r.Subject); err == nil && at.name == s.name {
			r.Subject = subj
		}
		out[i] = r
	}
	return out
}

// resolve is the resolver over the store. A reference names a
// subject of the same context, or a qualified one of another.
func (s *Store) resolve(ctx context.Context, subject string, version int) (*schemaRec, error) {
	st, subject, err := s.At(subject)
	if err != nil {
		return nil, err
	}
	_, rec, err := st.Version(ctx, subject, version, true)
	return rec, err
}

// SchemaByID returns schema id.
func (s *Store) SchemaByID(ctx context.Context, id int) (*schemaRec, error) {
	r, err := s.schema(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, errf(codeSchemaNotFound, "Schema %d not found", id)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	return r, nil
}

// Subjects lists the subjects with a live version (any version when
// deleted), sorted.
func (s *Store) Subjects(ctx context.Context, deleted bool) ([]string, error) {
	keys, err := s.kv.Keys(ctx, s.pfx+"subj.")
	if err != nil {
		return nil, storeErr(err)
	}
	out := []string{}
	for _, k := range keys {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(k, s.pfx+"subj."))
		if err != nil {
			continue
		}
		rec, _, err := s.subject(ctx, string(b))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, storeErr(err)
		}
		if len(rec.live(deleted)) > 0 {
			out = append(out, string(b))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Versions lists the version numbers of subject.
func (s *Store) Versions(ctx context.Context, subject string, deleted bool) ([]int, error) {
	rec, _, err := s.subject(ctx, subject)
	if errors.Is(err, ErrNotFound) {
		return nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	out := []int{}
	for _, v := range rec.live(deleted) {
		out = append(out, v.Version)
	}
	if len(out) == 0 {
		return nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
	}
	return out, nil
}

// Version returns version (-1: the latest) of subject and its schema.
func (s *Store) Version(ctx context.Context, subject string, version int, deleted bool) (versionRec, *schemaRec, error) {
	rec, _, err := s.subject(ctx, subject)
	if errors.Is(err, ErrNotFound) || (err == nil && len(rec.live(deleted)) == 0) {
		return versionRec{}, nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
	}
	if err != nil {
		return versionRec{}, nil, storeErr(err)
	}
	var v versionRec
	var ok bool
	if version == -1 {
		v, ok = rec.latest(deleted)
	} else {
		v, ok = rec.version(version, deleted)
	}
	if !ok {
		return versionRec{}, nil, errf(codeVersionNotFound, "Version %d not found.", version)
	}
	sch, err := s.schema(ctx, v.ID)
	if err != nil {
		return versionRec{}, nil, storeErr(fmt.Errorf("subject %s version %d: schema %d: %w", subject, v.Version, v.ID, err))
	}
	return v, sch, nil
}

// SubjectVersion is one subject version.
type SubjectVersion struct {
	Subject string `json:"subject"`
	Version int    `json:"version"`
}

// walk calls fn for every subject version (deleted ones when deleted).
func (s *Store) walk(ctx context.Context, deleted bool, fn func(subject string, v versionRec) error) error {
	subjects, err := s.Subjects(ctx, deleted)
	if err != nil {
		return err
	}
	for _, subj := range subjects {
		rec, _, err := s.subject(ctx, subj)
		if err != nil {
			return storeErr(err)
		}
		for _, v := range rec.live(deleted) {
			if err := fn(subj, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// IDVersions lists the subject versions that hold schema id.
func (s *Store) IDVersions(ctx context.Context, id int, deleted bool) ([]SubjectVersion, error) {
	if _, err := s.SchemaByID(ctx, id); err != nil {
		return nil, err
	}
	out := []SubjectVersion{}
	err := s.walk(ctx, deleted, func(subj string, v versionRec) error {
		if v.ID == id {
			out = append(out, SubjectVersion{subj, v.Version})
		}
		return nil
	})
	return out, err
}

// ReferencedBy lists the ids of schemas that reference version of
// subject.
func (s *Store) ReferencedBy(ctx context.Context, subject string, version int) ([]int, error) {
	v, _, err := s.Version(ctx, subject, version, false)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	out := []int{}
	err = s.walk(ctx, false, func(_ string, sv versionRec) error {
		if seen[sv.ID] {
			return nil
		}
		seen[sv.ID] = true
		r, err := s.schema(ctx, sv.ID)
		if err != nil {
			return storeErr(err)
		}
		for _, ref := range r.References {
			if ref.Subject == subject && ref.Version == v.Version {
				out = append(out, sv.ID)
				break
			}
		}
		return nil
	})
	sort.Ints(out)
	return out, err
}

// Lookup finds in under subject: the version holding the same schema.
func (s *Store) Lookup(ctx context.Context, subject string, in sr.Schema, deleted bool) (versionRec, *schemaRec, error) {
	rec, _, err := s.subject(ctx, subject)
	if errors.Is(err, ErrNotFound) || (err == nil && len(rec.live(deleted)) == 0) {
		return versionRec{}, nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
	}
	if err != nil {
		return versionRec{}, nil, storeErr(err)
	}
	in.References = s.localRefs(in.References)
	want, err := prepare(ctx, in, s.resolve)
	if err != nil {
		return versionRec{}, nil, errf(codeInvalidSchema, "Invalid schema: %v", err)
	}
	// Two versions can hold the same schema in different forms
	// (Confluent keeps the text it was sent). As Confluent answers: a
	// version with the identical text, else the oldest equivalent one.
	vs := rec.live(deleted)
	schemas := make([]*schemaRec, len(vs))
	for i, v := range vs {
		if schemas[i], err = s.schema(ctx, v.ID); err != nil {
			return versionRec{}, nil, storeErr(err)
		}
		if identical(schemas[i], want) {
			return v, schemas[i], nil
		}
	}
	for i, v := range vs {
		if matches(schemas[i], want) {
			return v, schemas[i], nil
		}
	}
	return versionRec{}, nil, errf(codeSchemaNotFound, "Schema not found")
}

// Register registers in under subject and returns its id and version.
// A schema the subject already holds returns that version; a schema
// another subject holds keeps its id.
func (s *Store) Register(ctx context.Context, subject string, in sr.Schema) (versionRec, error) {
	if subject == "" {
		return versionRec{}, errf(codeInvalidSchema, "empty subject")
	}
	in.References = s.localRefs(in.References)
	want, err := prepare(ctx, in, s.resolve)
	if err != nil {
		return versionRec{}, errf(codeInvalidSchema, "Invalid schema: %v", err)
	}
	if v, ok, err := s.findInSubject(ctx, subject, want); err != nil || ok {
		return v, err
	}
	id, err := s.idFor(ctx, want)
	if err != nil {
		return versionRec{}, storeErr(err)
	}
	return s.addVersion(ctx, subject, id)
}

func (s *Store) findInSubject(ctx context.Context, subject string, want *schemaRec) (versionRec, bool, error) {
	rec, _, err := s.subject(ctx, subject)
	if errors.Is(err, ErrNotFound) {
		return versionRec{}, false, nil
	}
	if err != nil {
		return versionRec{}, false, storeErr(err)
	}
	for _, v := range rec.live(false) {
		sch, err := s.schema(ctx, v.ID)
		if err != nil {
			return versionRec{}, false, storeErr(err)
		}
		if matches(sch, want) {
			return v, true, nil
		}
	}
	return versionRec{}, false, nil
}

// idFor returns the id of the schema with want's key, allocating one
// when there is none.
func (s *Store) idFor(ctx context.Context, want *schemaRec) (int, error) {
	if id, ok, err := s.idByKey(ctx, want); err != nil || ok {
		return id, err
	}
	for {
		id, err := s.allocate(ctx)
		if err != nil {
			return 0, err
		}
		rec := *want
		rec.ID = id
		b, _ := json.Marshal(&rec)
		err = s.createOrSame(ctx, s.idKey(id), b)
		if errors.Is(err, ErrConflict) {
			continue // an import took the id
		}
		if err != nil {
			return 0, err
		}
		_, err = s.kv.Create(ctx, s.hashKey(want.Keys[0]), []byte(strconv.Itoa(id)))
		if errors.Is(err, ErrConflict) {
			// Another node registered the same schema meanwhile; use its
			// id. Ours stays unused.
			if other, ok, err := s.idByKey(ctx, want); err != nil || ok {
				return other, err
			}
			return id, nil
		}
		return id, err
	}
}

func (s *Store) idByKey(ctx context.Context, want *schemaRec) (int, bool, error) {
	for _, k := range want.Keys {
		b, _, err := s.kv.Get(ctx, s.hashKey(k))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		id, err := strconv.Atoi(string(b))
		if err != nil {
			continue
		}
		sch, err := s.schema(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return 0, false, err
		}
		if matches(sch, want) {
			return id, true, nil
		}
	}
	return 0, false, nil
}

// allocate takes the next id with a compare-and-set on the counter.
func (s *Store) allocate(ctx context.Context) (int, error) {
	for {
		b, rev, err := s.kv.Get(ctx, s.key(keyNext))
		cur := 0
		switch {
		case errors.Is(err, ErrNotFound):
			rev = 0
		case err != nil:
			return 0, err
		default:
			if cur, err = strconv.Atoi(string(b)); err != nil {
				return 0, fmt.Errorf("next: %w", err)
			}
		}
		id := cur + 1
		val := []byte(strconv.Itoa(id))
		if rev == 0 {
			_, err = s.kv.Create(ctx, s.key(keyNext), val)
		} else {
			_, err = s.kv.Update(ctx, s.key(keyNext), val, rev)
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return id, nil
	}
}

// raiseNext sets the counter to at least id.
func (s *Store) raiseNext(ctx context.Context, id int) error {
	for {
		b, rev, err := s.kv.Get(ctx, s.key(keyNext))
		cur := 0
		switch {
		case errors.Is(err, ErrNotFound):
			rev = 0
		case err != nil:
			return err
		default:
			cur, _ = strconv.Atoi(string(b))
		}
		if cur >= id {
			return nil
		}
		val := []byte(strconv.Itoa(id))
		if rev == 0 {
			_, err = s.kv.Create(ctx, s.key(keyNext), val)
		} else {
			_, err = s.kv.Update(ctx, s.key(keyNext), val, rev)
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
}

// addVersion appends id to subject as its next version, unless a live
// version already holds it.
func (s *Store) addVersion(ctx context.Context, subject string, id int) (versionRec, error) {
	for {
		rec, rev, err := s.subject(ctx, subject)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return versionRec{}, storeErr(err)
		}
		next := 1
		for _, v := range rec.Versions {
			if v.ID == id && !v.Deleted {
				return v, nil
			}
			next = max(next, v.Version+1)
		}
		v := versionRec{Version: next, ID: id}
		rec.Versions = append(rec.Versions, v)
		b, _ := json.Marshal(rec)
		if errors.Is(err, ErrNotFound) {
			_, err = s.kv.Create(ctx, s.subjKey(subject), b)
		} else {
			_, err = s.kv.Update(ctx, s.subjKey(subject), b, rev)
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return versionRec{}, storeErr(err)
		}
		return v, nil
	}
}

// Compatibility levels.
var levels = map[string]bool{
	"NONE": true, "BACKWARD": true, "BACKWARD_TRANSITIVE": true, "FORWARD": true,
	"FORWARD_TRANSITIVE": true, "FULL": true, "FULL_TRANSITIVE": true,
}

// DefaultLevel is the global level when none is set (Confluent's).
const DefaultLevel = "BACKWARD"

// Config returns the level of subject ("": the context's level; in
// the default context the global level, BACKWARD when none is set).
// ok is false when none is set there.
func (s *Store) Config(ctx context.Context, subject string) (string, bool, error) {
	key := s.key(keyConfig)
	if subject != "" {
		key = s.cfgKey(subject)
	}
	b, _, err := s.kv.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		if subject == "" && s.Default() {
			return DefaultLevel, true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, storeErr(err)
	}
	return string(b), true, nil
}

// Level is the level that applies to subject: its own, else the
// context's, else the global one.
func (s *Store) Level(ctx context.Context, subject string) (string, error) {
	l, ok, err := s.Config(ctx, subject)
	if err != nil || ok {
		return l, err
	}
	if subject != "" && !s.Default() {
		if l, ok, err = s.Config(ctx, ""); err != nil || ok {
			return l, err
		}
	}
	l, _, err = s.Context(DefaultContext).Config(ctx, "")
	return l, err
}

// SetConfig sets the level of subject ("": the context's level, the
// global one in the default context).
func (s *Store) SetConfig(ctx context.Context, subject, level string) error {
	level = strings.ToUpper(level)
	if !levels[level] {
		return errf(codeInvalidCompatibility, "Invalid compatibility level: %s", level)
	}
	key := s.key(keyConfig)
	if subject != "" {
		key = s.cfgKey(subject)
	}
	if _, err := s.kv.Put(ctx, key, []byte(level)); err != nil {
		return storeErr(err)
	}
	return nil
}

// DeleteSubject deletes every version of subject, as Confluent's
// DELETE /subjects/{subject}: soft (the versions stay readable with
// ?deleted=true) unless permanent, which needs the soft delete first
// and removes them. Returns the version numbers deleted. Refused
// (42206) while another schema references one of them.
func (s *Store) DeleteSubject(ctx context.Context, subject string, permanent bool) ([]int, error) {
	for {
		rec, rev, err := s.subject(ctx, subject)
		if errors.Is(err, ErrNotFound) || (err == nil && len(rec.Versions) == 0) {
			return nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
		}
		if err != nil {
			return nil, storeErr(err)
		}
		live := rec.live(false)
		var target []versionRec
		switch {
		case !permanent && len(live) == 0:
			return nil, errf(codeSubjectSoftDeleted, "Subject '%s' was soft deleted.Set permanent=true to delete permanently", subject)
		case !permanent:
			target = live
		case len(live) > 0:
			return nil, errf(codeSubjectNotSoftDeleted, "Subject '%s' was not deleted first before being permanently deleted", subject)
		default:
			target = rec.live(true)
		}
		for _, v := range target {
			if err := s.refuseReferenced(ctx, subject, v.Version); err != nil {
				return nil, err
			}
		}
		out := make([]int, 0, len(target))
		for _, v := range target {
			out = append(out, v.Version)
		}
		if permanent {
			err = s.kv.Delete(ctx, s.subjKey(subject), rev)
			if errors.Is(err, ErrConflict) {
				continue
			}
			if err != nil {
				return nil, storeErr(err)
			}
			if err := s.kv.Delete(ctx, s.cfgKey(subject), 0); err != nil {
				return nil, storeErr(err)
			}
			if err := s.dropUnused(ctx, target); err != nil {
				return nil, err
			}
			return out, nil
		}
		for i := range rec.Versions {
			rec.Versions[i].Deleted = true
		}
		b, _ := json.Marshal(rec)
		_, err = s.kv.Update(ctx, s.subjKey(subject), b, rev)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return nil, storeErr(err)
		}
		return out, nil
	}
}

// DeleteVersion deletes one version of subject (-1: the latest), as
// Confluent's DELETE /subjects/{subject}/versions/{version}: soft
// unless permanent, which needs the soft delete first. Returns the
// version number. Refused (42206) while another schema references it.
func (s *Store) DeleteVersion(ctx context.Context, subject string, version int, permanent bool) (int, error) {
	for {
		rec, rev, err := s.subject(ctx, subject)
		if errors.Is(err, ErrNotFound) || (err == nil && len(rec.Versions) == 0) {
			return 0, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
		}
		if err != nil {
			return 0, storeErr(err)
		}
		var v versionRec
		var ok bool
		if version == -1 {
			v, ok = rec.latest(permanent)
		} else {
			v, ok = rec.version(version, true)
		}
		if !ok {
			return 0, errf(codeVersionNotFound, "Version %d not found.", version)
		}
		switch {
		case !permanent && v.Deleted:
			return 0, errf(codeVersionSoftDeleted, "Subject '%s' Version %d was soft deleted.Set permanent=true to delete permanently", subject, v.Version)
		case permanent && !v.Deleted:
			return 0, errf(codeVersionNotSoftDeleted, "Subject '%s' Version %d was not deleted first before being permanently deleted", subject, v.Version)
		}
		if err := s.refuseReferenced(ctx, subject, v.Version); err != nil {
			return 0, err
		}
		var keep []versionRec
		for _, x := range rec.Versions {
			switch {
			case x.Version != v.Version:
				keep = append(keep, x)
			case !permanent:
				x.Deleted = true
				keep = append(keep, x)
			}
		}
		if permanent && len(keep) == 0 {
			err = s.kv.Delete(ctx, s.subjKey(subject), rev)
		} else {
			rec.Versions = keep
			b, _ := json.Marshal(rec)
			_, err = s.kv.Update(ctx, s.subjKey(subject), b, rev)
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return 0, storeErr(err)
		}
		if permanent {
			if len(keep) == 0 {
				if err := s.kv.Delete(ctx, s.cfgKey(subject), 0); err != nil {
					return 0, storeErr(err)
				}
			}
			if err := s.dropUnused(ctx, []versionRec{v}); err != nil {
				return 0, err
			}
		}
		return v.Version, nil
	}
}

// refuseReferenced returns 42206 when a schema (in a live or a
// soft-deleted version) references version of subject.
func (s *Store) refuseReferenced(ctx context.Context, subject string, version int) error {
	seen := map[int]bool{}
	return s.walk(ctx, true, func(_ string, sv versionRec) error {
		if seen[sv.ID] {
			return nil
		}
		seen[sv.ID] = true
		r, err := s.schema(ctx, sv.ID)
		if err != nil {
			return storeErr(err)
		}
		for _, ref := range r.References {
			if ref.Subject == subject && ref.Version == version {
				return errf(codeReferenceExists, "One or more references exist to the schema {magic=1,keytype=SCHEMA,subject=%s,version=%d}.", subject, version)
			}
		}
		return nil
	})
}

// dropUnused removes the schema ids of removed versions that no
// subject version (live or soft-deleted) holds any more, with their
// lookup keys. The id counter is not lowered: ids are not reused.
func (s *Store) dropUnused(ctx context.Context, removed []versionRec) error {
	ids := map[int]bool{}
	for _, v := range removed {
		ids[v.ID] = true
	}
	err := s.walk(ctx, true, func(_ string, sv versionRec) error {
		delete(ids, sv.ID)
		return nil
	})
	if err != nil {
		return err
	}
	for id := range ids {
		rec, err := s.schema(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return storeErr(err)
		}
		for _, k := range rec.Keys {
			b, rev, err := s.kv.Get(ctx, s.hashKey(k))
			if err == nil && string(b) == strconv.Itoa(id) {
				if err := s.kv.Delete(ctx, s.hashKey(k), rev); err != nil && !errors.Is(err, ErrConflict) {
					return storeErr(err)
				}
			}
		}
		if err := s.kv.Delete(ctx, s.idKey(id), 0); err != nil {
			return storeErr(err)
		}
		s.c.mu.Lock()
		delete(s.c.ids, s.idKey(id))
		s.c.mu.Unlock()
	}
	return nil
}
