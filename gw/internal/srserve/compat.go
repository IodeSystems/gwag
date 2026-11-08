package srserve

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// Compatibility for PROTOBUF, by level:
//
//   - NONE: always compatible.
//   - BACKWARD, FORWARD, FULL: every field number the candidate and
//     the version both declare must agree in type, cardinality and
//     shared enum values (schemaregistry.CompareShared). Fields added
//     or removed are compatible: protobuf readers skip unknown fields.
//     BACKWARD and FULL also fail when a message of the version is
//     gone from the candidate (records of that type could no longer
//     be read).
//   - *_TRANSITIVE: the same against every live version, not one.
//
// Other schema types are compatible only when the text is identical.

// Compatible checks in against version of subject (-1 the latest;
// 0: every version the level asks for).
func (s *Store) Compatible(ctx context.Context, subject string, version int, in sr.Schema) (bool, []string, error) {
	level, err := s.Level(ctx, subject)
	if err != nil {
		return false, nil, err
	}
	rec, _, err := s.subject(ctx, subject)
	if err != nil || len(rec.live(false)) == 0 {
		if version == 0 {
			// Nothing to be compatible with.
			return true, nil, nil
		}
		return false, nil, errf(codeSubjectNotFound, "Subject '%s' not found.", subject)
	}
	var targets []versionRec
	switch {
	case version > 0:
		v, ok := rec.version(version, false)
		if !ok {
			return false, nil, errf(codeVersionNotFound, "Version %d not found.", version)
		}
		targets = []versionRec{v}
	case version == -1 || !strings.HasSuffix(level, "_TRANSITIVE"):
		v, _ := rec.latest(false)
		targets = []versionRec{v}
	default:
		targets = rec.live(false)
	}
	if level == "NONE" {
		return true, nil, nil
	}
	in.References = s.localRefs(in.References)
	cand, err := prepare(ctx, in, s.resolve)
	if err != nil {
		return false, nil, errf(codeInvalidSchema, "Invalid schema: %v", err)
	}
	var msgs []string
	for _, t := range targets {
		old, err := s.schema(ctx, t.ID)
		if err != nil {
			return false, nil, storeErr(err)
		}
		if old.Type != cand.Type {
			msgs = append(msgs, fmt.Sprintf("version %d: schema type %s, candidate %s", t.Version, old.Type, cand.Type))
			continue
		}
		if cand.Type != TypeProtobuf {
			if old.Schema != cand.Schema {
				msgs = append(msgs, fmt.Sprintf("version %d: gwag checks %s schemas by identical text only", t.Version, cand.Type))
			}
			continue
		}
		ofd, err := compile(ctx, old.Schema, old.References, s.resolve)
		if err != nil && old.Serialized != "" {
			ofd, err = compile(ctx, old.Serialized, old.References, s.resolve)
		}
		if err != nil {
			return false, nil, storeErr(fmt.Errorf("version %d does not compile: %w", t.Version, err))
		}
		nfd, err := compile(ctx, cand.Serialized, cand.References, s.resolve)
		if err != nil {
			return false, nil, errf(codeInvalidSchema, "Invalid schema: %v", err)
		}
		for _, m := range protoProblems(ofd, nfd, level) {
			msgs = append(msgs, fmt.Sprintf("version %d: %s", t.Version, m))
		}
	}
	return len(msgs) == 0, msgs, nil
}

func protoProblems(old, cand protoreflect.FileDescriptor, level string) []string {
	removedFails := strings.HasPrefix(level, "BACKWARD") || strings.HasPrefix(level, "FULL")
	var out []string
	var walk func(protoreflect.MessageDescriptors)
	walk = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			om := ms.Get(i)
			walk(om.Messages())
			if om.IsMapEntry() {
				continue
			}
			nm := sr.FindMessage(cand, om.FullName())
			if nm == nil {
				if removedFails {
					out = append(out, fmt.Sprintf("message %s removed", om.FullName()))
				}
				continue
			}
			for _, p := range sr.CompareShared(nm, om) {
				if p.Warning {
					continue
				}
				out = append(out, fmt.Sprintf("%s: %s", om.FullName(), strings.Replace(p.String(), "registry has", "was", 1)))
			}
		}
	}
	walk(old.Messages())
	return out
}
