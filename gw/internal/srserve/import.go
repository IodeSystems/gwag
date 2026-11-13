package srserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Bundle is what `gwag ctl schema import` copies out of another registry.
type Bundle struct {
	Schemas  []ImportSchema  `json:"schemas"`
	Versions []ImportVersion `json:"versions"`
	// Global is the global compatibility level ("" leaves it).
	Global string `json:"global,omitempty"`
	// Levels are the subject-level compatibility levels.
	Levels map[string]string `json:"levels,omitempty"`
}

// ImportSchema is one schema id as the source registry serves it.
type ImportSchema struct {
	ID         int    `json:"id"`
	SchemaType string `json:"schemaType,omitempty"`
	// Schema is the default format (.proto text for PROTOBUF).
	Schema string `json:"schema"`
	// Serialized is ?format=serialized (PROTOBUF only; may be empty).
	Serialized string         `json:"serialized,omitempty"`
	References []sr.Reference `json:"references,omitempty"`
}

// ImportVersion is one subject version.
type ImportVersion struct {
	Subject string `json:"subject"`
	Version int    `json:"version"`
	ID      int    `json:"id"`
	Deleted bool   `json:"deleted,omitempty"`
}

// ImportResult counts what an import changed.
type ImportResult struct {
	SchemasAdded  int `json:"schemas_added"`
	VersionsAdded int `json:"versions_added"`
	Unchanged     int `json:"unchanged"`
	// MaxID is the highest id imported; new ids are allocated above.
	MaxID int `json:"max_id"`
	// Warnings name schemas gwag could not compile: they are kept
	// and served as they came, but a lookup matches them only by
	// identical text.
	Warnings []string `json:"warnings,omitempty"`
	// Conflicts lists what differs (DryRun only; otherwise a conflict
	// is an error).
	Conflicts []string `json:"conflicts,omitempty"`
}

// ImportOptions changes what ImportWith allows.
type ImportOptions struct {
	// Default allows the default context. Only for a bundle another
	// gwag registry's default context gave (Store.Bundle): its ids
	// are gwag-allocated too, and a copy keeps them. Conflicts are
	// refused as in any import.
	Default bool
	// DryRun checks the bundle and counts what an import would add,
	// and writes nothing. Conflicts are returned in
	// ImportResult.Conflicts, not as an error.
	DryRun bool
}

// Import copies b into the store's context, keeping ids and versions.
// The default context is refused: it is gwag's own id space, and
// another registry's ids would clash with it. Nothing is written when
// a schema id or subject version the context already holds differs
// from b's (an error naming every conflict). Importing the same bundle
// again changes nothing.
func (s *Store) Import(ctx context.Context, b Bundle) (ImportResult, error) {
	return s.ImportWith(ctx, b, ImportOptions{})
}

