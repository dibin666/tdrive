package tgc

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// A tdrive drive lives in one Telegram channel. A private broadcast channel is
// the right container: it is separate from the account's own chats, its message
// ids are dense and monotonic, its history is effectively unbounded, and
// Telegram's own search can find a caption's hashtags inside it, which is what
// makes the index rebuildable.

// ChannelInfo describes a channel the account could store files in.
type ChannelInfo struct {
	TGID       int64  `json:"tgId"`
	AccessHash int64  `json:"-"`
	Title      string `json:"title"`
	Username   string `json:"username,omitempty"`
	// CanPost is false for channels the account only reads. Those are shown
	// greyed out rather than hidden, so it is obvious why one is missing.
	CanPost bool `json:"canPost"`
	// Participants is shown in the picker to help tell channels apart.
	Participants int `json:"participants,omitempty"`
}

// CreateChannel makes a new private broadcast channel to hold a drive.
func (m *Manager) CreateChannel(ctx context.Context, title, about string) (ChannelInfo, error) {
	api, err := m.API(ctx)
	if err != nil {
		return ChannelInfo{}, err
	}
	if title == "" {
		title = "tdrive"
	}

	updates, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Broadcast: true,
		Title:     title,
		About:     about,
	})
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("create channel: %w", friendly(err))
	}

	chats, ok := extractChats(updates)
	if !ok {
		return ChannelInfo{}, errors.New("telegram did not report the new channel")
	}
	for _, c := range chats {
		if ch, ok := c.(*tg.Channel); ok {
			hash, _ := ch.GetAccessHash()
			return ChannelInfo{
				TGID:       ch.ID,
				AccessHash: hash,
				Title:      ch.Title,
				Username:   ch.Username,
				CanPost:    true,
			}, nil
		}
	}
	return ChannelInfo{}, errors.New("telegram returned no channel in the create response")
}

// ListChannels enumerates the account's channels so the settings page can offer
// an existing one instead of creating a new drive.
func (m *Manager) ListChannels(ctx context.Context) ([]ChannelInfo, error) {
	api, err := m.API(ctx)
	if err != nil {
		return nil, err
	}

	const page = 100
	var (
		out        []ChannelInfo
		seen       = map[int64]bool{}
		offsetDate int
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)

	// Telegram pages dialogs from the most recent backwards, so an account
	// with many chats needs several round trips before its older channels
	// appear.
	for range 20 {
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			Limit:      page,
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
		})
		if err != nil {
			return nil, fmt.Errorf("list dialogs: %w", friendly(err))
		}

		var (
			chats    []tg.ChatClass
			dialogs  []tg.DialogClass
			messages []tg.MessageClass
			more     bool
		)
		switch d := res.(type) {
		case *tg.MessagesDialogs:
			chats, dialogs, messages = d.Chats, d.Dialogs, d.Messages
		case *tg.MessagesDialogsSlice:
			chats, dialogs, messages = d.Chats, d.Dialogs, d.Messages
			more = len(d.Dialogs) == page
		default:
			return nil, fmt.Errorf("unexpected dialogs response %T", res)
		}

		for _, c := range chats {
			ch, ok := c.(*tg.Channel)
			if !ok || ch.Megagroup || seen[ch.ID] {
				continue
			}
			hash, hasHash := ch.GetAccessHash()
			if !hasHash {
				continue
			}
			seen[ch.ID] = true
			out = append(out, ChannelInfo{
				TGID:       ch.ID,
				AccessHash: hash,
				Title:      ch.Title,
				Username:   ch.Username,
				// Only a creator or an admin with post rights can store
				// files; anything else would fail on the first upload.
				CanPost: ch.Creator || ch.AdminRights.PostMessages,
			})
		}

		if !more || len(dialogs) == 0 {
			break
		}
		offsetDate, offsetID, offsetPeer = nextDialogOffset(dialogs, messages, chats)
		if offsetPeer == nil {
			break
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].CanPost != out[j].CanPost {
			return out[i].CanPost
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// nextDialogOffset derives the paging cursor from the last dialog in a page.
func nextDialogOffset(dialogs []tg.DialogClass, messages []tg.MessageClass, chats []tg.ChatClass) (int, int, tg.InputPeerClass) {
	last, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
	if !ok {
		return 0, 0, nil
	}

	date := 0
	for _, msg := range messages {
		if m, ok := msg.(*tg.Message); ok && m.ID == last.TopMessage {
			date = m.Date
			break
		}
	}

	peer := inputPeerFor(last.Peer, chats)
	return date, last.TopMessage, peer
}

func inputPeerFor(peer tg.PeerClass, chats []tg.ChatClass) tg.InputPeerClass {
	ch, ok := peer.(*tg.PeerChannel)
	if !ok {
		// Users and basic groups are never storage targets; skipping them
		// only affects paging precision, which the loop bound tolerates.
		return &tg.InputPeerEmpty{}
	}
	for _, c := range chats {
		if chat, ok := c.(*tg.Channel); ok && chat.ID == ch.ChannelID {
			hash, _ := chat.GetAccessHash()
			return &tg.InputPeerChannel{ChannelID: chat.ID, AccessHash: hash}
		}
	}
	return &tg.InputPeerEmpty{}
}

// ResolveChannel re-reads a channel to confirm it is reachable and to pick up a
// rotated access hash. A stale access hash makes every later download fail with
// CHANNEL_INVALID, so this runs whenever a channel is selected.
func (m *Manager) ResolveChannel(ctx context.Context, tgID, accessHash int64) (ChannelInfo, error) {
	api, err := m.API(ctx)
	if err != nil {
		return ChannelInfo{}, err
	}

	res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: tgID, AccessHash: accessHash},
	})
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("resolve channel %d: %w", tgID, friendly(err))
	}

	for _, c := range res.GetChats() {
		ch, ok := c.(*tg.Channel)
		if !ok || ch.ID != tgID {
			continue
		}
		hash, _ := ch.GetAccessHash()
		return ChannelInfo{
			TGID:       ch.ID,
			AccessHash: hash,
			Title:      ch.Title,
			Username:   ch.Username,
			CanPost:    ch.Creator || ch.AdminRights.PostMessages,
		}, nil
	}
	return ChannelInfo{}, fmt.Errorf("%w: channel %d is not reachable from this account", ErrNotInChannel, tgID)
}

