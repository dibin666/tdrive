package tgc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
)

// Admitting a second account to the storage channel.
//
// An account that is not in the channel is useless: it can neither post nor
// read. Doing it by hand means finding the new account in a Telegram client,
// inviting it and promoting it, which is fiddly enough to get wrong — and
// getting it wrong produces an account that looks configured and fails on its
// first upload. So tdrive does it: the primary exports an invite link, the
// joining account uses it, and the primary promotes it.
//
// That automatic path needs a primary that can still export an invite for the
// channel, which is not always true — the primary may have been migrated to
// another Telegram account, or may not be the channel's creator. So it is only
// ever the second thing tried. The first is to ask the account itself whether
// it is already in the channel, which covers every account somebody added by
// hand in a Telegram client, and LinkChannel below covers the rest by letting
// the channel be picked from that account's own channel list.
//
// The rights granted are post, edit and delete. Post is the obvious one. Edit
// and delete matter because renaming or deleting a file rewrites the captions
// of messages some other account wrote, and Telegram only allows that with the
// corresponding admin right.

// ErrCannotPromote means the primary account is not allowed to grant admin
// rights in this channel, which happens when it is not the creator. There is no
// way around it from here: someone with the right has to do it in a Telegram
// client.
var ErrCannotPromote = errors.New("tgc: this account cannot grant admin rights in the storage channel")

// ErrPrimaryNotInChannel means the primary account cannot see the storage
// channel at all, so there is nobody left to invite anyone into it.
var ErrPrimaryNotInChannel = errors.New("tgc: the primary account cannot reach the storage channel")

// ErrNoPostRights means an account is in the storage channel but may not post
// to it, and cannot be promoted from here — the case of the primary itself,
// which no other account has the standing to promote.
var ErrNoPostRights = errors.New("tgc: this account is in the storage channel but cannot post to it")

// ErrWrongChannel rejects a hand-picked channel that is not the one this drive
// stores into. Recording it would produce an account that posts into the wrong
// place, which is worse than not admitting it at all.
var ErrWrongChannel = errors.New("tgc: that is not this drive's storage channel")

// JoinChannel admits joiner to the channel and gives it the rights the drive
// needs, recording the result so the scheduler knows the account is usable.
//
// It is safe to re-run: an account that is already a member and already an
// admin ends up in the same state, with its access hash refreshed.
func (c *Cluster) JoinChannel(ctx context.Context, joiner *Manager, channel database.Channel) error {
	c.channelStateMu.Lock()
	defer c.channelStateMu.Unlock()
	return c.joinChannel(ctx, joiner, channel)
}

func (c *Cluster) joinChannel(ctx context.Context, joiner *Manager, channel database.Channel) error {
	// The default channel may have changed since the last check. Do not leave
	// the account schedulable for the old channel while this one is being
	// resolved.
	joiner.resetChannelCheck()
	if !joiner.Ready() {
		return fmt.Errorf("%w: sign this account in before adding it to the channel", ErrNotReady)
	}

	// Ask the account itself first. It may already be in the channel — tdrive
	// admitted it earlier, or somebody added it by hand in a Telegram client —
	// and then there is nothing to invite: recording the access hash its own
	// session sees is the entire job.
	membership, err := joiner.FindChannel(ctx, channel.TGID, channel.AccessHash)
	switch {
	case err == nil:
		return c.recordMembership(ctx, joiner, channel, membership)
	case errors.Is(err, ErrNotInChannel):
		// Not a member yet, so the primary has to invite it below.
	default:
		return err
	}

	primary, err := c.Primary()
	if err != nil {
		return err
	}
	if joiner.ID() == primary.ID() {
		return fmt.Errorf("%w: %q", ErrPrimaryNotInChannel, channel.Title)
	}

	// The invite has to be exported with the primary's own access hash, not
	// whatever is on the channel row, which may have been minted by an account
	// this deployment no longer runs.
	asPrimarySeesIt, err := c.channelAsPrimarySeesIt(ctx, channel)
	if err != nil {
		return err
	}
	link, err := primary.exportInvite(ctx, asPrimarySeesIt)
	if err != nil {
		return err
	}
	accessHash, err := joiner.joinByInvite(ctx, link, asPrimarySeesIt)
	if err != nil {
		return err
	}
	// A freshly joined member is never an admin, so recordMembership promotes
	// it before calling the account usable. It is handed the channel as the
	// primary sees it so the promotion does not have to work out a usable
	// access hash all over again.
	return c.recordMembership(ctx, joiner, asPrimarySeesIt, ChannelInfo{
		TGID:       channel.TGID,
		AccessHash: accessHash,
		Title:      channel.Title,
	})
}

