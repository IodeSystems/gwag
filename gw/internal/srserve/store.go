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

// Keys:
//
//	id.<n>          schemaRec of schema id n (immutable once written)
//	key.<hex>       the first id holding a schema with that key
//	subj.<b64>      subjectRec: the subject's versions
//	cfg.<b64>       the subject's compatibility level
//	config          the global compatibility level
//	next            the highest id allocated or imported (decimal)
//
// <b64> is the subject in unpadded URL-safe base64: subjects may hold
// any character, KV keys may not.
const (
	keyConfig = "config"
	keyNext   = "next"
)

func idKey(id int) string           { return "id." + strconv.Itoa(id) }
func hashKey(k string) string       { return "key." + k }
func subjKey(subject string) string { return "subj." + b64(subject) }
func cfgKey(subject string) string  { return "cfg." + b64(subject) }
func b64(s string) string           { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

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

// Store is the registry state over a KV.
type Store struct {
	kv KV
	// firstID is the lowest id Register allocates.
	firstID int

	mu  sync.Mutex
	ids map[int]*schemaRec // cache; a schema id never changes
}

// NewStore returns a store over kv. Register allocates ids from
// firstID up (at least 1), always above every id already held.
func NewStore(kv KV, firstID int) *Store {
	if firstID < 1 {
		firstID = 1
	}
	return &Store{kv: kv, firstID: firstID, ids: map[int]*schemaRec{}}
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
	codeSubjectLevelNoConfig  = 40408
	codeConflict              = 40901
	codeInvalidSchema         = 42201
	codeInvalidVersion        = 42202
	codeInvalidCompatibility  = 42203
	codeOperationNotPermitted = 42205
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
	s.mu.Lock()
	r := s.ids[id]
	s.mu.Unlock()
	if r != nil {
		return r, nil
	}
	r = &schemaRec{}
	if _, err := s.getJSON(ctx, idKey(id), r); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ids[id] = r
	s.mu.Unlock()
	return r, nil
}

func (s *Store) subject(ctx context.Context, subject string) (*subjectRec, uint64, error) {
	rec := &subjectRec{}
	rev, err := s.getJSON(ctx, subjKey(subject), rec)
	return rec, rev, err
}

// resolve is the resolver over the store.
func (s *Store) resolve(ctx context.Context, subject string, version int) (*schemaRec, error) {
	_, rec, err := s.Version(ctx, subject, version, true)
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
	keys, err := s.kv.Keys(ctx, "subj.")
	if err != nil {
		return nil, storeErr(err)
	}
	out := []string{}
	for _, k := range keys {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(k, "subj."))
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
		err = s.createOrSame(ctx, idKey(id), b)
		if errors.Is(err, ErrConflict) {
			continue // an import took the id
		}
		if err != nil {
			return 0, err
		}
		_, err = s.kv.Create(ctx, hashKey(want.Keys[0]), []byte(strconv.Itoa(id)))
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
		b, _, err := s.kv.Get(ctx, hashKey(k))
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
		b, rev, err := s.kv.Get(ctx, keyNext)
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
		id := max(cur+1, s.firstID)
		val := []byte(strconv.Itoa(id))
		if rev == 0 {
			_, err = s.kv.Create(ctx, keyNext, val)
		} else {
			_, err = s.kv.Update(ctx, keyNext, val, rev)
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
		b, rev, err := s.kv.Get(ctx, keyNext)
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
			_, err = s.kv.Create(ctx, keyNext, val)
		} else {
			_, err = s.kv.Update(ctx, keyNext, val, rev)
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
			_, err = s.kv.Create(ctx, subjKey(subject), b)
		} else {
			_, err = s.kv.Update(ctx, subjKey(subject), b, rev)
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

// Config returns the level of subject ("" global). ok is false when
// none is set there.
func (s *Store) Config(ctx context.Context, subject string) (string, bool, error) {
	key := keyConfig
	if subject != "" {
		key = cfgKey(subject)
	}
	b, _, err := s.kv.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		if subject == "" {
			return DefaultLevel, true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, storeErr(err)
	}
	return string(b), true, nil
}

// Level is the level that applies to subject: its own, else global.
func (s *Store) Level(ctx context.Context, subject string) (string, error) {
	l, ok, err := s.Config(ctx, subject)
	if err != nil || ok {
		return l, err
	}
	l, _, err = s.Config(ctx, "")
	return l, err
}

// SetConfig sets the level of subject ("" global).
func (s *Store) SetConfig(ctx context.Context, subject, level string) error {
	level = strings.ToUpper(level)
	if !levels[level] {
		return errf(codeInvalidCompatibility, "Invalid compatibility level: %s", level)
	}
	key := keyConfig
	if subject != "" {
		key = cfgKey(subject)
	}
	if _, err := s.kv.Put(ctx, key, []byte(level)); err != nil {
		return storeErr(err)
	}
	return nil
}
