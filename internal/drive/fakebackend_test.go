package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// fakeTelegram is an in-memory stand-in for a Telegram channel. It enforces the
// constraints that actually bite in production — a 1 MiB read ceiling that may
// not straddle a 1 MiB boundary, an exact upload length, expiring file
// references — so a test that passes here is exercising the same edge cases the
// real service would.
type fakeTelegram struct {
	mu       sync.Mutex
	messages map[int]*fakeMessage
	nextID   int
	nextDoc  int64

	ready bool

	// migrateTo, when set, makes every read that asks the wrong datacenter
	// fail with a migration error — which is what Telegram does, to every
	// such request rather than only the first.
	migrateTo int

	reads   atomic.Int64
	uploads atomic.Int64
}

type fakeMessage struct {
	caption string
	// body is nil for caption-only records: directories and empty files.
	body       []byte
	docID      int64
	accessHash int64
	dc         int
	// generation bumps on every caption edit, invalidating outstanding file
	// references exactly as Telegram does.
	fileRef []byte
}

var (
	errRefExpired = errors.New("FILE_REFERENCE_EXPIRED")
	errMigrate    = errors.New("FILE_MIGRATE")
)

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{
		messages: make(map[int]*fakeMessage),
		nextID:   1000,
		nextDoc:  5000,
		ready:    true,
	}
}

func (f *fakeTelegram) Ready() bool { return f.ready }

func (f *fakeTelegram) Upload(ctx context.Context, _ ChannelRef, spec UploadSpec) (StoredDoc, error) {
	if err := ctx.Err(); err != nil {
		return StoredDoc{}, err
	}
	f.uploads.Add(1)

	// Telegram commits to a part count from the declared size, so a body that
	// does not match exactly is a bug worth failing loudly on.
	body, err := io.ReadAll(io.LimitReader(spec.Body, spec.Size+1))
	if err != nil {
		return StoredDoc{}, err
	}
	if int64(len(body)) != spec.Size {
		return StoredDoc{}, fmt.Errorf(
			"upload declared %d bytes but the body had %d", spec.Size, len(body))
	}
	if spec.Progress != nil {
		spec.Progress(spec.Size, spec.Size)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	f.nextDoc++
	msg := &fakeMessage{
		caption:    spec.Caption,
		body:       body,
		docID:      f.nextDoc,
		accessHash: f.nextDoc * 7,
		dc:         2,
		fileRef:    []byte(fmt.Sprintf("ref-%d-0", f.nextDoc)),
	}
	f.messages[f.nextID] = msg

	return StoredDoc{
		MsgID:         f.nextID,
		DocID:         msg.docID,
		AccessHash:    msg.accessHash,
		DCID:          msg.dc,
		FileReference: msg.fileRef,
		Size:          int64(len(body)),
	}, nil
}

func (f *fakeTelegram) SendRecord(_ context.Context, _ ChannelRef, caption string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.messages[f.nextID] = &fakeMessage{caption: caption}
	return f.nextID, nil
}

func (f *fakeTelegram) EditRecord(_ context.Context, _ ChannelRef, msgID int, caption string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.messages[msgID]
	if !ok {
		return fmt.Errorf("MESSAGE_ID_INVALID: %d", msgID)
	}
	msg.caption = caption
	// An edit rotates the file reference, which is why the drive drops its
	// cache after a rename.
	if msg.body != nil {
		msg.fileRef = []byte(fmt.Sprintf("ref-%d-%d", msg.docID, len(caption)))
	}
	return nil
}

func (f *fakeTelegram) DeleteRecords(_ context.Context, _ ChannelRef, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.messages, id)
	}
	return nil
}

func (f *fakeTelegram) ReadChunk(
	ctx context.Context,
	_ ChannelRef,
	doc StoredDoc,
	offset, limit int64,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.reads.Add(1)

	if limit > 1<<20 {
		return nil, fmt.Errorf("limit %d exceeds Telegram's 1 MiB ceiling", limit)
	}
	if offset/(1<<20) != (offset+limit-1)/(1<<20) {
		return nil, fmt.Errorf("read [%d,%d) straddles a 1 MiB boundary", offset, offset+limit)
	}

	if f.migrateTo > 0 && doc.DCID != f.migrateTo {
		return nil, errMigrate
	}

	f.mu.Lock()
	msg := f.find(doc.DocID)
	var current []byte
	if msg != nil {
		current = msg.fileRef
	}
	f.mu.Unlock()
	if msg == nil {
		return nil, fmt.Errorf("document %d is gone", doc.DocID)
	}

	// A reference that no longer matches the message is exactly how Telegram
	// reports an expired one.
	if string(doc.FileReference) != string(current) {
		return nil, errRefExpired
	}

	if offset >= int64(len(msg.body)) {
		return nil, nil
	}
	end := min(offset+limit, int64(len(msg.body)))
	out := make([]byte, end-offset)
	copy(out, msg.body[offset:end])
	return out, nil
}

func (f *fakeTelegram) RefreshDoc(_ context.Context, _ ChannelRef, msgID int) (StoredDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.messages[msgID]
	if !ok {
		return StoredDoc{}, fmt.Errorf("message %d is gone", msgID)
	}
	dc := msg.dc
	if f.migrateTo > 0 {
		dc = f.migrateTo
	}
	return StoredDoc{
		MsgID:         msgID,
		DocID:         msg.docID,
		AccessHash:    msg.accessHash,
		DCID:          dc,
		FileReference: msg.fileRef,
		Size:          int64(len(msg.body)),
	}, nil
}

func (f *fakeTelegram) IsReferenceExpired(err error) bool { return errors.Is(err, errRefExpired) }

func (f *fakeTelegram) MigratedDC(err error) (int, bool) {
	if errors.Is(err, errMigrate) {
		return f.migrateTo, true
	}
	return 0, false
}

// find locates a message by document id. Callers hold f.mu.
func (f *fakeTelegram) find(docID int64) *fakeMessage {
	for _, msg := range f.messages {
		if msg.docID == docID {
			return msg
		}
	}
	return nil
}

// expireRefs rotates every file reference, simulating the hour or so after
// which Telegram stops honouring the ones handed out at upload time.
func (f *fakeTelegram) expireRefs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, msg := range f.messages {
		if msg.body != nil {
			msg.fileRef = append([]byte("aged-"), msg.fileRef...)
		}
	}
}

// captions returns every stored caption, which is what the indexer would read.
func (f *fakeTelegram) captions() map[int]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int]string, len(f.messages))
	for id, msg := range f.messages {
		out[id] = msg.caption
	}
	return out
}

func (f *fakeTelegram) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}
