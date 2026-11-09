package srserve

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Export reads a Bundle out of a registry: subjects (all, or the
// named ones and the subjects their references name), every version,
// each schema id in the default and the serialized format, and the
// compatibility levels. deleted includes soft-deleted subjects and
// versions.
//
// Export reads one context of the source. A Confluent registry lists
// the subjects of every context at its root, qualified (":.x:s"); a
// source with subjects in more than one context fails, naming them:
// read one at a time from URL/contexts/.x. The bundle names subjects
// without the qualifier.
func Export(ctx context.Context, c *sr.Client, subjects []string, deleted bool) (Bundle, error) {
	b := Bundle{Levels: map[string]string{}}
	var err error
	if len(subjects) == 0 {
		if subjects, err = c.Subjects(ctx, deleted); err != nil {
			return b, fmt.Errorf("list subjects: %w", err)
		}
	}
	ctxs := map[string]bool{}
	for _, s := range subjects {
		ctxs[subjectContext(s)] = true
	}
	if len(ctxs) > 1 {
		names := make([]string, 0, len(ctxs))
		for n := range ctxs {
			names = append(names, n)
		}
		sort.Strings(names)
		return b, fmt.Errorf("the source has subjects in contexts %s; read one context at a time (the source URL + /contexts/<context>, and --subject)", strings.Join(names, ", "))
	}
	queue := append([]string(nil), subjects...)
	doneSubj := map[string]bool{}
	ids := map[int]*ImportSchema{}
	for len(queue) > 0 {
		subj := queue[0]
		queue = queue[1:]
		if doneSubj[subj] {
			continue
		}
		doneSubj[subj] = true
		vs, err := c.Versions(ctx, subj, deleted)
		if err != nil {
			return b, fmt.Errorf("subject %s: versions: %w", subj, err)
		}
		live := map[int]bool{}
		if deleted {
			lv, err := c.Versions(ctx, subj, false)
			if err != nil && !sr.IsNotFound(err) {
				return b, fmt.Errorf("subject %s: versions: %w", subj, err)
			}
			for _, v := range lv {
				live[v] = true
			}
		}
		for _, n := range vs {
			v, err := c.Version(ctx, subj, n, "", deleted)
			if err != nil {
				return b, fmt.Errorf("subject %s version %d: %w", subj, n, err)
			}
			b.Versions = append(b.Versions, ImportVersion{Subject: bareSubject(subj), Version: n, ID: v.ID, Deleted: deleted && !live[n]})
			for _, r := range v.References {
				queue = append(queue, r.Subject)
			}
			if ids[v.ID] != nil {
				continue
			}
			x := &ImportSchema{ID: v.ID, SchemaType: v.SchemaType, Schema: v.Schema, References: v.References}
			if schemaType(v.SchemaType) == TypeProtobuf {
				s, err := c.SchemaByID(ctx, v.ID, "serialized")
				if err != nil {
					return b, fmt.Errorf("schema id %d serialized: %w", v.ID, err)
				}
				x.Serialized = s.Schema
			}
			ids[v.ID] = x
		}
		l, err := c.Config(ctx, subj)
		if err != nil {
			return b, fmt.Errorf("subject %s: config: %w", subj, err)
		}
		if l != "" {
			b.Levels[bareSubject(subj)] = l
		}
	}
	// Under /contexts/.x the level is the context's; unset is 40408.
	var e *sr.Error
	if b.Global, err = c.Config(ctx, ""); err != nil && !(errors.As(err, &e) && e.Code == codeSubjectLevelNoConfig) {
		return b, fmt.Errorf("global config: %w", err)
	}
	for _, x := range ids {
		b.Schemas = append(b.Schemas, *x)
	}
	sort.Slice(b.Schemas, func(i, j int) bool { return b.Schemas[i].ID < b.Schemas[j].ID })
	return b, nil
}

// subjectContext is the context a qualified subject names ("." for an
// unqualified one).
func subjectContext(s string) string {
	if strings.HasPrefix(s, ":") {
		if name, _, ok := strings.Cut(s[1:], ":"); ok {
			if n, err := NormContext(name); err == nil {
				return n
			}
		}
	}
	return DefaultContext
}

// bareSubject drops a subject's context qualifier.
func bareSubject(s string) string {
	if strings.HasPrefix(s, ":") {
		if _, rest, ok := strings.Cut(s[1:], ":"); ok {
			return rest
		}
	}
	return s
}

// Bundle reads the store's context as a Bundle, for a copy into
// another gwag registry (ImportWith with Default): every subject
// version, deleted ones included, each schema id they name in both
// formats, and the compatibility levels set in the context. skip,
// when set, leaves out the subjects it names (a schema id only those
// subjects use is left out with them). Reads the store directly, not
// over the REST API, so subjects of other contexts are not mixed in.
func (s *Store) Bundle(ctx context.Context, skip func(subject string) bool) (Bundle, error) {
	b := Bundle{Levels: map[string]string{}}
	subjects, err := s.Subjects(ctx, true)
	if err != nil {
		return b, err
	}
	ids := map[int]bool{}
	for _, subj := range subjects {
		if skip != nil && skip(subj) {
			continue
		}
		rec, _, err := s.subject(ctx, subj)
		if err != nil {
			return b, storeErr(err)
		}
		for _, v := range rec.live(true) {
			b.Versions = append(b.Versions, ImportVersion{Subject: subj, Version: v.Version, ID: v.ID, Deleted: v.Deleted})
			ids[v.ID] = true
		}
	}
	cfgs, err := s.kv.Keys(ctx, s.pfx+"cfg.")
	if err != nil {
		return b, storeErr(err)
	}
	for _, k := range cfgs {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(k, s.pfx+"cfg."))
		if err != nil || (skip != nil && skip(string(raw))) {
			continue
		}
		l, ok, err := s.Config(ctx, string(raw))
		if err != nil {
			return b, err
		}
		if ok {
			b.Levels[string(raw)] = l
		}
	}
	for id := range ids {
		rec, err := s.schema(ctx, id)
		if err != nil {
			return b, storeErr(fmt.Errorf("schema id %d: %w", id, err))
		}
		b.Schemas = append(b.Schemas, ImportSchema{ID: id, SchemaType: rec.Type, Schema: rec.Schema, Serialized: rec.Serialized, References: rec.References})
	}
	sort.Slice(b.Schemas, func(i, j int) bool { return b.Schemas[i].ID < b.Schemas[j].ID })
	// The context's own level: Config("") answers the default
	// BACKWARD in the default context when none is set; read the key.
	if v, _, err := s.kv.Get(ctx, s.key(keyConfig)); err == nil {
		b.Global = string(v)
	} else if !errors.Is(err, ErrNotFound) {
		return b, storeErr(err)
	}
	return b, nil
}
