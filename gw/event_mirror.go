package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Event stream mirror, the stream configs (green side). The NATS
// side is cluster_mirror.go; docs/cutover.md has the runbook.
//
// Mirroring a stream is one source on green's stream of the same
// name: StreamSource{Name, External{mirrorAPIPrefix,
// mirrorDeliverPrefix}}. That source is the mirror state; there is no
// other record. Start adds it (and creates green's stream from blue's
// subject and gwag's event stream config when green has none), stop
// removes it, status reads it. Every other config write of an event
// stream (ensureEventStream, setEventStreamReplicas) keeps the
// stream's sources.
//
// Sources, not a JetStream mirror: a mirror has no subjects, so
// events published on green while the mirror runs (switchover
// traffic) would have no stream, and a stream green already created
// cannot become a mirror. A sourced message keeps its subject,
// headers and Nats-Msg-Id; the server adds Nats-Stream-Source. Its
// sequence on green is green's own.

var (
	errMirrorOff     = errors.New("this gateway's cluster has no event stream mirror source (ClusterOptions.MirrorFrom, gwag up --mirror-from)")
	errMirrorFrom    = errors.New("--from does not name this cluster's mirror source")
	errMirrorUnknown = errors.New("no such event stream")
	errMirrorBlue    = errors.New("cannot read the source cluster's streams")
)

// eventStreamPrefix starts the name of every event stream.
const eventStreamPrefix = "EV_"

func mirrorSource(name string) *jetstream.StreamSource {
	return &jetstream.StreamSource{
		Name:     name,
		External: &jetstream.ExternalStream{APIPrefix: mirrorAPIPrefix, DeliverPrefix: mirrorDeliverPrefix},
	}
}

func isMirrorSource(s *jetstream.StreamSource) bool {
	return s != nil && s.External != nil && s.External.APIPrefix == mirrorAPIPrefix
}

func hasMirrorSource(srcs []*jetstream.StreamSource) bool {
	return slices.ContainsFunc(srcs, isMirrorSource)
}

// withoutMirrorSource is srcs without the mirror source; nil when
// nothing is left.
func withoutMirrorSource(srcs []*jetstream.StreamSource) []*jetstream.StreamSource {
	var out []*jetstream.StreamSource
	for _, s := range srcs {
		if !isMirrorSource(s) {
			out = append(out, s)
		}
	}
	return out
}

// sourcesMatch compares the stream sources by the fields gwag sets.
func sourcesMatch(a, b []*jetstream.StreamSource) bool {
	return slices.EqualFunc(a, b, func(x, y *jetstream.StreamSource) bool {
		if x == nil || y == nil {
			return x == y
		}
		if x.Name != y.Name || x.FilterSubject != y.FilterSubject || x.OptStartSeq != y.OptStartSeq || (x.External == nil) != (y.External == nil) {
			return false
		}
		return x.External == nil || *x.External == *y.External
	})
}

// mirrorState is the cluster's mirror, for a green-side operation.
func (g *Gateway) mirrorState() (*Cluster, *clusterMirror, error) {
	cl := g.cfg.cluster
	if cl == nil || cl.JS == nil || cl.mirror == nil || len(cl.mirror.from) == 0 {
		return nil, nil, errMirrorOff
	}
	return cl, cl.mirror, nil
}

// blueEventStreams lists blue's event streams: name → subjects.
func blueEventStreams(ctx context.Context, bjs jetstream.JetStream) (map[string][]string, error) {
	out := map[string][]string{}
	l := bjs.ListStreams(ctx)
	for info := range l.Info() {
		if strings.HasPrefix(info.Config.Name, eventStreamPrefix) {
			out[info.Config.Name] = info.Config.Subjects
		}
	}
	if err := l.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", errMirrorBlue, err)
	}
	return out, nil
}

