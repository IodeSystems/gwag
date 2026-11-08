package srserve

import (
	"context"
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