// ImportWith is Import with options.
func (s *Store) ImportWith(ctx context.Context, b Bundle, o ImportOptions) (ImportResult, error) {
	var res ImportResult
	if s.Default() && !o.Default {
		return res, errf(codeOperationNotPermitted, "import into a named context (POST /contexts/.corp/gwag/import; gwag ctl schema import --into .corp): the default context holds the ids gwag allocates")
	}
	byID := map[int]*ImportSchema{}
	for i := range b.Schemas {
		x := &b.Schemas[i]
		if x.ID < 1 {
			return res, errf(codeInvalidSchema, "schema id %d: want >= 1", x.ID)
		}
		byID[x.ID] = x
		res.MaxID = max(res.MaxID, x.ID)
	}
	// Bundle versions by subject, for references to resolve before
	// anything is written.
	bundleVer := map[string]map[int]int{}
	for _, v := range b.Versions {
		if v.Subject == "" || v.Version < 1 {
			return res, errf(codeInvalidVersion, "subject %q version %d: invalid", v.Subject, v.Version)
		}
		if strings.HasPrefix(v.Subject, ":") {
			return res, errf(codeInvalidSubject, "subject %q: a bundle names subjects without a context", v.Subject)
		}
		if byID[v.ID] == nil {
			return res, errf(codeSchemaNotFound, "subject %s version %d: schema id %d is not in the bundle", v.Subject, v.Version, v.ID)
		}
		if bundleVer[v.Subject] == nil {
			bundleVer[v.Subject] = map[int]int{}
		}
		bundleVer[v.Subject][v.Version] = v.ID
	}
	for subj, l := range b.Levels {
		if !levels[strings.ToUpper(l)] {
			return res, errf(codeInvalidCompatibility, "subject %s: invalid compatibility level %s", subj, l)
		}
	}
	if b.Global != "" && !levels[strings.ToUpper(b.Global)] {
		return res, errf(codeInvalidCompatibility, "invalid global compatibility level %s", b.Global)
	}

	recs := map[int]*schemaRec{}
	var resolve resolver
	resolve = func(ctx context.Context, subject string, version int) (*schemaRec, error) {
		if vs := bundleVer[subject]; vs != nil {
			n := version
			if n == -1 {
				for v := range vs {
					n = max(n, v)
				}
			}
			if id, ok := vs[n]; ok {
				return recOf(ctx, id, byID, recs, resolve, &res)
			}
		}
		return s.resolve(ctx, subject, version)
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	// Check every id and version first.
	var conflicts []string
	newID := map[int]bool{}
	for _, id := range ids {
		rec, err := recOf(ctx, id, byID, recs, resolve, &res)
		if err != nil {
			return res, err
		}
		cur, err := s.schema(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			newID[id] = true
		case err != nil:
			return res, storeErr(err)
		case !matches(cur, rec):
			conflicts = append(conflicts, fmt.Sprintf("schema id %d differs from the one gwag holds", id))
		}
	}
	type subjChange struct {
		add     []versionRec
		deleted map[int]bool
	}
	changes := map[string]*subjChange{}
	for _, v := range b.Versions {
		cur, _, err := s.subject(ctx, v.Subject)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return res, storeErr(err)
		}
		c := changes[v.Subject]
		if c == nil {
			c = &subjChange{deleted: map[int]bool{}}
			changes[v.Subject] = c
		}
		if have, ok := cur.version(v.Version, true); ok {
			if have.ID != v.ID {
				conflicts = append(conflicts, fmt.Sprintf("subject %s version %d is schema id %d here, %d in the source", v.Subject, v.Version, have.ID, v.ID))
				continue
			}
			if have.Deleted != v.Deleted {
				c.deleted[v.Version] = v.Deleted
				continue
			}
			res.Unchanged++
			continue
		}
		c.add = append(c.add, versionRec{Version: v.Version, ID: v.ID, Deleted: v.Deleted})
	}
	sort.Strings(conflicts)
	if o.DryRun {
		res.Conflicts = conflicts
		res.SchemasAdded = len(newID)
		for _, c := range changes {
			res.VersionsAdded += len(c.add)
		}
		sort.Strings(res.Warnings)
		return res, nil
	}
	if len(conflicts) > 0 {
		return res, errf(codeConflict, "import conflicts, nothing written: %s", strings.Join(conflicts, "; "))
	}

	// Schemas, then the counter, then versions: a version never names
	// an id that is not written yet.
	for _, id := range ids {
		if !newID[id] {
			continue
		}
		rec := recs[id]
		val, _ := json.Marshal(rec)
		if err := s.createOrSame(ctx, s.idKey(id), val); err != nil {
			if errors.Is(err, ErrConflict) {
				return res, errf(codeConflict, "schema id %d was registered while importing; run the import again", id)
			}
			return res, storeErr(err)
		}
		for _, k := range rec.Keys {
			if _, err := s.kv.Create(ctx, s.hashKey(k), []byte(fmt.Sprint(id))); err != nil && !errors.Is(err, ErrConflict) {
				return res, storeErr(err)
			}
		}
		res.SchemasAdded++
	}
	if err := s.raiseNext(ctx, res.MaxID); err != nil {
		return res, storeErr(err)
	}
	subjects := make([]string, 0, len(changes))
	for subj := range changes {
		subjects = append(subjects, subj)
	}
	sort.Strings(subjects)
	for _, subj := range subjects {
		c := changes[subj]
		if len(c.add) == 0 && len(c.deleted) == 0 {
			continue
		}
		n, err := s.mergeVersions(ctx, subj, c.add, c.deleted)
		if err != nil {
			return res, err
		}
		res.VersionsAdded += n
	}
	if b.Global != "" {
		if err := s.SetConfig(ctx, "", b.Global); err != nil {
			return res, err
		}
	}
	for subj, l := range b.Levels {
		if err := s.SetConfig(ctx, subj, l); err != nil {
			return res, err
		}
	}
	sort.Strings(res.Warnings)
	return res, nil
}