// pickStreams resolves the operator's list (stream names or channel
// subjects) against names → subjects. Empty only: every name.
func pickStreams(all map[string][]string, only []string) ([]string, error) {
	if len(only) == 0 {
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		return names, nil
	}
	var out, missing []string
	for _, want := range only {
		found := ""
		if _, ok := all[want]; ok {
			found = want
		} else {
			for n, subs := range all {
				if slices.Contains(subs, want) {
					found = n
					break
				}
			}
		}
		if found == "" {
			missing = append(missing, want)
		} else if !slices.Contains(out, found) {
			out = append(out, found)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", errMirrorUnknown, strings.Join(missing, ", "))
	}
	sort.Strings(out)
	return out, nil
}

// startMirror makes green's event streams source blue's: every
// event stream blue has, or the listed ones (stream name or channel
// subject). from, when set, must name the configured source.
func (g *Gateway) startMirror(ctx context.Context, from string, only []string) (mirrorView, error) {
	cl, m, err := g.mirrorState()
	if err != nil {
		return mirrorView{}, err
	}
	if from != "" && !m.matchesFrom(from) {
		return mirrorView{}, fmt.Errorf("%w: %q; configured: %s", errMirrorFrom, from, m.fromString())
	}
	bjs, err := m.blueStreams()
	if err != nil {
		return mirrorView{}, fmt.Errorf("%w: %v", errMirrorBlue, err)
	}
	blue, err := blueEventStreams(ctx, bjs)
	if err != nil {
		return mirrorView{}, err
	}
	names, err := pickStreams(blue, only)
	if err != nil {
		return mirrorView{}, err
	}
	for _, n := range names {
		if err := g.addMirrorSource(ctx, cl.JS, n, blue[n]); err != nil {
			return mirrorView{}, fmt.Errorf("%s: %w", n, err)
		}
	}
	return g.mirrorStatus(ctx)
}

// addMirrorSource adds the mirror source to green's stream name,
// creating the stream (gwag's event stream config, blue's subjects)
// when green has none.
func (g *Gateway) addMirrorSource(ctx context.Context, js jetstream.JetStream, name string, subjects []string) error {
	g.mu.Lock()
	replicas := g.eventStreamReplicas()
	pt := g.peers
	g.mu.Unlock()
	created := false
	_, err := applyStream(ctx, js, name, g.isJetStreamLeader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
		if cur != nil {
			cfg := *cur
			if !hasMirrorSource(cfg.Sources) {
				cfg.Sources = append(slices.Clone(cfg.Sources), mirrorSource(name))
			}
			return cfg, true
		}
		r := replicas
		if pt != nil {
			r = max(r, int(pt.currentR.Load()))
		}
		subject := ""
		if len(subjects) > 0 {
			subject = subjects[0]
		}
		cfg := g.eventStreamConfig(name, subject, r)
		cfg.Subjects = slices.Clone(subjects)
		cfg.Sources = []*jetstream.StreamSource{mirrorSource(name)}
		created = true
		return cfg, true
	})
	if err != nil || !created || len(subjects) != 1 {
		return err
	}
	// Created here: replica changes cover it like a stream this node
	// ensured (setEventStreamReplicas).
	st := &g.eventStreams
	st.mu.Lock()
	if st.known == nil {
		st.known = map[string]string{}
	}
	if _, ok := st.known[name]; !ok {
		st.known[name] = subjects[0]
	}
	st.mu.Unlock()
	return nil
}

// stopMirror removes the mirror source from green's streams: every
// mirrored one, or the listed ones. The streams stay, with their
// messages, as normal streams.
func (g *Gateway) stopMirror(ctx context.Context, only []string) (mirrorView, error) {
	cl, _, err := g.mirrorState()
	if err != nil {
		return mirrorView{}, err
	}
	mirrored := map[string][]string{}
	l := cl.JS.ListStreams(ctx)
	for info := range l.Info() {
		if strings.HasPrefix(info.Config.Name, eventStreamPrefix) && hasMirrorSource(info.Config.Sources) {
			mirrored[info.Config.Name] = info.Config.Subjects
		}
	}
	if err := l.Err(); err != nil {
		return mirrorView{}, err
	}
	names, err := pickStreams(mirrored, only)
	if err != nil {
		return mirrorView{}, err
	}
	for _, n := range names {
		_, err := applyStream(ctx, cl.JS, n, g.isJetStreamLeader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
			if cur == nil {
				return jetstream.StreamConfig{}, false
			}
			cfg := *cur
			cfg.Sources = withoutMirrorSource(cur.Sources)
			return cfg, true
		})
		if err != nil && !errors.Is(err, errStreamMissing) {
			return mirrorView{}, fmt.Errorf("%s: %w", n, err)
		}
	}
	return g.mirrorStatus(ctx)
}

