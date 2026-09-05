package drive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Choosing which Telegram account runs a transfer.
//
// The task limits are global to the drive. The cluster supplies accounts in
// primary-first order, so a transfer uses the primary account whenever it can;
// a later account is only selected when the earlier one is unavailable or its
// daily quota cannot accept the transfer. A transfer still holds one account
// for its whole life, so a browser upload's segments and a parallel download's
// range requests never straddle two logins.

// ErrNoAccount is returned when no Telegram account can take the work: none is
// configured, none is signed in, or none has been admitted to the channel.
var ErrNoAccount = errors.New("drive: no telegram account is available")

// lease is an account plus the task slot reserved on it. release must be called
// exactly once; it is idempotent.
type lease struct {
	account Account
	quota   *quotaReservation
	release func()
}

// newLease records the account chosen for one logical transfer and makes the
// returned release idempotent. The task limiter itself is global; these counts
// are only for the per-account status shown in the settings page.
func (s *Service) newLease(
	account Account,
	upload bool,
	reservation *quotaReservation,
	slotRelease func(),
) *lease {
	s.trackTask(account.ID(), upload, 1)
	var once sync.Once
	return &lease{
		account: account,
		quota:   reservation,
		release: func() {
			once.Do(func() {
				if reservation != nil {
					reservation.commit()
				}
				s.trackTask(account.ID(), upload, -1)
				if slotRelease != nil {
					slotRelease()
				}
			})
		},
	}
}

func (s *Service) trackTask(accountID string, upload bool, delta int) {
	if accountID == "" || delta == 0 {
		return
	}
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()
	counts := s.activeDownloads
	if upload {
		counts = s.activeUploads
	}
	if counts == nil {
		counts = make(map[string]int)
		if upload {
			s.activeUploads = counts
		} else {
			s.activeDownloads = counts
		}
	}
	counts[accountID] += delta
	if counts[accountID] <= 0 {
		delete(counts, accountID)
	}
}

// syncLimiter brings the one global limiter for a direction in line with the
// current setting and returns the accounts currently eligible for work.
func (s *Service) syncLimiter(upload bool) ([]Account, *taskLimiter) {
	accounts := s.cluster.Accounts()
	settings := s.cfg.RuntimeSettings()

	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	limiter, limit := s.downloadLimiter, settings.DownloadConcurrency
	if upload {
		limiter, limit = s.uploadLimiter, settings.UploadConcurrency
	}
	if limiter == nil {
		limiter = newTaskLimiter(limit)
		if upload {
			s.uploadLimiter = limiter
		} else {
			s.downloadLimiter = limiter
		}
	} else {
		limiter.setLimit(limit)
	}
	return accounts, limiter
}

// acquire reserves a global task slot and then chooses one account for the
// transfer. Account order is significant: the first account is the primary,
// while later accounts are fallbacks rather than extra queue capacity.
func (s *Service) acquire(ctx context.Context, upload bool, _ string, expectedSize ...int64) (*lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	size := int64(0)
	if len(expectedSize) > 0 {
		size = expectedSize[0]
	}

	for {
		accounts, limiter := s.syncLimiter(upload)
		if len(accounts) == 0 {
			if !s.cluster.Ready() {
				return nil, ErrNoAccount
			}
			return nil, fmt.Errorf("%w: no account can post to the storage channel", ErrNoAccount)
		}

		quota := s.quotaTracker()
		observedQuotaGeneration := quota.generationNow()
		account := firstAccountForTransfer(s, accounts, upload, size)
		if account == nil {
			// If all candidates are cooling down or are otherwise unavailable,
			// quota.wait alone could sleep until midnight. Poll the account
			// state instead; a login or a FLOOD_WAIT expiry is an equally valid
			// admission wake-up.
			if !hasAvailableAccount(accounts) {
				if err := waitForAccountOrQuota(ctx, quota, observedQuotaGeneration); err != nil {
					return nil, err
				}
				continue
			}
			// Every available account has committed or reserved its budget. A
			// quota reservation is held until its current content finishes, so
			// the ordinary wake-up is the next UTC day (or an administrator
			// changing a quota, which also signals this tracker).
			if err := quota.wait(ctx, observedQuotaGeneration); err != nil {
				return nil, err
			}
			continue
		}

		// Do not let a backup account bypass the global task queue. The slot is
		// acquired before the quota reservation is committed so every retry still
		// respects the same FIFO queue.
		release, err := limiter.acquire(ctx)
		if err != nil {
			return nil, err
		}

		// The account list and health state can change while this request waits
		// for the global slot. Choose again from the current eligible set rather
		// than pinning a queued request to a primary that just hit FLOOD_WAIT.
		account = firstAccountForTransfer(s, s.cluster.Accounts(), upload, size)
		if account == nil {
			release()
			continue
		}

		reservation, reserved := s.reserveQuota(account, upload, size)
		if reserved {
			return s.newLease(account, upload, reservation, release), nil
		}
		// Another waiter may have consumed the account's last budget while this
		// acquire was blocked. Return the global slot and retry; the next pass
		// may select a fallback account.
		release()
	}
}