// LinkChannel records an account's membership of a channel that was picked by
// hand out of that account's own channel list.
//
// It is the way out of every situation the invite dance cannot handle: a
// primary that may not export invites, an account already added manually, or a
// channel whose stored access hash belongs to a Telegram account this
// deployment has since replaced. The pick is checked against the drive's own
// channel, because an account posting into some other channel would write files
// nothing can read back.
func (c *Cluster) LinkChannel(ctx context.Context, member *Manager, channel database.Channel, tgID int64) error {
	c.channelStateMu.Lock()
	defer c.channelStateMu.Unlock()
	member.resetChannelCheck()
	if !member.Ready() {
		return fmt.Errorf("%w: sign this account in before adding it to the channel", ErrNotReady)
	}
	if tgID != channel.TGID {
		return fmt.Errorf("%w: this drive stores into %q (channel %d)", ErrWrongChannel, channel.Title, channel.TGID)
	}

	membership, err := member.FindChannel(ctx, channel.TGID, channel.AccessHash)
	if err != nil {
		return err
	}
	return c.recordMembership(ctx, member, channel, membership)
}

// recordMembership stores what one account can do with the storage channel,
// having the primary grant posting rights first when the account is only a
// reader. Every path that admits an account ends here, so an account is
// reported as usable in exactly one place.
func (c *Cluster) recordMembership(
	ctx context.Context,
	member *Manager,
	channel database.Channel,
	membership ChannelInfo,
) error {
	primary, err := c.Primary()
	if err != nil {
		return err
	}
	isPrimary := member.ID() == primary.ID()
	if isPrimary {
		// The channel row carries the primary's access hash, and this is the
		// freshest one there is.
		c.rememberChannelDetails(ctx, channel, membership.AccessHash, membership.Title)
	}
	if membership.CanPost {
		if err := c.db.UpsertChannelAccess(ctx, channel.ID, member.ID(), membership.AccessHash, true); err != nil {
			member.setChannelReady(false)
			return err
		}
		member.setChannelReady(true)
		return nil
	}

	// A member that cannot post is recorded either way: the accounts list
	// distinguishes "not a member" from "a member that cannot store", and the
	// scheduler leaves it alone until the rights are there.
	if err := c.db.UpsertChannelAccess(ctx, channel.ID, member.ID(), membership.AccessHash, false); err != nil {
		member.setChannelReady(false)
		c.log.Warn("could not record a channel membership", zap.Error(err))
	}
	member.setChannelReady(false)
	if isPrimary {
		return fmt.Errorf("%w: %q", ErrNoPostRights, channel.Title)
	}

	self := member.Self()
	if self == nil {
		return fmt.Errorf("%w: the joining account is not signed in", ErrNotReady)
	}
	asPrimarySeesIt, err := c.channelAsPrimarySeesIt(ctx, channel)
	if err != nil {
		return err
	}
	if err := primary.promote(ctx, asPrimarySeesIt, self.GetID()); err != nil {
		return err
	}
	if err := c.db.UpsertChannelAccess(ctx, channel.ID, member.ID(), membership.AccessHash, true); err != nil {
		member.setChannelReady(false)
		return err
	}
	member.setChannelReady(true)
	return nil
}

