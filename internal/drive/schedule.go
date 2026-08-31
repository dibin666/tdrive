package drive

import (
	"context"
	"errors"
	"fmt"
)

// Choosing which Telegram account runs a transfer.
//
// The rule the settings page promises is that the configured task limits are
// per account: "one upload at a time" with two accounts means two uploads at
// once, each on its own login. That falls out of giving every account its own
// taskLimiter — the total is the limit times the number of accounts, and no
// account can be pushed past its own share no matter how busy the drive is.
//
// Isolation is the point. A transfer holds a slot on exactly one account for
// its whole life, so a browser upload's segments and a parallel download's
// range requests never straddle two logins. That matters beyond tidiness:
// Telegram mints access hashes per account, so a segment fetched with the wrong
// one simply fails.

// ErrNoAccount is returned when no Telegram account can take the work: none is
// configured, none is signed in, or none has been admitted to the channel.
var ErrNoAccount = errors.New("drive: no telegram account is available")

// lease is an account plus the task slot reserved on it. release must be called
// exactly once; it is idempotent.
type lease struct {
	account Account
	release func()
}

// syncLimiters brings the per-account limiter set in line with the accounts the
// cluster currently offers and the limits currently configured.
//
// Limiters for accounts that went away are dropped rather than stopped: a
// transfer still holding a slot owns its release closure, so it finishes
// cleanly against a limiter nothing else can reach.
func (s *Service) syncLimiters() []Account {
	accounts := s.cluster.Accounts()
	settings := s.cfg.RuntimeSettings()

	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	live := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		id := account.ID()
		live[id] = struct{}{}

		if limiter, ok := s.uploadLimiters[id]; ok {
			limiter.setLimit(settings.UploadConcurrency)
		} else {
			s.uploadLimiters[id] = newTaskLimiter(settings.UploadConcurrency)
		}
		if limiter, ok := s.downloadLimiters[id]; ok {
			limiter.setLimit(settings.DownloadConcurrency)
		} else {
			s.downloadLimiters[id] = newTaskLimiter(settings.DownloadConcurrency)
		}
	}
	for id := range s.uploadLimiters {
		if _, ok := live[id]; !ok {
			delete(s.uploadLimiters, id)
		}
	}
	for id := range s.downloadLimiters {
		if _, ok := live[id]; !ok {
			delete(s.downloadLimiters, id)
		}
	}
	return accounts
}

// limiterFor returns an account's limiter for the given direction, creating it
// if the account appeared between a sync and this call.
func (s *Service) limiterFor(id string, upload bool) *taskLimiter {
	settings := s.cfg.RuntimeSettings()

	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	set, limit := s.downloadLimiters, settings.DownloadConcurrency
	if upload {
		set, limit = s.uploadLimiters, settings.UploadConcurrency
	}
	if limiter, ok := set[id]; ok {
		return limiter
	}
	limiter := newTaskLimiter(limit)
	set[id] = limiter
	return limiter
}

// order arranges candidates least-loaded first, with prefer promoted to the
// front and a rotating cursor breaking ties.
//
// prefer exists for downloads: the account that uploaded a segment already
// holds a usable handle for it, so choosing it saves a round trip. It is a
// preference rather than a rule, because pinning reads to the uploader would
// leave every file written before a second account existed unable to use it.
func (s *Service) order(accounts []Account, limiters []*taskLimiter, prefer string) []int {
	idx := make([]int, len(accounts))
	for i := range idx {
		idx[i] = i
	}

	cursor := int(s.schedCursor.Add(1))
	rank := func(i int) (int, int, int) {
		preferred := 1
		if prefer != "" && accounts[i].ID() == prefer {
			preferred = 0
		}
		// Rotate the tiebreak so equally idle accounts take turns.
		return preferred, limiters[i].activeCount(), (i + cursor) % max(len(accounts), 1)
	}
	// A handful of accounts at most; an insertion sort keeps the comparison
	// readable and avoids pulling in a closure-heavy sort for six elements.
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0; j-- {
			ap, aa, ac := rank(idx[j])
			bp, ba, bc := rank(idx[j-1])
			if ap < bp || (ap == bp && aa < ba) || (ap == bp && aa == ba && ac < bc) {
				idx[j], idx[j-1] = idx[j-1], idx[j]
				continue
			}
			break
		}
	}
	return idx
}

// acquire reserves a task slot on one account.
//
// An idle account is taken immediately. When every account is at its limit the
// call blocks on all of them at once and takes whichever frees a slot first,
// which is what makes a busy drive drain evenly instead of queueing everything
// behind the account that happens to be first in the list.
func (s *Service) acquire(ctx context.Context, upload bool, prefer string) (*lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	accounts := s.syncLimiters()
	if len(accounts) == 0 {
		if !s.cluster.Ready() {
			return nil, ErrNoAccount
		}
		return nil, fmt.Errorf("%w: no account can post to the storage channel", ErrNoAccount)
	}

	limiters := make([]*taskLimiter, len(accounts))
	for i, account := range accounts {
		limiters[i] = s.limiterFor(account.ID(), upload)
	}

	ordered := s.order(accounts, limiters, prefer)
	for _, i := range ordered {
		if release, ok := limiters[i].tryAcquire(); ok {
			return &lease{account: accounts[i], release: release}, nil
		}
	}

	queue := make([]*taskLimiter, len(ordered))
	for n, i := range ordered {
		queue[n] = limiters[i]
	}
	winner, release, err := acquireAny(ctx, queue)
	if err != nil {
		return nil, err
	}
	return &lease{account: accounts[ordered[winner]], release: release}, nil
}

// leaseUpload reserves an upload slot on some account.
func (s *Service) leaseUpload(ctx context.Context) (*lease, error) {
	return s.acquire(ctx, true, "")
}

// leaseDownload reserves a download slot, preferring the account named — in
// practice the one that uploaded the segments about to be read.
func (s *Service) leaseDownload(ctx context.Context, prefer string) (*lease, error) {
	return s.acquire(ctx, false, prefer)
}

// metaAccount picks an account for a metadata operation: creating a directory
// record, rewriting a caption after a rename, deleting messages.
//
// These take no task slot. They are single small RPCs, and making a rename
// queue behind two multi-gigabyte uploads would be a strange thing to do to
// someone who pressed F2. Every account is granted edit and delete rights when
// it joins the channel, so any of them can amend a record another one wrote.
func (s *Service) metaAccount(ctx context.Context) (Account, error) {
	accounts := s.cluster.Accounts()
	if len(accounts) == 0 {
		return nil, ErrNoAccount
	}
	cursor := int(s.schedCursor.Add(1))
	return accounts[cursor%len(accounts)], nil
}

// ReadAccount picks an account for a read that does not occupy a transfer
// slot — a plugin pulling a bounded chunk, for instance. It prefers the account
// that uploaded the file, which already holds handles for it.
func (s *Service) ReadAccount(ctx context.Context, fileID string) (Account, error) {
	return s.accountFor(ctx, s.SegmentOwner(ctx, fileID))
}

// accountFor returns a specific account, falling back to a scheduled one when
// the named account is gone or cannot serve. An unknown id is normal: it is
// what a segment uploaded by a since-removed account carries.
func (s *Service) accountFor(ctx context.Context, id string) (Account, error) {
	if id != "" {
		if account, ok := s.cluster.Account(id); ok && account.Available() {
			return account, nil
		}
	}
	return s.metaAccount(ctx)
}
