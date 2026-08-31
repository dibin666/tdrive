package tgc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// Manager implements drive.Account. The adapter lives here rather than in the
// drive package so that nothing above this layer has to import gotd types.
var _ drive.Account = (*Manager)(nil)

// channelAccessRefreshInterval is short enough to repair a rotated access hash
// before it causes a visible transfer failure, while avoiding a Telegram RPC
// for every segment request.
const channelAccessRefreshInterval = 5 * time.Minute

// ChannelRef resolves this account's own coordinates for a storage channel.
//
// The per-account row is the authoritative one, because Telegram mints an
// access hash for the requesting account. A recent row is used directly for
// speed; an old row is re-resolved through this account's own session so a
// rotated hash cannot make an otherwise usable account fail with
// CHANNEL_INVALID. The channels table remains the fallback for the primary
// account on databases written before per-account access rows existed.
func (m *Manager) ChannelRef(ctx context.Context, channelID string) (drive.ChannelRef, error) {
	acct := m.Account()
	channel, err := m.db.ChannelByID(ctx, channelID)
	if err != nil {
		return drive.ChannelRef{}, err
	}

	access, err := m.db.ChannelAccessFor(ctx, channelID, acct.ID)
	switch {
	case err == nil:
		if access.AccessHash != 0 && time.Since(access.CheckedAt) < channelAccessRefreshInterval {
			m.channelCheckedAt.Store(access.CheckedAt.UnixMilli())
			m.setChannelReady(access.CanPost)
			return drive.ChannelRef{ChannelID: channel.ID, TGID: channel.TGID, AccessHash: access.AccessHash}, nil
		}
	case !errors.Is(err, database.ErrNotFound):
		return drive.ChannelRef{}, err
	}

	lookupHash := channel.AccessHash
	if access.AccessHash != 0 {
		lookupHash = access.AccessHash
	}
	info, err := m.FindChannel(ctx, channel.TGID, lookupHash)
	if err != nil {
		m.setChannelReady(false)
		if errors.Is(err, ErrNotInChannel) {
			if deleteErr := m.db.DeleteChannelAccess(ctx, channel.ID, acct.ID); deleteErr != nil {
				m.log.Warn("could not clear stale channel access", zap.Error(deleteErr))
			}
			return drive.ChannelRef{}, fmt.Errorf(
				"%w: account %s has not been admitted to channel %q",
				ErrNotInChannel, acct.Label, channel.Title)
		}
		return drive.ChannelRef{}, err
	}
	m.markChannelChecked(time.Now())
	if err := m.db.UpsertChannelAccess(ctx, channel.ID, acct.ID, info.AccessHash, info.CanPost); err != nil {
		m.setChannelReady(false)
		return drive.ChannelRef{}, err
	}
	if acct.IsPrimary {
		// Keep the legacy/global channel coordinate aligned with the primary's
		// fresh view. Non-primary hashes must never be written there.
		if _, err := m.db.UpsertChannel(ctx, channel.TGID, info.AccessHash, channel.Title); err != nil {
			m.log.Warn("could not refresh the primary storage channel access hash", zap.Error(err))
		}
	}
	m.setChannelReady(info.CanPost)
	return drive.ChannelRef{ChannelID: channel.ID, TGID: channel.TGID, AccessHash: info.AccessHash}, nil
}

// refreshChannelReference forces a fresh lookup through this account's
// session. It is used only after Telegram rejects a channel reference, so it
// repairs a rotated or account-mismatched access hash without adding an RPC to
// every normal transfer.
func (m *Manager) refreshChannelReference(ctx context.Context, reference drive.ChannelRef) (drive.ChannelRef, error) {
	info, err := m.FindChannel(ctx, reference.TGID, reference.AccessHash)
	if err != nil {
		m.setChannelReady(false)
		return drive.ChannelRef{}, err
	}
	m.markChannelChecked(time.Now())
	m.setChannelReady(info.CanPost)
	if reference.ChannelID != "" {
		if err := m.db.UpsertChannelAccess(
			ctx, reference.ChannelID, m.ID(), info.AccessHash, info.CanPost,
		); err != nil {
			return drive.ChannelRef{}, err
		}
		if m.Account().IsPrimary {
			if _, err := m.db.UpsertChannel(ctx, reference.TGID, info.AccessHash, info.Title); err != nil {
				m.log.Warn("could not refresh the primary storage channel access hash", zap.Error(err))
			}
		}
	}
	return drive.ChannelRef{
		ChannelID:  reference.ChannelID,
		TGID:       info.TGID,
		AccessHash: info.AccessHash,
	}, nil
}

// ErrNotInChannel marks an account that cannot reach the storage channel,
// which is a configuration problem rather than a transient failure: the
// account has to be invited and given posting rights before it can be used.
//
// It is raised both here, from the drive's own bookkeeping, and by FindChannel,
// which asks Telegram itself. The two agree on what it means: this account is
// not in that channel.
var ErrNotInChannel = errors.New("tgc: account is not a member of the storage channel")

