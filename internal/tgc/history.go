package tgc

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"

	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/indexer"
)

// Manager supplies channel history to the indexer.
var _ indexer.Source = (*Manager)(nil)

// historyPage is Telegram's maximum for messages.getHistory.
const historyPage = 100

// ScanHistory walks a channel from newest to oldest, handing every message to
// visit.
//
// Paging is by message id rather than by offset: Telegram's offset paging
// shifts when messages are added or deleted mid-scan, which on a channel that
// is actively receiving uploads would silently skip records. Walking ids
// downwards cannot skip.
func (m *Manager) ScanHistory(
	ctx context.Context,
	ch drive.ChannelRef,
	visit func(indexer.Message) error,
) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	peer := InputPeer(ch.TGID, ch.AccessHash)

	offsetID := 0
	for {
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			Limit:    historyPage,
			OffsetID: offsetID,
		})
		if err != nil {
			return fmt.Errorf("get history: %w", friendly(err))
		}

		msgs, ok := res.(interface{ GetMessages() []tg.MessageClass })
		if !ok {
			return fmt.Errorf("unexpected history response %T", res)
		}
		batch := msgs.GetMessages()
		if len(batch) == 0 {
			return nil
		}

		lowest := 0
		for _, mc := range batch {
			msg, ok := mc.(*tg.Message)
			if !ok {
				// Service messages ("channel created") carry no caption and
				// are not records, but they still advance the cursor.
				if base, ok := mc.AsNotEmpty(); ok {
					if id := base.GetID(); lowest == 0 || id < lowest {
						lowest = id
					}
				}
				continue
			}
			if lowest == 0 || msg.ID < lowest {
				lowest = msg.ID
			}

			out := indexer.Message{ID: msg.ID, Caption: msg.Message}
			if media, ok := msg.Media.(*tg.MessageMediaDocument); ok {
				if doc, ok := media.Document.(*tg.Document); ok {
					out.Doc = &drive.StoredDoc{
						MsgID:         msg.ID,
						DocID:         doc.ID,
						AccessHash:    doc.AccessHash,
						DCID:          doc.DCID,
						FileReference: doc.FileReference,
						Size:          doc.Size,
					}
				}
			}
			if err := visit(out); err != nil {
				return err
			}
		}

		if lowest == 0 || len(batch) < historyPage {
			return nil
		}
		offsetID = lowest

		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
