package drive

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/reader"
)

// Telegram file references are anti-hotlinking tokens that go stale after
// roughly an hour. Reading with a stale one fails, which is recoverable:
// re-read the message, take the fresh reference, retry.
//
// The awkward part is that a stale reference is discovered by every concurrent
// chunk fetch at once. Without coordination, a six-way parallel read turns one
// expiry into six identical getMessages calls — exactly the kind of burst that
// earns a FLOOD_WAIT. singleflight collapses them into one.
//
// The second reason this machinery exists is multiple accounts. An access hash
// is minted for the account that asked for it, so the pair stored on a segment
// row is only usable by the account that uploaded it. Any other account has to
// resolve its own — the same getMessages round trip an expiry needs — which is
// what lets a file uploaded by one account be read by another. Handles are
// therefore cached per account, and only the owner writes its handle back to
// the database.

type refEntry struct {
	ref []byte
	dc  int
	// accessHash is the document handle held by the account this entry was
	// cached for. Zero means "not resolved by this account yet".
	accessHash int64
	expires    time.Time
}

type refCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]refEntry
	group   singleflight.Group
}

func newRefCache(ttl time.Duration) *refCache {
	return &refCache{ttl: ttl, entries: make(map[string]refEntry)}
}

// refKey namespaces a cached handle by the account that resolved it. The
// account goes last so that Forget can still match every entry for a file with
// a plain prefix test.
func refKey(fileID string, idx int, accountID string) string {
	return fileID + ":" + strconv.Itoa(idx) + "|" + accountID
}

func (c *refCache) get(key string) (refEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return refEntry{}, false
	}
	return e, true
}

func (c *refCache) put(key string, ref []byte, dc int, accessHash int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = refEntry{
		ref:        ref,
		dc:         dc,
		accessHash: accessHash,
		expires:    time.Now().Add(c.ttl),
	}
}

