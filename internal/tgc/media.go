package tgc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// StoredDocument is everything needed to read a segment back later. It is
// exactly what goes into the segments table.
type StoredDocument struct {
	MsgID         int
	DocID         int64
	AccessHash    int64
	FileReference []byte
	DCID          int
	Size          int64
}

// VideoInfo carries the attributes that make Telegram treat an upload as a
// seekable video. The reference implementation sets supports_streaming for
// video uploads, and doing the same here is what lets Telegram's own clients
// scrub a segment without downloading it whole.
type VideoInfo struct {
	Duration float64
	Width    int
	Height   int
}

// SendDocument posts an already-uploaded file to a channel and returns its
// stored coordinates.
//
// Uploads are silent: a drive that pushes a notification for every segment of
// every file would be unusable as a chat.
func SendDocument(
	ctx context.Context,
	api *tg.Client,
	peer tg.InputPeerClass,
	file tg.InputFileClass,
	filename, mime, caption string,
	video *VideoInfo,
) (StoredDocument, error) {
	attrs := []tg.DocumentAttributeClass{
		&tg.DocumentAttributeFilename{FileName: filename},
	}
	if video != nil {
		attrs = append(attrs, &tg.DocumentAttributeVideo{
			SupportsStreaming: true,
			Duration:          video.Duration,
			W:                 video.Width,
			H:                 video.Height,
		})
	}

	randomID, err := randInt64()
	if err != nil {
		return StoredDocument{}, err
	}

	updates, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Silent: true,
		Peer:   peer,
		Media: &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   mime,
			Attributes: attrs,
			// ForceFile keeps Telegram from re-encoding or reinterpreting a
			// segment. A middle segment of a split archive is not a valid
			// file of any type, and letting Telegram guess would corrupt it.
			ForceFile: video == nil,
		},
		Message:  caption,
		RandomID: randomID,
	})
	if err != nil {
		return StoredDocument{}, fmt.Errorf("send document %q: %w", filename, friendly(err))
	}

	msg, doc, err := documentFromUpdates(updates)
	if err != nil {
		return StoredDocument{}, err
	}
	return StoredDocument{
		MsgID:         msg,
		DocID:         doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
		DCID:          doc.DCID,
		Size:          doc.Size,
	}, nil
}

// SendText posts a plain message, used for the directory records whose captions
// carry the folder tree.
func SendText(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, text string) (int, error) {
	randomID, err := randInt64()
	if err != nil {
		return 0, err
	}
	updates, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Silent:    true,
		NoWebpage: true,
		Peer:      peer,
		Message:   text,
		RandomID:  randomID,
	})
	if err != nil {
		return 0, fmt.Errorf("send message: %w", friendly(err))
	}
	return messageIDFromUpdates(updates)
}

// EditCaption rewrites a record's caption. Renames and moves go through here,
// because the caption is the durable copy of a file's name and location.
func EditCaption(ctx context.Context, api *tg.Client, channel *tg.InputChannel, msgID int, caption string) error {
	_, err := api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer:    &tg.InputPeerChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		ID:      msgID,
		Message: caption,
	})
	if err != nil {
		// Editing to identical text is not a failure worth surfacing; it
		// happens when a move leaves the caption unchanged.
		if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
			return nil
		}
		return fmt.Errorf("edit caption of message %d: %w", msgID, friendly(err))
	}
	return nil
}

// DeleteMessages removes records from a channel. Telegram caps a delete at 100
// ids per call, which a large directory easily exceeds.
func DeleteMessages(ctx context.Context, api *tg.Client, channel *tg.InputChannel, ids []int) error {
	const batch = 100
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if _, err := api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: channel,
			ID:      ids[start:end],
		}); err != nil {
			return fmt.Errorf("delete messages: %w", friendly(err))
		}
	}
	return nil
}

// FetchDocument re-reads a message to obtain a fresh file reference. Telegram
// expires file references after about an hour, so this runs whenever a read
// fails with FILE_REFERENCE_EXPIRED rather than on a timer.
func FetchDocument(ctx context.Context, api *tg.Client, channel *tg.InputChannel, msgID int) (StoredDocument, error) {
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return StoredDocument{}, fmt.Errorf("fetch message %d: %w", msgID, friendly(err))
	}

	msgs, ok := res.(interface{ GetMessages() []tg.MessageClass })
	if !ok {
		return StoredDocument{}, fmt.Errorf("unexpected messages response %T", res)
	}
	for _, mc := range msgs.GetMessages() {
		m, ok := mc.(*tg.Message)
		if !ok || m.ID != msgID {
			continue
		}
		doc, err := documentOf(m)
		if err != nil {
			return StoredDocument{}, err
		}
		return StoredDocument{
			MsgID:         m.ID,
			DocID:         doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
			DCID:          doc.DCID,
			Size:          doc.Size,
		}, nil
	}
	return StoredDocument{}, fmt.Errorf("message %d is gone from the channel", msgID)
}

