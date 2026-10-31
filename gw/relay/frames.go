package relay

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

// compressThreshold is the smallest frame worth compressing. Below it a
// deflate block's overhead eats the gain.
const compressThreshold = 512

// maxSharedIDs caps prepared frames per event. A PreparedMessage is worth it
// only if sockets share it; building one per socket costs more than a plain
// write, so past this many ids a frame goes out on its own.
const maxSharedIDs = 8

// frameCache lets the sockets on a channel share one compressed frame, so a
// fan-out compresses once rather than once per socket. A frame holds the
// client's sub id, so it is shared per (event, id). The channel keeps only its
// latest event: deliveries of one event arrive together.
//
// Matching on payload bytes, not identity, keeps this correct without
// coalescing, where each socket gets its own copy of an event, and for a Source
// that reuses buffers.
//
// All methods accept a nil receiver (compression off).
type frameCache struct {
	mu       sync.Mutex
	channels map[string]*channelFrames
}

type channelFrames struct {
	refs int // guarded by frameCache.mu

	mu      sync.Mutex
	payload []byte // private copy
	byID    map[string]*websocket.PreparedMessage
}

func newFrameCache() *frameCache {
	return &frameCache{channels: map[string]*channelFrames{}}
}

// acquire keeps channel's entry alive until the matching release.
func (fc *frameCache) acquire(channel string) {
	if fc == nil {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	cf := fc.channels[channel]
	if cf == nil {
		cf = &channelFrames{}
		fc.channels[channel] = cf
	}
	cf.refs++
}

func (fc *frameCache) release(channel string) {
	if fc == nil {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	cf := fc.channels[channel]
	if cf == nil {
		return
	}
	if cf.refs--; cf.refs == 0 {
		delete(fc.channels, channel)
	}
}

// frame returns the shared wire form of data frame f, or nil when f should be
// written on its own: too small to compress, on a released channel, past
// maxSharedIDs, or not marshalable (the writer then reports the error and tears
// the socket down).
func (fc *frameCache) frame(channel string, f outFrame) *websocket.PreparedMessage {
	if fc == nil || len(f.Payload) < compressThreshold {
		return nil
	}
	fc.mu.Lock()
	cf := fc.channels[channel]
	fc.mu.Unlock()
	if cf == nil {
		return nil
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if !bytes.Equal(cf.payload, f.Payload) {
		cf.payload = bytes.Clone(f.Payload)
		cf.byID = map[string]*websocket.PreparedMessage{}
	}
	if pm := cf.byID[f.ID]; pm != nil {
		return pm
	}
	if len(cf.byID) >= maxSharedIDs {
		return nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	pm, err := websocket.NewPreparedMessage(websocket.TextMessage, b)
	if err != nil {
		return nil
	}
	cf.byID[f.ID] = pm
	return pm
}