func (c *refCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Forget clears every cached reference for a file, used when its messages are
// deleted or their captions rewritten.
func (c *refCache) Forget(fileID string) {
	prefix := fileID + ":"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// segState is what a source has learned about its segment since the stream
// opened: a refreshed file reference, a corrected datacenter, a document handle
// resolved for the reading account, or all three.
//
// It is held on the source rather than only in refCache because correctness
// must not depend on a cache. The chunks of one read run concurrently and would
// otherwise each rediscover the same expiry or migration, and a cache with a
// short TTL would make them rediscover it again on the next chunk.
type segState struct {
	ref        []byte
	dc         int
	accessHash int64
}

// segmentSource reads one segment's bytes through one account, transparently
// repairing an expired file reference, a datacenter migration, or the absence
// of a handle this account can use.
type segmentSource struct {
	svc     *Service
	account Account
	file    database.File
	seg     database.Segment
	channel database.Channel
	chRef   ChannelRef
	key     string

	state atomic.Pointer[segState]
}

func (s *segmentSource) doc(st segState) StoredDoc {
	return StoredDoc{
		MsgID:         s.seg.TGMsgID,
		DocID:         s.seg.TGDocID,
		AccessHash:    st.accessHash,
		DCID:          st.dc,
		FileReference: st.ref,
		Size:          s.seg.Size,
	}
}

// ownsStoredHandle reports whether the stored access hash and file reference
// were minted for the account doing the reading. They are not portable: another
// account presenting them gets an error rather than bytes.
//
// A segment with no recorded owner — written before accounts existed, or
// recovered by an index rebuild — is treated as belonging to nobody, so the
// reader resolves a fresh handle instead of gambling on a stale one.
func (s *segmentSource) ownsStoredHandle() bool {
	return s.seg.AccountID != "" && s.seg.AccountID == s.account.ID()
}

func (s *segmentSource) Chunk(ctx context.Context, offset, limit int64) ([]byte, error) {
	st, ok := s.current()
	if !ok {
		// This account has no handle it can use for this segment, which is the
		// normal state when another account uploaded it. Resolving one is the
		// same round trip an expired reference needs.
		fresh, err := s.refresh(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve segment %d for this account: %w", s.seg.Index, err)
		}
		st = fresh
	}

	data, err := s.account.ReadChunk(ctx, s.chRef, s.doc(st), offset, limit)
	if err == nil {
		return data, nil
	}

	// A migration says exactly where the document lives; remember it so the
	// rest of this download and every later one go straight there.
	if newDC, ok := s.account.MigratedDC(err); ok && newDC != st.dc {
		moved := st
		moved.dc = newDC
		s.remember(moved)
		s.svc.persistDC(ctx, s.file.ID, s.seg.Index, newDC)
		return s.account.ReadChunk(ctx, s.chRef, s.doc(moved), offset, limit)
	}

	if !s.account.IsReferenceExpired(err) {
		return nil, err
	}

	fresh, refreshErr := s.refresh(ctx)
	if refreshErr != nil {
		return nil, fmt.Errorf("refresh file reference for segment %d: %w", s.seg.Index, refreshErr)
	}
	return s.account.ReadChunk(ctx, s.chRef, s.doc(fresh), offset, limit)
}

// current returns the best-known handle for the reading account: what this
// stream has already learned, then this account's cache entry, then the stored
// row — but the stored row only when this account is the one that put it there.
//
// The false return is the important case. A handle belonging to another account
// is not a worse handle, it is an unusable one, so the caller must resolve
// rather than try it and hope.
func (s *segmentSource) current() (segState, bool) {
	if st := s.state.Load(); st != nil {
		return *st, true
	}
	if e, ok := s.svc.refs.get(s.key); ok && e.accessHash != 0 {
		return segState{ref: e.ref, dc: e.dc, accessHash: e.accessHash}, true
	}
	if s.ownsStoredHandle() {
		return segState{
			ref:        s.seg.FileReference,
			dc:         s.seg.DCID,
			accessHash: s.seg.AccessHash,
		}, true
	}
	return segState{}, false
}

// remember records a repair for the rest of this stream and, through the
// cache, for streams that start soon after on the same account.
func (s *segmentSource) remember(st segState) {
	s.state.Store(&st)
	s.svc.refs.put(s.key, st.ref, st.dc, st.accessHash)
}

// refresh re-reads the message backing this segment, yielding a handle minted
// for the reading account. Concurrent callers share one round trip.
func (s *segmentSource) refresh(ctx context.Context) (segState, error) {
	s.svc.refs.drop(s.key)
	s.state.Store(nil)

	res, err, _ := s.svc.refs.group.Do(s.key, func() (any, error) {
		doc, err := s.account.RefreshDoc(ctx, s.chRef, s.seg.TGMsgID)
		if err != nil {
			return nil, err
		}

		st := segState{ref: doc.FileReference, dc: doc.DCID, accessHash: doc.AccessHash}
		s.remember(st)

		// Only the owning account writes back. Another account's access hash
		// and file reference would be worse than useless to the owner: it would
		// present them on its next read and fail.
		if s.ownsStoredHandle() {
			if err := s.svc.db.RefreshFileReference(
				ctx, s.file.ID, s.seg.Index, doc.FileReference, s.account.ID()); err != nil {
				s.svc.log.Warn("could not persist a refreshed file reference",
					zap.String("file", s.file.ID), zap.Int("segment", s.seg.Index), zap.Error(err))
			}
		}
		// Which datacenter holds a document is a property of the document, not
		// of the account asking, so this is always worth storing.
		s.svc.persistDC(ctx, s.file.ID, s.seg.Index, doc.DCID)
		return st, nil
	})
	if err != nil {
		return segState{}, err
	}
	return res.(segState), nil
}

// persistDC records where a document really lives, so later reads skip the
// migration round trip.
func (s *Service) persistDC(ctx context.Context, fileID string, idx, dc int) {
	if dc <= 0 {
		return
	}
	if err := s.db.SetSegmentDC(ctx, fileID, idx, dc); err != nil {
		s.log.Warn("could not persist a segment's datacenter",
			zap.String("file", fileID), zap.Int("segment", idx), zap.Error(err))
	}
}

// SegmentOwner is the account that uploaded a file's first segment, which is
// the account a reader should prefer: it already holds usable handles, so
// choosing it saves one round trip per segment. An empty result means nobody in
// particular, and any account will do.
func (s *Service) SegmentOwner(ctx context.Context, fileID string) string {
	if fileID == "" {
		return ""
	}
	segs, err := s.db.Segments(ctx, fileID)
	if err != nil {
		return ""
	}
	for _, seg := range segs {
		if seg.AccountID != "" {
			return seg.AccountID
		}
	}
	return ""
}

// OpenFile returns a seekable stream over a whole logical file, read through
// one account. Callers get one continuous file no matter how many Telegram
// documents back it.
//
// The account is passed in rather than chosen here because it comes with the
// caller's download slot: the reader must run on the account whose budget was
// reserved, not on whichever one happens to look idle mid-stream.
func (s *Service) OpenFile(ctx context.Context, f database.File, account Account) (*reader.File, error) {
	if account == nil {
		return nil, ErrNoAccount
	}
	request := struct {
		FileID string `json:"fileId"`
	}{FileID: f.ID}
	operation, err := s.beforePluginOperation(ctx, "files.open", request, &request)
	if err != nil {
		return nil, err
	}
	f, err = s.db.FileByID(ctx, request.FileID)
	if err != nil {
		return nil, err
	}
	segs, err := s.db.Segments(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 && f.Size > 0 {
		return nil, fmt.Errorf("file %q has no stored segments", f.Name)
	}

	channel, err := s.channelFor(ctx, f.ChannelID)
	if err != nil {
		return nil, err
	}
	chRef, err := channelRef(ctx, account, channel)
	if err != nil {
		return nil, err
	}

	byIndex := make(map[int]database.Segment, len(segs))
	for _, seg := range segs {
		byIndex[seg.Index] = seg
	}

	sourceFor := func(_ context.Context, idx int) (reader.Source, error) {
		seg, ok := byIndex[idx]
		if !ok {
			// A missing segment is why files get marked broken; say so
			// plainly rather than returning zeros.
			return nil, fmt.Errorf("file %q is missing segment %d of %d",
				f.Name, idx, f.SegmentCount)
		}
		return &segmentSource{
			svc:     s,
			account: account,
			file:    f,
			seg:     seg,
			channel: channel,
			chRef:   chRef,
			key:     refKey(f.ID, idx, account.ID()),
		}, nil
	}

	settings := s.cfg.RuntimeSettings()
	opened, err := reader.Open(ctx, sourceFor, f.Size, f.SegmentSize, reader.Options{
		Concurrency:  settings.StreamConcurrency,
		Buffers:      s.cfg.Stream.Buffers,
		ChunkTimeout: s.cfg.Stream.ChunkTimeout,
	})
	if err != nil {
		return nil, err
	}
	s.afterPluginOperation(ctx, operation)
	return opened, nil
}
