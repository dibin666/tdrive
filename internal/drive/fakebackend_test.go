package drive

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// fakeTelegram is an in-memory stand-in for a Telegram channel. It enforces the
// constraints that actually bite in production — a 1 MiB read ceiling that may
// not straddle a 1 MiB boundary, an exact upload length, expiring file
// references, and access hashes that are only valid for the account they were
// minted for — so a test that passes here is exercising the same edge cases the
// real service would.
//
// It doubles as the Cluster: one fake Telegram serves however many fake
// accounts a test asks for, which is what makes primary/fallback scheduling and
// cross-account reads testable without a network.
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

	accounts []*fakeAccount
}

type fakeMessage struct {
	caption string
	// body is nil for caption-only records: directories and empty files.
	body  []byte
	docID int64
	dc    int
	// fileRef rotates on every caption edit, invalidating outstanding
	// references exactly as Telegram does.
	fileRef []byte
}

// fakeAccount is one Telegram login onto the shared fake channel. Everything an
// account can observe that differs from its peers lives here: its id, whether
// the scheduler may use it, and the access hashes it mints.
type fakeAccount struct {
	tg *fakeTelegram
	id string

	// Daily quotas are zero (unlimited) unless a quota test opts in.
	uploadQuota   int64
	downloadQuota int64

	// unavailable stands in for a FLOOD_WAIT: the account is still signed in
	// and still works if asked, but the scheduler should route around it.
	unavailable atomic.Bool
	// uploads counts what this particular account stored, which is how a test
	// checks which primary/fallback account handled a transfer.
	uploads atomic.Int64
	reads   atomic.Int64
	// refreshes counts handle resolutions, which is how a test checks that a
	// cross-account read pays exactly one extra round trip and a same-account
	// read pays none.
	refreshes atomic.Int64
}

var (
	errRefExpired = errors.New("FILE_REFERENCE_EXPIRED")
	errMigrate    = errors.New("FILE_MIGRATE")
	// errWrongAccount is what Telegram effectively answers when an account
	// presents a handle minted for a different account.
	errWrongAccount = errors.New("FILE_ID_INVALID")
)

func newFakeTelegram() *fakeTelegram { return newFakeTelegramN(1) }

// newFakeTelegramN builds a cluster of n independent accounts over one channel.
func newFakeTelegramN(n int) *fakeTelegram {
	f := &fakeTelegram{
		messages: make(map[int]*fakeMessage),
		nextID:   1000,
		nextDoc:  5000,
		ready:    true,
	}
	for i := range n {
		f.accounts = append(f.accounts, &fakeAccount{tg: f, id: "acct-" + strconv.Itoa(i+1)})
	}
	return f
}

func (f *fakeTelegram) Ready() bool { return f.ready }

// Accounts implements Cluster. The slice keeps the first fake account as the
// primary and the rest as fallbacks. Like the real cluster it returns all
// signed-in accounts when every one is unavailable, so a test that throttles
// everything sees queueing rather than a hard failure.
func (f *fakeTelegram) Accounts() []Account {
	var out []Account
	for _, a := range f.accounts {
		if !a.unavailable.Load() {
			out = append(out, a)
		}
	}
	if len(out) > 0 || !f.ready {
		return out
	}
	for _, a := range f.accounts {
		out = append(out, a)
	}
	return out
}

func (f *fakeTelegram) Account(id string) (Account, bool) {
	for _, a := range f.accounts {
		if a.id == id {
			return a, true
		}
	}
	return nil, false
}

// account is the test-side accessor for one account, so a test can throttle it
// or read its counters.
func (f *fakeTelegram) account(i int) *fakeAccount { return f.accounts[i] }

func (a *fakeAccount) ID() string      { return a.id }
func (a *fakeAccount) Available() bool { return a.tg.ready && !a.unavailable.Load() }

func (a *fakeAccount) DailyQuota(upload bool) int64 {
	if upload {
		return a.uploadQuota
	}
	return a.downloadQuota
}

// ChannelRef mints this account's own view of the channel. The fake ignores the
// value when serving, but handing out a different one per account matches
// reality and keeps callers honest about asking the right account.
func (a *fakeAccount) ChannelRef(_ context.Context, channelID string) (ChannelRef, error) {
	return ChannelRef{TGID: 1, AccessHash: hashFor(1, a.id) + int64(len(channelID))}, nil
}

func (a *fakeAccount) Ready() bool { return a.tg.ready }

// hashFor derives the access hash a given account would be handed for a
// document. Deriving rather than storing is the point: it makes presenting
// another account's hash detectably wrong.
func hashFor(docID int64, accountID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(accountID))
	return docID*7 ^ int64(h.Sum64()&0x7fffffff)
}

func (a *fakeAccount) Upload(ctx context.Context, _ ChannelRef, spec UploadSpec) (StoredDoc, error) {
	f := a.tg
	if err := ctx.Err(); err != nil {
		return StoredDoc{}, err
	}
	f.uploads.Add(1)
	a.uploads.Add(1)

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
		caption: spec.Caption,
		body:    body,
		docID:   f.nextDoc,
		dc:      2,
		fileRef: []byte(fmt.Sprintf("ref-%d-0", f.nextDoc)),
	}
	f.messages[f.nextID] = msg

	return StoredDoc{
		MsgID:         f.nextID,
		DocID:         msg.docID,
		AccessHash:    hashFor(msg.docID, a.id),
		DCID:          msg.dc,
		FileReference: msg.fileRef,
		Size:          int64(len(body)),
	}, nil
}

func (a *fakeAccount) SendRecord(_ context.Context, _ ChannelRef, caption string) (int, error) {
	f := a.tg
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.messages[f.nextID] = &fakeMessage{caption: caption}
	return f.nextID, nil
}

func (a *fakeAccount) EditRecord(_ context.Context, _ ChannelRef, msgID int, caption string) error {
	f := a.tg
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

func (a *fakeAccount) DeleteRecords(_ context.Context, _ ChannelRef, ids []int) error {
	f := a.tg
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.messages, id)
	}
	return nil
}

func (a *fakeAccount) ReadChunk(
	ctx context.Context,
	_ ChannelRef,
	doc StoredDoc,
	offset, limit int64,
) ([]byte, error) {
	f := a.tg
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.reads.Add(1)
	a.reads.Add(1)

	if limit > 1<<20 {
		return nil, fmt.Errorf("limit %d exceeds Telegram's 1 MiB ceiling", limit)
	}
	if offset/(1<<20) != (offset+limit-1)/(1<<20) {
		return nil, fmt.Errorf("read [%d,%d) straddles a 1 MiB boundary", offset, offset+limit)
	}

	// A handle minted for another account is not merely stale, it is invalid —
	// no amount of refreshing the file reference would make it work.
	if doc.AccessHash != hashFor(doc.DocID, a.id) {
		return nil, errWrongAccount
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

func (a *fakeAccount) RefreshDoc(_ context.Context, _ ChannelRef, msgID int) (StoredDoc, error) {
	f := a.tg
	a.refreshes.Add(1)

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
		AccessHash:    hashFor(msg.docID, a.id),
		DCID:          dc,
		FileReference: msg.fileRef,
		Size:          int64(len(msg.body)),
	}, nil
}

func (a *fakeAccount) IsReferenceExpired(err error) bool { return errors.Is(err, errRefExpired) }

func (a *fakeAccount) MigratedDC(err error) (int, bool) {
	if errors.Is(err, errMigrate) {
		return a.tg.migrateTo, true
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
