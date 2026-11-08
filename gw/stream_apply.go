package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Applying a stream config from every node.
//
// Every gateway of a cluster reacts to the same peer and slot events,
// so every node sends the same stream create or update at the same
// moment. nats-server (seen on v2.15.2) leaves some of several
// concurrent identical requests unanswered although it applies them:
// of three concurrent R1→R3 updates of one stream, one gets no reply
// and its caller waits out its timeout. Sequential identical updates
// are answered. applyStream therefore:
//
//   - reads the stream first and does not write a stream that already
//     has the config;
//   - writes at once only on the JetStream meta leader. Another node
//     reads again for up to applyStreamFollowerWait and writes only if
//     the change has not shown by then (the leader's gateway may not
//     have seen the same event, or the leader may have changed). The
//     meta leader proposes every stream change anyway, and each node
//     knows locally whether it is the leader, so this needs no
//     coordination. In the usual case one request is sent;
//   - bounds each request by applyStreamAttemptTimeout. After no
//     answer, or a failure that can be transient, it reads the stream
//     again: a stream at the config is success. Otherwise it retries
//     with backoff until ctx ends.

const (
	applyStreamAttemptTimeout = 3 * time.Second
	applyStreamFollowerWait   = 2 * time.Second
	applyStreamBackoffMin     = 50 * time.Millisecond
	applyStreamBackoffMax     = time.Second
)

// errStreamMissing: the stream does not exist and the caller does
// not create it.
var errStreamMissing = errors.New("stream does not exist")

// streamBuild returns the config the stream must have, given its
// current config (nil when the stream does not exist). ok false
// means: do not create it (applyStream returns errStreamMissing).
type streamBuild func(cur *jetstream.StreamConfig) (want jetstream.StreamConfig, ok bool)

// applyStream brings stream name to the config build returns and
// returns the config the stream has afterwards. leader reports
// whether this node is the JetStream meta leader.
func applyStream(ctx context.Context, js jetstream.JetStream, name string, leader func() bool, build streamBuild) (jetstream.StreamConfig, error) {
	start := time.Now()
	backoff := applyStreamBackoffMin
	var lastErr error
	for {
		cur, err := readStreamConfig(ctx, js, name)
		if err == nil {
			want, ok := build(cur)
			switch {
			case cur == nil && !ok:
				return jetstream.StreamConfig{}, errStreamMissing
			case cur != nil && streamConfigMatches(*cur, want):
				return *cur, nil
			case leader() || time.Since(start) >= applyStreamFollowerWait:
				got, err := writeStream(ctx, js, cur == nil, want)
				if err == nil {
					return got, nil
				}
				if !retryableStreamErr(err) {
					return jetstream.StreamConfig{}, err
				}
				lastErr = err
			}
		} else {
			lastErr = err
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return jetstream.StreamConfig{}, lastErr
		case <-t.C:
		}
		backoff = min(2*backoff, applyStreamBackoffMax)
	}
}

// readStreamConfig returns the stream's config, or nil when the
// stream does not exist.
func readStreamConfig(ctx context.Context, js jetstream.JetStream, name string) (*jetstream.StreamConfig, error) {
	c, cancel := context.WithTimeout(ctx, applyStreamAttemptTimeout)
	defer cancel()
	s, err := js.Stream(c, name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := s.CachedInfo().Config
	return &cfg, nil
}

func writeStream(ctx context.Context, js jetstream.JetStream, create bool, cfg jetstream.StreamConfig) (jetstream.StreamConfig, error) {
	c, cancel := context.WithTimeout(ctx, applyStreamAttemptTimeout)
	defer cancel()
	var (
		s   jetstream.Stream
		err error
	)
	if create {
		s, err = js.CreateStream(c, cfg)
	} else {
		s, err = js.UpdateStream(c, cfg)
	}
	if err != nil {
		return jetstream.StreamConfig{}, err
	}
	return s.CachedInfo().Config, nil
}

// retryableStreamErr: no answer, or a state another node's
// concurrent write can explain (a create that lost the race, an
// update of a stream whose create is not applied yet). Any other API
// error is final: the config is refused.
func retryableStreamErr(err error) bool {
	if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) || errors.Is(err, jetstream.ErrStreamNotFound) {
		return true
	}
	var apiErr *jetstream.APIError
	return !errors.As(err, &apiErr)
}

// streamConfigMatches reports whether cur already has every field
// the gateway sets in want.
func streamConfigMatches(cur, want jetstream.StreamConfig) bool {
	return cur.Replicas == want.Replicas &&
		slices.Equal(cur.Subjects, want.Subjects) &&
		cur.MaxAge == want.MaxAge &&
		cur.Duplicates == want.Duplicates &&
		cur.Storage == want.Storage &&
		cur.Retention == want.Retention &&
		cur.AllowMsgTTL == want.AllowMsgTTL &&
		cur.SubjectDeleteMarkerTTL == want.SubjectDeleteMarkerTTL
}

// isJetStreamLeader reports whether this node's NATS server is the
// JetStream meta leader.
func (g *Gateway) isJetStreamLeader() bool {
	cl := g.cfg.cluster
	return cl != nil && cl.Server != nil && cl.Server.JetStreamIsLeader()
}