// Location builds the file location for a stored segment.
func (d StoredDocument) Location() *tg.InputDocumentFileLocation {
	return &tg.InputDocumentFileLocation{
		ID:            d.DocID,
		AccessHash:    d.AccessHash,
		FileReference: d.FileReference,
		ThumbSize:     "",
	}
}

// GetChunk reads one range of a document.
//
// Precise makes Telegram honour byte-exact offsets and limits instead of only
// 1 KiB-aligned ones, which the parallel reader needs so a range request can
// start anywhere. The limit still may not exceed 1 MiB and the read may not
// cross a 1 MiB boundary; callers align to a power-of-two chunk size to satisfy
// both.
func GetChunk(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, offset, limit int64) ([]byte, error) {
	res, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Precise:  true,
		Location: loc,
		Offset:   offset,
		Limit:    int(limit),
	})
	if err != nil {
		return nil, err
	}
	switch f := res.(type) {
	case *tg.UploadFile:
		return f.Bytes, nil
	case *tg.UploadFileCDNRedirect:
		// CDN redirects require a separate decryption path. Telegram only
		// issues them for large public files, which a private storage
		// channel is not, so treating this as unsupported is safe and keeps
		// the read path simple.
		return nil, errors.New("telegram returned a CDN redirect, which tdrive does not use")
	default:
		return nil, fmt.Errorf("unexpected getFile response %T", res)
	}
}

// ChunkSizeFor picks the read granularity for a range.
//
// 1 MiB is Telegram's per-call maximum and the right choice for streaming a
// whole file. For a short range — a media player probing a container header,
// or a WebDAV client reading a few bytes — a smaller power of two avoids
// pulling a megabyte to satisfy a kilobyte. Staying on powers of two keeps
// every read inside one 1 MiB window, which upload.getFile requires.
func ChunkSizeFor(start, end int64) int64 {
	size := int64(1024 * 1024)
	for size > 1024 && size > (end-start) {
		size /= 2
	}
	return size
}

// IsFileReferenceExpired reports the one error the reader retries by
// re-resolving a document rather than failing the request.
func IsFileReferenceExpired(err error) bool {
	return tgerr.Is(err, "FILE_REFERENCE_EXPIRED") ||
		tgerr.Is(err, "FILE_REFERENCE_INVALID") ||
		tgerr.Is(err, "FILE_REFERENCE_EMPTY")
}

// MigrateDC reports the datacenter a document actually lives on when Telegram
// answers a read with FILE_MIGRATE_x.
func MigrateDC(err error) (int, bool) {
	if e, ok := tgerr.As(err); ok && e.Type == "FILE_MIGRATE" {
		return e.Argument, true
	}
	return 0, false
}

func documentFromUpdates(u tg.UpdatesClass) (int, *tg.Document, error) {
	upd, ok := u.(*tg.Updates)
	if !ok {
		return 0, nil, fmt.Errorf("unexpected send response %T", u)
	}
	for _, update := range upd.Updates {
		var msg tg.MessageClass
		switch v := update.(type) {
		case *tg.UpdateNewChannelMessage:
			msg = v.Message
		case *tg.UpdateNewMessage:
			msg = v.Message
		default:
			continue
		}
		m, ok := msg.(*tg.Message)
		if !ok {
			continue
		}
		doc, err := documentOf(m)
		if err != nil {
			return 0, nil, err
		}
		return m.ID, doc, nil
	}
	return 0, nil, errors.New("telegram did not report the stored document")
}

func messageIDFromUpdates(u tg.UpdatesClass) (int, error) {
	switch v := u.(type) {
	case *tg.Updates:
		for _, update := range v.Updates {
			switch m := update.(type) {
			case *tg.UpdateNewChannelMessage:
				if msg, ok := m.Message.(*tg.Message); ok {
					return msg.ID, nil
				}
			case *tg.UpdateNewMessage:
				if msg, ok := m.Message.(*tg.Message); ok {
					return msg.ID, nil
				}
			case *tg.UpdateMessageID:
				return m.ID, nil
			}
		}
	case *tg.UpdateShortSentMessage:
		return v.ID, nil
	}
	return 0, errors.New("telegram did not report the new message id")
}

func documentOf(m *tg.Message) (*tg.Document, error) {
	media, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, fmt.Errorf("message %d holds no document", m.ID)
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return nil, fmt.Errorf("message %d holds an inaccessible document", m.ID)
	}
	return doc, nil
}

func randInt64() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("generate random id: %w", err)
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}