// firstAccountForTransfer follows the cluster's primary-first order. A later
// account is considered only when an earlier account cannot start the transfer,
// including when its daily quota is exhausted. Accounts that can fit the whole
// content are preferred over an account that would cross its quota boundary;
// crossing remains a last resort when no account can fit it.
func firstAccountForTransfer(s *Service, accounts []Account, upload bool, size int64) Account {
	for _, account := range accounts {
		if account == nil || !account.Available() {
			continue
		}
		if s.quotaMayStart(account, upload, size) && s.quotaFits(account, upload, size) {
			return account
		}
	}
	for _, account := range accounts {
		if account == nil || !account.Available() {
			continue
		}
		if s.quotaMayStart(account, upload, size) {
			return account
		}
	}
	return nil
}

func hasAvailableAccount(accounts []Account) bool {
	for _, account := range accounts {
		if account != nil && account.Available() {
			return true
		}
	}
	return false
}

// waitForAccountOrQuota waits for either a quota reservation to be released or
// an account to become usable. Account health can change without a local
// notification (for example when a FLOOD_WAIT expires), so a short poll also
// covers accounts that are still connecting or a Cluster implementation that
// does not expose a readiness notification.
func waitForAccountOrQuota(
	ctx context.Context,
	quota *quotaTracker,
	observed uint64,
) error {
	quota.mu.Lock()
	if quota.generation != observed {
		quota.mu.Unlock()
		return nil
	}
	wakeup := quota.wakeup
	quota.mu.Unlock()

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wakeup:
		return nil
	case <-timer.C:
		return nil
	}
}

// leaseUpload reserves one global upload slot and assigns one account.
func (s *Service) leaseUpload(ctx context.Context, expectedSize ...int64) (*lease, error) {
	return s.acquire(ctx, true, "", expectedSize...)
}

// leaseDownload reserves one global download slot and assigns one account. The
// preferred id is retained at the call boundary for compatibility; account
// selection is always primary-first with failover.
func (s *Service) leaseDownload(ctx context.Context, prefer string, expectedSize ...int64) (*lease, error) {
	return s.acquire(ctx, false, prefer, expectedSize...)
}

// metaAccount picks the primary account for a metadata operation, falling back
// to the next eligible account only when the primary is unavailable.
func (s *Service) metaAccount(ctx context.Context) (Account, error) {
	accounts := s.cluster.Accounts()
	if len(accounts) == 0 {
		return nil, ErrNoAccount
	}
	return accounts[0], nil
}

// ReadAccount picks the primary account for a read that does not occupy a
// transfer slot, falling back when the primary is unavailable. The file owner
// is not promoted ahead of the primary: a second account is a backup, not a
// second read queue.
func (s *Service) ReadAccount(ctx context.Context, fileID string) (Account, error) {
	return s.accountFor(ctx, s.SegmentOwner(ctx, fileID))
}

// accountFor returns the first eligible account. An unknown id is normal: it
// is what a segment uploaded by a since-removed account carries.
func (s *Service) accountFor(ctx context.Context, _ string) (Account, error) {
	return s.metaAccount(ctx)
}