// Upload streams one document into a channel.
//
// This is the reference implementation's upload path, step for step: a
// streaming uploader issuing configured-size saveBigFilePart calls, then a document
// message carrying the caption. The one addition is WithThreads, which keeps
// several parts in flight instead of sending them one at a time.
func (m *Manager) Upload(ctx context.Context, ch drive.ChannelRef, spec drive.UploadSpec) (drive.StoredDoc, error) {
	api, err := m.API(ctx)
	if err != nil {
		return drive.StoredDoc{}, err
	}

	settings := m.cfg.RuntimeSettings()
	up := uploader.NewUploader(api).
		WithPartSize(int(settings.UploadPartSize)).
		WithThreads(settings.UploadThreads)
	if spec.Progress != nil {
		up = up.WithProgress(progressAdapter(spec.Progress))
	}

	input, err := up.Upload(ctx, uploader.NewUpload(spec.Name, spec.Body, spec.Size))
	if err != nil {
		return drive.StoredDoc{}, err
	}

	// No video attributes are set. Telegram's supports_streaming needs real
	// duration and dimensions, which would mean probing the container, and a
	// middle segment of a split video is not a valid container at all. tdrive
	// serves its own range reads, so nothing depends on it.
	doc, err := SendDocument(ctx, api, InputPeer(ch.TGID, ch.AccessHash), input,
		spec.Name, spec.MIME, spec.Caption, nil)
	if err != nil && isChannelUnreachable(err) {
		if fresh, refreshErr := m.refreshChannelReference(ctx, ch); refreshErr == nil && fresh.AccessHash != 0 {
			doc, err = SendDocument(ctx, api, InputPeer(fresh.TGID, fresh.AccessHash), input,
				spec.Name, spec.MIME, spec.Caption, nil)
		}
	}
	if err != nil {
		return drive.StoredDoc{}, err
	}
	return toStored(doc), nil
}

func (m *Manager) SendRecord(ctx context.Context, ch drive.ChannelRef, caption string) (int, error) {
	api, err := m.API(ctx)
	if err != nil {
		return 0, err
	}
	messageID, err := SendText(ctx, api, InputPeer(ch.TGID, ch.AccessHash), caption)
	if err != nil && isChannelUnreachable(err) {
		if fresh, refreshErr := m.refreshChannelReference(ctx, ch); refreshErr == nil {
			messageID, err = SendText(ctx, api, InputPeer(fresh.TGID, fresh.AccessHash), caption)
		}
	}
	return messageID, err
}

func (m *Manager) EditRecord(ctx context.Context, ch drive.ChannelRef, msgID int, caption string) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	err = EditCaption(ctx, api, InputChannel(ch.TGID, ch.AccessHash), msgID, caption)
	if err != nil && isChannelUnreachable(err) {
		if fresh, refreshErr := m.refreshChannelReference(ctx, ch); refreshErr == nil {
			err = EditCaption(ctx, api, InputChannel(fresh.TGID, fresh.AccessHash), msgID, caption)
		}
	}
	return err
}

func (m *Manager) DeleteRecords(ctx context.Context, ch drive.ChannelRef, msgIDs []int) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	err = DeleteMessages(ctx, api, InputChannel(ch.TGID, ch.AccessHash), msgIDs)
	if err != nil && isChannelUnreachable(err) {
		if fresh, refreshErr := m.refreshChannelReference(ctx, ch); refreshErr == nil {
			err = DeleteMessages(ctx, api, InputChannel(fresh.TGID, fresh.AccessHash), msgIDs)
		}
	}
	return err
}

func (m *Manager) ReadChunk(
	ctx context.Context,
	_ drive.ChannelRef,
	doc drive.StoredDoc,
	offset, limit int64,
) ([]byte, error) {
	api, err := m.APIForDC(ctx, doc.DCID)
	if err != nil {
		return nil, err
	}
	return GetChunk(ctx, api, &tg.InputDocumentFileLocation{
		ID:            doc.DocID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}, offset, limit)
}

func (m *Manager) RefreshDoc(ctx context.Context, ch drive.ChannelRef, msgID int) (drive.StoredDoc, error) {
	api, err := m.API(ctx)
	if err != nil {
		return drive.StoredDoc{}, err
	}
	doc, err := FetchDocument(ctx, api, InputChannel(ch.TGID, ch.AccessHash), msgID)
	if err != nil && isChannelUnreachable(err) {
		if fresh, refreshErr := m.refreshChannelReference(ctx, ch); refreshErr == nil {
			doc, err = FetchDocument(ctx, api, InputChannel(fresh.TGID, fresh.AccessHash), msgID)
		}
	}
	if err != nil {
		return drive.StoredDoc{}, err
	}
	return toStored(doc), nil
}

func (m *Manager) IsReferenceExpired(err error) bool { return IsFileReferenceExpired(err) }

func (m *Manager) MigratedDC(err error) (int, bool) { return MigrateDC(err) }

func toStored(d StoredDocument) drive.StoredDoc {
	return drive.StoredDoc{
		MsgID:         d.MsgID,
		DocID:         d.DocID,
		AccessHash:    d.AccessHash,
		DCID:          d.DCID,
		FileReference: d.FileReference,
		Size:          d.Size,
	}
}

// progressAdapter bridges the drive's callback to gotd's interface.
// ProgressState.Uploaded is the confirmed byte total, so a retried part is not
// counted twice.
type progressAdapter func(uploaded, total int64)

func (p progressAdapter) Chunk(_ context.Context, state uploader.ProgressState) error {
	p(state.Uploaded, state.Total)
	return nil
}
