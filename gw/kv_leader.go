package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	req, err := json.Marshal(map[string]string{"last_by_subj": "$KV." + bucket + "." + key})
	if err != nil {
		return false, err
	}
	msg, err := nc.RequestWithContext(ctx, "$JS.API.STREAM.MSG.GET.KV_"+bucket, req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errKVLookup, err)
	}
	var resp struct {
		Error *struct {
			Code    int `json:"code"`
			ErrCode int `json:"err_code"`
		} `json:"error"`
		Message *struct {
			Header []byte `json:"hdrs"` // base64 in JSON; decoded by encoding/json
		} `json:"message"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return false, fmt.Errorf("%w: %v", errKVLookup, err)
	}
	if resp.Error != nil {
		// 10037: no message found.
		if resp.Error.ErrCode == 10037 {
			return true, nil
		}
		return false, fmt.Errorf("%w: api error %d/%d", errKVLookup, resp.Error.Code, resp.Error.ErrCode)
	}
	if resp.Message == nil {
		return false, errKVLookup
	}
	h := resp.Message.Header
	return bytes.Contains(h, []byte("KV-Operation")) || bytes.Contains(h, []byte("Nats-Marker-Reason")), nil
}