// mirrorStreamView is one event stream in the mirror status.
type mirrorStreamView struct {
	Stream   string   `json:"stream"`
	Subjects []string `json:"subjects"`
	Mirrored bool     `json:"mirrored" doc:"green's stream sources blue's"`
	OnGreen  bool     `json:"onGreen"`
	OnBlue   bool     `json:"onBlue"`
	// Sequences are each side's own: a sourced message gets green's
	// next sequence.
	BlueLastSeq  uint64 `json:"blueLastSeq"`
	BlueMsgs     uint64 `json:"blueMsgs"`
	GreenLastSeq uint64 `json:"greenLastSeq"`
	GreenMsgs    uint64 `json:"greenMsgs"`
	Lag          uint64 `json:"lag" doc:"messages on blue not yet on green, as the server last saw it (StreamSourceInfo.Lag)"`
	ActiveMs     int64  `json:"activeMs" doc:"ms since the source last delivered or sent a heartbeat; -1: never"`
	Error        string `json:"error,omitempty" doc:"the source's replication error, if any"`
}

// mirrorView is the mirror status of a green gateway.
type mirrorView struct {
	Configured bool               `json:"configured"`
	From       string             `json:"from"`
	Leafnodes  int                `json:"leafnodes" doc:"leafnode connections of this node (the mirror connection to blue)"`
	BlueError  string             `json:"blueError,omitempty" doc:"blue's streams could not be read; their columns are 0"`
	Streams    []mirrorStreamView `json:"streams"`
}

// mirrorStatus reports, per event stream on either side, whether it
// is mirrored, both sides' last sequence and message count, and the
// lag NATS reports.
func (g *Gateway) mirrorStatus(ctx context.Context) (mirrorView, error) {
	cl, m, err := g.mirrorState()
	if err != nil {
		return mirrorView{}, err
	}
	v := mirrorView{Configured: true, From: m.fromString(), Leafnodes: cl.Server.NumLeafNodes(), Streams: []mirrorStreamView{}}
	byName := map[string]*mirrorStreamView{}
	get := func(n string) *mirrorStreamView {
		if s := byName[n]; s != nil {
			return s
		}
		s := &mirrorStreamView{Stream: n, ActiveMs: -1}
		byName[n] = s
		return s
	}
	l := cl.JS.ListStreams(ctx)
	for info := range l.Info() {
		if !strings.HasPrefix(info.Config.Name, eventStreamPrefix) {
			continue
		}
		s := get(info.Config.Name)
		s.OnGreen = true
		s.Subjects = info.Config.Subjects
		s.GreenLastSeq, s.GreenMsgs = info.State.LastSeq, info.State.Msgs
		s.Mirrored = hasMirrorSource(info.Config.Sources)
		for _, si := range info.Sources {
			if si == nil || si.External == nil || si.External.APIPrefix != mirrorAPIPrefix {
				continue
			}
			s.Lag = si.Lag
			if si.Active >= 0 {
				s.ActiveMs = si.Active.Milliseconds()
			}
			if si.Error != nil {
				s.Error = si.Error.Error()
			}
		}
	}
	if err := l.Err(); err != nil {
		return mirrorView{}, err
	}
	if err := g.readBlueSeqs(ctx, m, get); err != nil {
		v.BlueError = err.Error()
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := byName[n]
		if s.Subjects == nil {
			s.Subjects = []string{}
		}
		v.Streams = append(v.Streams, *s)
	}
	return v, nil
}

func (g *Gateway) readBlueSeqs(ctx context.Context, m *clusterMirror, get func(string) *mirrorStreamView) error {
	bjs, err := m.blueStreams()
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	l := bjs.ListStreams(c)
	for info := range l.Info() {
		if !strings.HasPrefix(info.Config.Name, eventStreamPrefix) {
			continue
		}
		s := get(info.Config.Name)
		s.OnBlue = true
		if s.Subjects == nil {
			s.Subjects = info.Config.Subjects
		}
		s.BlueLastSeq, s.BlueMsgs = info.State.LastSeq, info.State.Msgs
	}
	return l.Err()
}