// channelAsPrimarySeesIt returns the storage channel carrying an access hash
// the primary account can actually use, refreshing the stored one when it has
// gone stale.
//
// The hash on the channel row belongs to whichever account resolved it last.
// Migrating the primary to another Telegram account, or Telegram rotating the
// hash, leaves a value every later call rejects with CHANNEL_INVALID — which is
// how a perfectly healthy channel starts looking broken.
func (c *Cluster) channelAsPrimarySeesIt(ctx context.Context, channel database.Channel) (database.Channel, error) {
	primary, err := c.Primary()
	if err != nil {
		return channel, err
	}
	view, err := primary.FindChannel(ctx, channel.TGID, channel.AccessHash)
	if err != nil {
		if errors.Is(err, ErrNotInChannel) {
			return channel, fmt.Errorf("%w: %q", ErrPrimaryNotInChannel, channel.Title)
		}
		return channel, err
	}
	c.rememberChannelDetails(ctx, channel, view.AccessHash, view.Title)
	channel.AccessHash = view.AccessHash
	return channel, nil
}

// rememberChannelDetails keeps the legacy/global row aligned with the primary
// account's current view. The title matters as well as the access hash because
// Telegram channel names can be changed outside tdrive.
func (c *Cluster) rememberChannelDetails(
	ctx context.Context,
	channel database.Channel,
	accessHash int64,
	title string,
) {
	if accessHash == 0 {
		if title == channel.Title {
			return
		}
		accessHash = channel.AccessHash
	}
	if accessHash == channel.AccessHash && title == channel.Title {
		return
	}
	if title == "" {
		title = channel.Title
	}
	if _, err := c.db.UpsertChannel(ctx, channel.TGID, accessHash, title); err != nil {
		c.log.Warn("could not refresh the storage channel's access hash", zap.Error(err))
	}
}

// JoinAll admits every non-primary account to the channel, which is what
// selecting or creating a storage channel has to do once more than one account
// exists. Individual failures are returned rather than aborting: one account
// that cannot join must not stop the others.
func (c *Cluster) JoinAll(ctx context.Context, channel database.Channel) map[string]error {
	c.channelStateMu.Lock()
	defer c.channelStateMu.Unlock()

	failures := make(map[string]error)
	for _, manager := range c.All() {
		manager.resetChannelCheck()
		if err := c.joinChannel(ctx, manager, channel); err != nil {
			failures[manager.ID()] = err
			c.log.Warn("could not admit a telegram account to the storage channel",
				zap.String("account", manager.ID()), zap.Error(err))
		}
	}
	return failures
}

// exportInvite produces a reusable link to the channel.
func (m *Manager) exportInvite(ctx context.Context, channel database.Channel) (string, error) {
	api, err := m.API(ctx)
	if err != nil {
		return "", err
	}
	res, err := api.MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{
		Peer:  InputPeer(channel.TGID, channel.AccessHash),
		Title: "tdrive",
	})
	if err != nil {
		return "", fmt.Errorf("export a channel invite: %w", friendly(err))
	}
	exported, ok := res.(*tg.ChatInviteExported)
	if !ok {
		return "", fmt.Errorf("telegram returned an unusable invite (%T)", res)
	}
	if exported.Link == "" {
		return "", errors.New("telegram returned an empty invite link")
	}
	return exported.Link, nil
}

// joinByInvite uses a link and returns this account's own access hash for the
// channel, which is the value every later call from this account must carry.
func (m *Manager) joinByInvite(ctx context.Context, link string, channel database.Channel) (int64, error) {
	api, err := m.API(ctx)
	if err != nil {
		return 0, err
	}
	hash := inviteHash(link)
	if hash == "" {
		return 0, fmt.Errorf("could not read an invite hash out of %q", link)
	}

	res, err := api.MessagesImportChatInvite(ctx, hash)
	switch {
	case err == nil:
		if ok, isOk := res.(*tg.MessagesChatInviteJoinResultOk); isOk {
			if accessHash, found := channelAccessFrom(ok.Updates, channel.TGID); found {
				return accessHash, nil
			}
		}
	case tgerr.Is(err, "USER_ALREADY_PARTICIPANT"):
		// Re-running the admission of an account that is already in. Its access
		// hash comes from its own dialog list below.
	default:
		return 0, fmt.Errorf("join the storage channel: %w", friendly(err))
	}

	// Either the join response carried no chat, or the account was already a
	// member. Either way its own view of the channel is in its dialogs.
	channels, err := m.ListChannels(ctx)
	if err != nil {
		return 0, fmt.Errorf("look up the joined channel: %w", err)
	}
	for _, info := range channels {
		if info.TGID == channel.TGID {
			return info.AccessHash, nil
		}
	}
	return 0, fmt.Errorf("this account joined %q but cannot see it", channel.Title)
}

