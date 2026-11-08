package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nats-io/nats.go"
)

// errKVLookup is a kvKeyGone lookup that could not decide.
var errKVLookup = errors.New("kv lookup failed")

// kvKeyGone asks the stream leader whether key is absent from bucket:
// no message for it, or a last message that is a delete, purge or
// expiry marker. It uses the JetStream API's message get, which only
// the leader answers, not KeyValue.Get: the buckets allow direct gets,
// and a direct get may be answered by a replica that is behind or
// rebuilding its store, and miss a key the stream holds. No leader →
// an error, and the caller keeps the key.
func kvKeyGone(ctx context.Context, nc *nats.Conn, bucket, key string) (bool, error) {
	_, ok, err := kvLeaderGet(ctx, nc, bucket, key)
	return !ok, err
}

// kvLeaderGet returns key's value as the stream leader holds it. ok
// is false when the key is absent (see kvKeyGone).
func kvLeaderGet(ctx context.Context, nc *nats.Conn, bucket, key string) (val []byte, ok bool, err error) {
	val, _, ok, err = kvLeaderEntry(ctx, nc, bucket, key)
	return val, ok, err
}

// kvLeaderEntry is kvLeaderGet plus the revision of the key's last
// message (its stream sequence), for a compare and set. rev is set
// when that message is a delete too (ok false), and 0 when the key has
// no message.
func kvLeaderEntry(ctx context.Context, nc *nats.Conn, bucket, key string) (val []byte, rev uint64, ok bool, err error) {
	req, err := json.Marshal(map[string]string{"last_by_subj": "$KV." + bucket + "." + key})
	if err != nil {
		return nil, 0, false, err
	}
	msg, err := nc.RequestWithContext(ctx, "$JS.API.STREAM.MSG.GET.KV_"+bucket, req)
	if err != nil {
		return nil, 0, false, fmt.Errorf("%w: %v", errKVLookup, err)
	}
	var resp struct {
		Error *struct {
			Code    int `json:"code"`
			ErrCode int `json:"err_code"`
		} `json:"error"`
		Message *struct {
			Seq    uint64 `json:"seq"`
			Header []byte `json:"hdrs"` // base64 in JSON; decoded by encoding/json
			Data   []byte `json:"data"`
		} `json:"message"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, 0, false, fmt.Errorf("%w: %v", errKVLookup, err)
	}
	if resp.Error != nil {
		// 10037: no message found.
		if resp.Error.ErrCode == 10037 {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("%w: api error %d/%d", errKVLookup, resp.Error.Code, resp.Error.ErrCode)
	}
	if resp.Message == nil {
		return nil, 0, false, errKVLookup
	}
	h := resp.Message.Header
	if bytes.Contains(h, []byte("KV-Operation")) || bytes.Contains(h, []byte("Nats-Marker-Reason")) {
		return nil, resp.Message.Seq, false, nil
	}
	return resp.Message.Data, resp.Message.Seq, true, nil
}

// kvLeaderKeys returns the keys that have a message in bucket, as
// the stream leader reports them (stream info answers only there),
// sorted. A key whose last message is a delete is included; read it
// with kvLeaderGet.
func kvLeaderKeys(ctx context.Context, nc *nats.Conn, bucket string) ([]string, error) {
	prefix := "$KV." + bucket + "."
	var keys []string
	for offset := 0; ; {
		req, err := json.Marshal(map[string]any{"subjects_filter": prefix + ">", "offset": offset})
		if err != nil {
			return nil, err
		}
		msg, err := nc.RequestWithContext(ctx, "$JS.API.STREAM.INFO.KV_"+bucket, req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errKVLookup, err)
		}
		var resp struct {
			Error *struct {
				Code    int `json:"code"`
				ErrCode int `json:"err_code"`
			} `json:"error"`
			Total int `json:"total"`
			State struct {
				Subjects map[string]uint64 `json:"subjects"`
			} `json:"state"`
		}
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return nil, fmt.Errorf("%w: %v", errKVLookup, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%w: api error %d/%d", errKVLookup, resp.Error.Code, resp.Error.ErrCode)
		}
		for s := range resp.State.Subjects {
			keys = append(keys, strings.TrimPrefix(s, prefix))
		}
		offset += len(resp.State.Subjects)
		if len(resp.State.Subjects) == 0 || offset >= resp.Total {
			break
		}
	}
	sort.Strings(keys)
	return keys, nil
}