// FindChannel returns this account's own view of a channel: first through the
// access hash it was given, and failing that by looking the channel up in the
// account's own channel list.
//
// The fallback is what makes membership detectable at all. An access hash is
// minted for one account, so the one stored on the channel row — whichever
// account resolved it last — means nothing to a second account, and Telegram
// rejects it with CHANNEL_INVALID. That failure says nothing about whether this
// account is in the channel, and an account that joined by hand in a Telegram
// client is found here without any invite being exported.
func (m *Manager) FindChannel(ctx context.Context, tgID, accessHash int64) (ChannelInfo, error) {
	if !m.Ready() {
		return ChannelInfo{}, ErrNotReady
	}

	if accessHash != 0 {
		info, err := m.ResolveChannel(ctx, tgID, accessHash)
		if err == nil {
			return info, nil
		}
		if !isChannelUnreachable(err) {
			return ChannelInfo{}, err
		}
	}

	channels, err := m.ListChannels(ctx)
	if err != nil {
		return ChannelInfo{}, err
	}
	for _, info := range channels {
		if info.TGID == tgID {
			return info, nil
		}
	}
	return ChannelInfo{}, fmt.Errorf("%w (channel %d)", ErrNotInChannel, tgID)
}

// isChannelUnreachable reports whether an error only means "this account cannot
// use that channel reference", which is worth retrying through the dialog list
// rather than reporting as a failure.
func isChannelUnreachable(err error) bool {
	return errors.Is(err, ErrNotInChannel) ||
		tgerr.Is(err, "CHANNEL_INVALID", "CHANNEL_PRIVATE", "PEER_ID_INVALID", "CHAT_ID_INVALID")
}

// InputChannel builds the peer reference used by every message call.
func InputChannel(tgID, accessHash int64) *tg.InputChannel {
	return &tg.InputChannel{ChannelID: tgID, AccessHash: accessHash}
}

// InputPeer builds the peer reference used when sending.
func InputPeer(tgID, accessHash int64) *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: tgID, AccessHash: accessHash}
}

func extractChats(u tg.UpdatesClass) ([]tg.ChatClass, bool) {
	type chatGetter interface{ GetChats() []tg.ChatClass }
	if g, ok := u.(chatGetter); ok {
		return g.GetChats(), true
	}
	return nil, false
}
