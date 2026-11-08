package schemaregistry

import (
	"encoding/binary"
	"errors"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// MagicByte starts every framed record.
//
// Stability: experimental
const MagicByte = 0

// Frame returns Confluent's protobuf framing of body: MagicByte, the
// schema id (4 bytes, big endian), the message indexes, body.
//
// The indexes locate the message in its schema file: [i] is the i-th
// top-level message, [i, j] its j-th nested message. They are written
// as a zigzag varint count and zigzag varints; [0], the first
// top-level message, is written as a single 0.
//
// Stability: experimental
func Frame(id int, indexes []int, body []byte) []byte {
	out := make([]byte, 5, 5+1+len(indexes)*2+len(body))
	out[0] = MagicByte
	binary.BigEndian.PutUint32(out[1:5], uint32(id))
	if len(indexes) == 1 && indexes[0] == 0 {
		out = binary.AppendVarint(out, 0)
	} else {
		out = binary.AppendVarint(out, int64(len(indexes)))
		for _, i := range indexes {
			out = binary.AppendVarint(out, int64(i))
		}
	}
	return append(out, body...)
}

// ErrNotFramed is returned by Unframe for data that does not start
// with MagicByte.
//
// Stability: experimental
var ErrNotFramed = errors.New("schema registry: data does not start with the magic byte")

// Unframe splits framed data into schema id, message indexes and body.
//
// Stability: experimental
func Unframe(data []byte) (id int, indexes []int, body []byte, err error) {
	if len(data) < 5 || data[0] != MagicByte {
		return 0, nil, nil, ErrNotFramed
	}
	id = int(binary.BigEndian.Uint32(data[1:5]))
	rest := data[5:]
	n, k := binary.Varint(rest)
	if k <= 0 {
		return 0, nil, nil, fmt.Errorf("schema registry: bad message index count")
	}
	rest = rest[k:]
	if n == 0 {
		return id, []int{0}, rest, nil
	}
	if n < 0 || n > 1024 {
		return 0, nil, nil, fmt.Errorf("schema registry: message index count %d", n)
	}
	indexes = make([]int, n)
	for i := range indexes {
		v, k := binary.Varint(rest)
		if k <= 0 || v < 0 {
			return 0, nil, nil, fmt.Errorf("schema registry: bad message index")
		}
		indexes[i] = int(v)
		rest = rest[k:]
	}
	return id, indexes, rest, nil
}

// MessageIndexes returns md's indexes in its file.
//
// Stability: experimental
func MessageIndexes(md protoreflect.MessageDescriptor) []int {
	var out []int
	for d := protoreflect.Descriptor(md); ; {
		m, ok := d.(protoreflect.MessageDescriptor)
		if !ok {
			break
		}
		out = append([]int{m.Index()}, out...)
		d = m.Parent()
	}
	return out
}

// MessageAt returns the message at indexes in fd.
//
// Stability: experimental
func MessageAt(fd protoreflect.FileDescriptor, indexes []int) (protoreflect.MessageDescriptor, error) {
	if len(indexes) == 0 {
		return nil, errors.New("schema registry: no message indexes")
	}
	ms := fd.Messages()
	var md protoreflect.MessageDescriptor
	for _, i := range indexes {
		if i >= ms.Len() {
			return nil, fmt.Errorf("schema registry: message index %v not in %s", indexes, fd.Path())
		}
		md = ms.Get(i)
		ms = md.Messages()
	}
	return md, nil
}

// FindMessage returns the message named name in fd, top-level or
// nested, or nil.
//
// Stability: experimental
func FindMessage(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.MessageDescriptor {
	var find func(ms protoreflect.MessageDescriptors) protoreflect.MessageDescriptor
	find = func(ms protoreflect.MessageDescriptors) protoreflect.MessageDescriptor {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			if m.FullName() == name {
				return m
			}
			if f := find(m.Messages()); f != nil {
				return f
			}
		}
		return nil
	}
	return find(fd.Messages())
}