// recOf builds (once) the record of bundle schema id. A PROTOBUF schema
// gets a key per form that compiles; none compiling leaves the raw
// key and a warning.
func recOf(ctx context.Context, id int, byID map[int]*ImportSchema, recs map[int]*schemaRec, resolve resolver, res *ImportResult) (*schemaRec, error) {
	if r := recs[id]; r != nil {
		return r, nil
	}
	x := byID[id]
	if x == nil {
		return nil, ErrNotFound
	}
	rec := &schemaRec{ID: id, Type: schemaType(x.SchemaType), Schema: x.Schema, Serialized: x.Serialized, References: normRefs(x.References), Imported: true}
	// Set before compiling: a reference cycle ends here.
	recs[id] = rec
	if rec.Type == TypeProtobuf {
		var errs []string
		for _, src := range []string{x.Schema, x.Serialized} {
			if src == "" {
				continue
			}
			fd, err := compile(ctx, src, rec.References, resolve)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			k, err := keyOf(fd, rec.References)
			if err != nil {
				return nil, err
			}
			if !containsStr(rec.Keys, k) {
				rec.Keys = append(rec.Keys, k)
			}
			if rec.Serialized == "" {
				rec.Derived = "serialized"
				if rec.Serialized, err = serialize(fd); err != nil {
					return nil, err
				}
			}
		}
		if len(rec.Keys) == 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("schema id %d does not compile: %s", id, strings.Join(errs, "; ")))
		}
	}
	if len(rec.Keys) == 0 {
		rec.Keys = []string{rawKey(rec.Type, rec.Schema, rec.References)}
	}
	return rec, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// mergeVersions adds versions to subject and sets deleted flags, with
// a compare-and-set on the subject record. Returns how many versions
// it added.
func (s *Store) mergeVersions(ctx context.Context, subject string, add []versionRec, deleted map[int]bool) (int, error) {
	for {
		rec, rev, err := s.subject(ctx, subject)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, storeErr(err)
		}
		missing := errors.Is(err, ErrNotFound)
		n := 0
		for _, v := range add {
			if have, ok := rec.version(v.Version, true); ok {
				if have.ID != v.ID {
					return 0, errf(codeConflict, "subject %s version %d was registered while importing; run the import again", subject, v.Version)
				}
				continue
			}
			rec.Versions = append(rec.Versions, v)
			n++
		}
		for i := range rec.Versions {
			if d, ok := deleted[rec.Versions[i].Version]; ok {
				rec.Versions[i].Deleted = d
			}
		}
		sort.Slice(rec.Versions, func(i, j int) bool { return rec.Versions[i].Version < rec.Versions[j].Version })
		b, _ := json.Marshal(rec)
		if missing {
			_, err = s.kv.Create(ctx, s.subjKey(subject), b)
		} else {
			_, err = s.kv.Update(ctx, s.subjKey(subject), b, rev)
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return 0, storeErr(err)
		}
		return n, nil
	}
}
