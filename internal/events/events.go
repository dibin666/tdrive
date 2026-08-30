// Package events broadcasts progress to connected browsers over SSE.
//
// Uploads and index rebuilds are long enough that polling either lags or
// hammers the API, and both are things a user watches. Server-sent events fit
// because the traffic is one-directional and survives proxies that would block
// a WebSocket.
package events

import (
	"encoding/json"
	"sync"
	"time"
)

// Type identifies what an event is about, so the browser can route it without
// inspecting the payload.
type Type string

const (
	// TypeUpload carries an upload job's progress.
	TypeUpload Type = "upload"
	// TypeDownload carries a download job's progress. Downloads are separate
	// from uploads on the wire because the browser drives some of them itself
	// and only reports in, while the server drives the staged ones end to end.
	TypeDownload Type = "download"
	// TypeIndex carries index rebuild progress.
	TypeIndex Type = "index"
	// TypeTelegram carries connection state changes, so the setup wizard
	// advances without polling.
	TypeTelegram Type = "telegram"
	// TypeTree signals that a directory's contents changed, prompting the
	// browser to refetch a listing.
	TypeTree Type = "tree"
)

// Event is one message.
type Event struct {
	Type Type  `json:"type"`
	Data any   `json:"data"`
	At   int64 `json:"at"`
	// UserID scopes an event to one account; empty means everyone. It is
	// included in the internal payload so plugin subscribers retain the same
	// scope information as the browser broker.
	UserID string `json:"userId,omitempty"`
}

// Broker fans events out to subscribers.
type Broker struct {
	mu   sync.RWMutex
	subs map[int]*subscriber
	next int
}

type subscriber struct {
	ch     chan []byte
	userID string
}

func NewBroker() *Broker {
	return &Broker{subs: make(map[int]*subscriber)}
}

// Subscribe returns a channel of encoded events and a function to release it.
func (b *Broker) Subscribe(userID string) (<-chan []byte, func()) {
	// A buffer absorbs the bursts that a fast upload produces without making
	// the publisher wait on a slow browser.
	ch := make(chan []byte, 64)

	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = &subscriber{ch: ch, userID: userID}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if s, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
		}
		b.mu.Unlock()
	}
}

// Publish delivers an event to matching subscribers.
//
// A subscriber whose buffer is full is skipped rather than waited for: progress
// is a stream of snapshots, so dropping one is harmless, whereas blocking here
// would stall the upload that produced it.
func (b *Broker) Publish(ev Event) {
	ev.At = time.Now().UnixMilli()
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if ev.UserID != "" && s.userID != ev.UserID {
			continue
		}
		select {
		case s.ch <- payload:
		default:
		}
	}
}

// UploadProgress is the payload of a TypeUpload event.
//
// It carries enough to redraw a transfer row on its own. The transfer panel
// used to answer an event by refetching the whole list, which meant the numbers
// only moved as fast as that round trip; now the event is the update and the
// refetch is just housekeeping.
type UploadProgress struct {
	JobID        string `json:"jobId"`
	FileID       string `json:"fileId,omitempty"`
	Name         string `json:"name"`
	Uploaded     int64  `json:"uploaded"`
	Total        int64  `json:"total"`
	Segment      int    `json:"segment"`
	SegmentCount int    `json:"segmentCount"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Source       string `json:"source,omitempty"`
	SourceURL    string `json:"sourceUrl,omitempty"`
	// Speed is bytes per second right now. Only the server can measure it for
	// the transfers it drives itself, which is every WebDAV write, VPS-local
	// upload and remote fetch.
	Speed float64 `json:"speed,omitempty"`
}

// DownloadProgress is the payload of a TypeDownload event.
type DownloadProgress struct {
	JobID      string  `json:"jobId"`
	FileID     string  `json:"fileId,omitempty"`
	Name       string  `json:"name"`
	Downloaded int64   `json:"downloaded"`
	Total      int64   `json:"total"`
	Mode       string  `json:"mode,omitempty"`
	Status     string  `json:"status"`
	Error      string  `json:"error,omitempty"`
	Speed      float64 `json:"speed,omitempty"`
}

// IndexProgress is the payload of a TypeIndex event.
type IndexProgress struct {
	Scanned  int    `json:"scanned"`
	Dirs     int    `json:"dirs"`
	Files    int    `json:"files"`
	Segments int    `json:"segments"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// TreeChanged is the payload of a TypeTree event.
type TreeChanged struct {
	Path string `json:"path"`
}
