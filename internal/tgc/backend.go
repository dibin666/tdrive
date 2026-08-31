package tgc

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// Manager implements drive.Account. The adapter lives here rather than in the
// drive package so that nothing above this layer has to import gotd types.
var _ drive.Account = (*Manager)(nil)

// ChannelRef resolves this account's own coordinates for a storage channel.
//
// The per-account row is the authoritative one, because Telegram mints an
// access hash for the requesting account. The channels table is consulted only
// for the primary account, whose column predates per-account rows and is what a
// database written before this feature has.
func (m *Manager) ChannelRef(ctx context.Context, channelID string) (drive.ChannelRef, error) {
	acct := m.Account()

	access, err := m.db.ChannelAccessFor(ctx, channelID, acct.ID)
	switch {
	case err == nil:
		channel, err := m.db.ChannelByID(ctx, channelID)
		if err != nil {
			return drive.ChannelRef{}, err
		}
		return drive.ChannelRef{TGID: channel.TGID, AccessHash: access.AccessHash}, nil
	case !errors.Is(err, database.ErrNotFound):
		return drive.ChannelRef{}, err
	}

	channel, err := m.db.ChannelByID(ctx, channelID)
	if err != nil {
		return drive.ChannelRef{}, err
	}
	if !acct.IsPrimary {
		return drive.ChannelRef{}, fmt.Errorf(
			"%w: account %s has not been admitted to channel %q",
			ErrNotInChannel, acct.Label, channel.Title)
	}
	return drive.ChannelRef{TGID: channel.TGID, AccessHash: channel.AccessHash}, nil
}

// ErrNotInChannel marks an account that cannot reach the storage channel,
// which is a configuration problem rather than a transient failure: the
// account has to be invited and given posting rights before it can be used.
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
	return SendText(ctx, api, InputPeer(ch.TGID, ch.AccessHash), caption)
}

func (m *Manager) EditRecord(ctx context.Context, ch drive.ChannelRef, msgID int, caption string) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	return EditCaption(ctx, api, InputChannel(ch.TGID, ch.AccessHash), msgID, caption)
}

func (m *Manager) DeleteRecords(ctx context.Context, ch drive.ChannelRef, msgIDs []int) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	return DeleteMessages(ctx, api, InputChannel(ch.TGID, ch.AccessHash), msgIDs)
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
