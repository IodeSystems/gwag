package schemaregistry

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Shadow is a (gwag.ps.v1.shadow) option.
//
// Stability: experimental
type Shadow struct {
	Registry, Subject, Version, Message string
	// Producer is side PRODUCER; false is CONSUMER.
	Producer bool
}

const shadowExt = "gwag.ps.v1.shadow"

// ShadowOf reads (gwag.ps.v1.shadow) off md. Works for dynamicpb
// option values (protocompile) and the generated type alike.
//
// Stability: experimental
func ShadowOf(md protoreflect.MessageDescriptor) (Shadow, bool) {
	var s Shadow
	found := false
	opts := md.Options()
	if opts == nil {
		return s, false
	}
	opts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !fd.IsExtension() || string(fd.FullName()) != shadowExt {
			return true
		}
		m := v.Message()
		get := func(name string) protoreflect.Value {
			f := m.Descriptor().Fields().ByName(protoreflect.Name(name))
			if f == nil {
				return protoreflect.Value{}
			}
			return m.Get(f)
		}
		s.Registry = get("registry").String()
		s.Subject = get("subject").String()
		s.Version = get("version").String()
		s.Message = get("message").String()
		s.Producer = get("side").Enum() == 1
		found = true
		return false
	})
	return s, found
}

var subjectFileRE = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// SubjectPath is the file path a subject's schema gets in a Tree.
//
// Stability: experimental
func SubjectPath(subject string) string {
	return "registry/" + subjectFileRE.ReplaceAllString(subject, "_") + ".proto"
}

// ShadowTarget picks the registry message a shadow copies: the named
// one, else the one with the same simple name, else the first.
//
// Stability: experimental
func ShadowTarget(fd protoreflect.FileDescriptor, md protoreflect.MessageDescriptor, name string) (protoreflect.MessageDescriptor, error) {
	if name != "" {
		t := FindMessage(fd, protoreflect.FullName(name))
		if t == nil {
			return nil, fmt.Errorf("registry schema has no message %s", name)
		}
		return t, nil
	}
	if t := fd.Messages().ByName(md.Name()); t != nil {
		return t, nil
	}
	if fd.Messages().Len() == 0 {
		return nil, fmt.Errorf("registry schema has no message")
	}
	return fd.Messages().Get(0), nil
}

// CheckShadow compares the shadow message md with the subject s names
// in the registry c talks to. A consumer shadow is compared by
// structure (CompareConsumer); a producer shadow goes through the
// registry's compatibility API. An error means the check could not run
// (subject missing, schema does not compile); the problems are the
// differences.
//
// Stability: experimental
func CheckShadow(ctx context.Context, c *Client, s Shadow, md protoreflect.MessageDescriptor) ([]Problem, error) {
	if s.Registry == "" || s.Subject == "" {
		return nil, fmt.Errorf("shadow option needs registry and subject")
	}
	v, err := c.SubjectVersion(ctx, s.Subject, s.Version, "")
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree(ctx, v, SubjectPath(s.Subject))
	if err != nil {
		return nil, err
	}
	fd, err := tree.Compile(ctx)
	if err != nil {
		return nil, err
	}
	target, err := ShadowTarget(fd, md, s.Message)
	if err != nil {
		return nil, err
	}
	if !s.Producer {
		return CompareConsumer(md, target), nil
	}
	fdp, err := ProducerCandidate(fd, target, md)
	if err != nil {
		return nil, err
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
	if err != nil {
		return nil, err
	}
	cand := Schema{Schema: base64.StdEncoding.EncodeToString(b), SchemaType: "PROTOBUF", References: v.References}
	version := s.Version
	if version == "" {
		version = "latest"
	}
	res, err := c.CheckCompatibility(ctx, s.Subject, version, cand)
	if err != nil {
		return nil, err
	}
	level, _ := c.CompatibilityLevel(ctx, s.Subject)
	var out []Problem
	if !res.IsCompatible {
		if len(res.Messages) == 0 {
			res.Messages = []string{"not compatible"}
		}
		for _, m := range res.Messages {
			// Verbose answers also echo the old schema and settings.
			if strings.HasPrefix(m, "{oldSchema") || strings.HasPrefix(m, "{validateFields") {
				continue
			}
			out = append(out, Problem{Text: fmt.Sprintf("registry (%s): %s", level, m)})
		}
	}
	return out, nil
}