// promote grants the joining account the rights the drive needs. It is run by
// the primary, which must itself be allowed to add admins — true when it
// created the channel, not necessarily otherwise.
func (m *Manager) promote(ctx context.Context, channel database.Channel, userID int64) error {
	api, err := m.API(ctx)
	if err != nil {
		return err
	}
	user, err := m.participant(ctx, channel, userID)
	if err != nil {
		return err
	}

	_, err = api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
		Channel: InputChannel(channel.TGID, channel.AccessHash),
		UserID:  user,
		AdminRights: tg.ChatAdminRights{
			// Post to store files. Edit and delete because a rename rewrites,
			// and a delete removes, messages this account did not write.
			PostMessages:   true,
			EditMessages:   true,
			DeleteMessages: true,
		},
		Rank: "tdrive",
	})
	switch {
	case err == nil:
		return nil
	case tgerr.Is(err, "CHAT_ADMIN_REQUIRED", "RIGHT_FORBIDDEN", "ADMIN_RANK_EMOJI_NOT_ALLOWED", "USER_CREATOR"):
		return fmt.Errorf("%w: %v", ErrCannotPromote, friendly(err))
	default:
		return fmt.Errorf("grant posting rights: %w", friendly(err))
	}
}

// participant finds a channel member by user id, which is how the primary gets
// an InputUser it can actually use: an access hash minted for itself.
func (m *Manager) participant(ctx context.Context, channel database.Channel, userID int64) (tg.InputUserClass, error) {
	api, err := m.API(ctx)
	if err != nil {
		return nil, err
	}

	const page = 200
	for offset := 0; offset < 10*page; offset += page {
		res, err := api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
			Channel: InputChannel(channel.TGID, channel.AccessHash),
			Filter:  &tg.ChannelParticipantsRecent{},
			Offset:  offset,
			Limit:   page,
		})
		if err != nil {
			return nil, fmt.Errorf("list channel members: %w", friendly(err))
		}
		participants, ok := res.(*tg.ChannelsChannelParticipants)
		if !ok {
			return nil, fmt.Errorf("unexpected members response %T", res)
		}
		for _, u := range participants.Users {
			user, ok := u.(*tg.User)
			if !ok || user.ID != userID {
				continue
			}
			hash, ok := user.GetAccessHash()
			if !ok {
				return nil, fmt.Errorf("telegram returned member %d without an access hash", userID)
			}
			return &tg.InputUser{UserID: user.ID, AccessHash: hash}, nil
		}
		if len(participants.Users) < page {
			break
		}
	}
	return nil, fmt.Errorf("account %d did not appear in the channel's member list", userID)
}

// channelAccessFrom digs this account's access hash for a channel out of the
// updates Telegram returns from a join.
func channelAccessFrom(updates tg.UpdatesClass, tgID int64) (int64, bool) {
	chats, ok := extractChats(updates)
	if !ok {
		return 0, false
	}
	for _, c := range chats {
		channel, ok := c.(*tg.Channel)
		if !ok || channel.ID != tgID {
			continue
		}
		if hash, ok := channel.GetAccessHash(); ok {
			return hash, true
		}
	}
	return 0, false
}

// inviteHash pulls the opaque part out of a t.me invite link. Telegram hands
// back either the "+hash" or the older "joinchat/hash" form depending on the
// channel, and importChatInvite wants the hash alone.
func inviteHash(link string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(link), "/")
	slash := strings.LastIndex(trimmed, "/")
	if slash >= 0 {
		trimmed = trimmed[slash+1:]
	}
	return strings.TrimPrefix(trimmed, "+")
}
