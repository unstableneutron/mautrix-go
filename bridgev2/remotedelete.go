// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Values for bridge.remote_deletes.
const (
	RemoteDeletesRedact  = "redact"
	RemoteDeletesMark    = "mark"
	RemoteDeletesMarkAll = "mark_all"
)

// remoteDeletedKey flags content the bridge already marked as deleted, so a
// repeated delete doesn't stack markers.
const remoteDeletedKey = "fi.mau.bridge.remote_deleted"

// RemoteEventWithCompletion is a remote event that wants the final result of
// handling it. HandleCompleted is called exactly once when the portal's handler
// returns or panics (as a failure wrapping ErrPanicInEventHandler), with the
// handler's own result, before the event queue drops or ignores errors and
// even if handling went on in the background past the queue's timeout. It is
// not called for events that never reach a portal's handler; QueueRemoteEvent's
// return value covers those.
type RemoteEventWithCompletion interface {
	RemoteEvent
	HandleCompleted(ctx context.Context, res EventHandlingResult)
}

// MatrixAPIWithEditLookup can read the newest edit of an event.
type MatrixAPIWithEditLookup interface {
	MatrixAPI
	// GetLatestEdit returns the newest m.replace edit of the event sent by
	// this user, decrypted, or nil when there is none.
	GetLatestEdit(ctx context.Context, roomID id.RoomID, eventID id.EventID) (*event.Event, error)
}

// shouldMarkRemoteDelete reports whether bridge.remote_deletes keeps a
// message deleted on the remote network instead of redacting it. Deletes the
// network reports as the user's own stay real deletes unless set to mark_all.
func (portal *Portal) shouldMarkRemoteDelete(evt RemoteMessageRemove) bool {
	switch portal.Bridge.Config.RemoteDeletes {
	case RemoteDeletesMarkAll:
		return true
	case RemoteDeletesMark:
		return !evt.GetSender().IsFromMe
	}
	return false
}

// markMessageParts edits every part of a message deleted on the remote
// network to its last content plus a visible marker. A part whose content
// can't be read back, such as when the bridge ratchets its keys on decrypt,
// is left untouched. The bridge bot then marks the message with a 🗑️ reaction
// (bridge.remote_delete_reaction) and a marker reply
// (bridge.remote_delete_notice), each optional; after an unreadable part the
// message's rows are deleted so a repeated delete is a no-op.
//
// Any send or database error fails the result and skips the steps after it,
// including the row deletion, so the rows stay for a retry. Every send uses a
// transaction ID derived from the message part, so a retry can't duplicate an
// edit, reaction or reply, and parts already marked are skipped.
func (portal *Portal) markMessageParts(ctx context.Context, parts []*database.Message, intent MatrixAPI, ts time.Time) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	// Networks often stamp a delete with the message's own time, so the marker carries none.
	marker := "🗑 Deleted in " + portal.Bridge.Network.GetName().DisplayName
	var errorList []error
	var unreadable *database.Message
	edited := false
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		// The edit only applies when it comes from the part's own sender.
		partIntent := intent
		if senderIntent, err := portal.getIntentForMXID(ctx, part.SenderMXID); err != nil {
			log.Warn().Err(err).Stringer("sender_mxid", part.SenderMXID).Msg("Failed to get intent of deleted part's sender")
		} else if senderIntent != nil {
			partIntent = senderIntent
		}
		content, err := portal.latestPartContent(ctx, partIntent, part, marker)
		if err != nil {
			log.Warn().Err(err).Stringer("part_mxid", part.MXID).Msg("Can't read deleted message part back, leaving it untouched")
			if unreadable == nil {
				unreadable = part
			}
			continue
		} else if content == nil {
			// Marked by an earlier attempt, which may have failed before its markers.
			edited = true
			continue
		}
		wrapped := &event.Content{Parsed: content, Raw: map[string]any{remoteDeletedKey: true}}
		_, err = partIntent.SendMessage(ctx, portal.MXID, event.EventMessage, wrapped, &MatrixSendExtra{
			Timestamp: ts, MessageMeta: part, TransactionID: remoteDeleteTxnID("edit", part),
		})
		if err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to mark message part deleted")
			errorList = append(errorList, fmt.Errorf("failed to mark part %q deleted: %w", part.PartID, err))
			continue
		}
		edited = true
		if part.RowID == 0 {
			continue // a kept edit version has no row
		}
		part.EditCount++
		if err = portal.Bridge.DB.Message.Update(ctx, part); err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to save marked message part")
			errorList = append(errorList, fmt.Errorf("failed to save marked part %q: %w", part.PartID, err))
		}
	}
	if len(errorList) == 0 && (edited || unreadable != nil) {
		if err := portal.sendRemoteDeleteMarkers(ctx, parts, unreadable, marker, ts); err != nil {
			errorList = append(errorList, err)
		}
	}
	if unreadable != nil && len(errorList) == 0 {
		if err := portal.Bridge.DB.Message.DeleteAllParts(ctx, portal.Receiver, unreadable.ID); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to forget kept message: %w", err))
		} else if err = portal.deleteEditVersions(ctx, unreadable.ID); err != nil {
			errorList = append(errorList, err)
		}
	}
	if len(errorList) > 0 {
		return EventHandlingResultFailed.WithError(errors.Join(errorList...))
	}
	return EventHandlingResultSuccess
}

// sendRemoteDeleteMarkers sends the bot's 🗑️ reaction to the message's first
// part and, when a part couldn't be edited, its marker reply, as configured.
// They come from the bridge bot so they aren't attributed to whoever deleted
// the message, and carry remoteDeletedKey so they are never bridged back.
func (portal *Portal) sendRemoteDeleteMarkers(ctx context.Context, parts []*database.Message, unreadable *database.Message, marker string, ts time.Time) error {
	cfg := portal.Bridge.Config
	sendNotice := unreadable != nil && cfg.RemoteDeleteNotice
	first := firstRealPart(parts)
	sendReaction := cfg.RemoteDeleteReaction && first != nil
	if !sendNotice && !sendReaction {
		return nil
	}
	bot := portal.Bridge.Bot
	if err := bot.EnsureJoined(ctx, portal.MXID); err != nil {
		return fmt.Errorf("failed to ensure bot is in the room for deleted markers: %w", err)
	}
	if sendReaction {
		// One 🗑️ on the message, and one on every version of a part that
		// remote_edits split into separate messages: each kept earlier version
		// (no row) and the part's newest version (its row).
		versioned := make(map[networkid.PartID]bool)
		for _, part := range parts {
			if part.RowID == 0 {
				versioned[part.PartID] = true
			}
		}
		targets := []*database.Message{first}
		for _, part := range parts {
			if part != first && !part.HasFakeMXID() && versioned[part.PartID] {
				targets = append(targets, part)
			}
		}
		for _, target := range targets {
			reaction := &event.Content{
				Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{
					Type: event.RelAnnotation, EventID: target.MXID, Key: RemoteDeleteReactionKey,
				}},
				Raw: map[string]any{remoteDeletedKey: true},
			}
			_, err := bot.SendMessage(ctx, portal.MXID, event.EventReaction, reaction, &MatrixSendExtra{
				Timestamp: ts, TransactionID: remoteDeleteTxnID("reaction", target),
			})
			if err != nil {
				return fmt.Errorf("failed to send deleted reaction: %w", err)
			}
		}
	}
	if sendNotice {
		notice := &event.Content{
			Parsed: &event.MessageEventContent{
				MsgType:   event.MsgNotice,
				Body:      marker + ". Kept here as it was.",
				Mentions:  &event.Mentions{},
				RelatesTo: (&event.RelatesTo{}).SetReplyTo(unreadable.MXID),
			},
			Raw: map[string]any{remoteDeletedKey: true},
		}
		_, err := bot.SendMessage(ctx, portal.MXID, event.EventMessage, notice, &MatrixSendExtra{
			Timestamp: ts, TransactionID: remoteDeleteTxnID("notice", unreadable),
		})
		if err != nil {
			return fmt.Errorf("failed to send deleted marker: %w", err)
		}
	}
	return nil
}

// RemoteDeleteReactionKey is the reaction the bridge bot puts on a kept message.
const RemoteDeleteReactionKey = "🗑️"

// firstRealPart returns the part a message's single reaction goes on: the one
// with the lowest part ID, which connectors use for the message's main part,
// in its newest version (a part with a row) when there is one.
func firstRealPart(parts []*database.Message) *database.Message {
	var first *database.Message
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		hasRow, firstHasRow := part.RowID != 0, first != nil && first.RowID != 0
		if first == nil || (hasRow && !firstHasRow) || (hasRow == firstHasRow && part.PartID < first.PartID) {
			first = part
		}
	}
	return first
}

// isRemoteDeleteMarker reports whether a Matrix event is one of the markers
// the bridge sent for a kept remote delete or a kept edit version, which must
// never be bridged.
func isRemoteDeleteMarker(evt *event.Event) bool {
	_, deleted := evt.Content.Raw[remoteDeletedKey]
	_, edited := evt.Content.Raw[remoteEditedKey]
	return deleted || edited
}

// remoteDeleteTxnID is the Matrix transaction ID of a kept-delete send for a
// message part, the same on every retry.
func remoteDeleteTxnID(kind string, part *database.Message) string {
	sum := sha256.Sum256([]byte(string(part.Room.ID) + "\x00" + string(part.Room.Receiver) + "\x00" + string(part.ID) + "\x00" + string(part.PartID) + "\x00" + string(part.MXID)))
	return "mau-remote-delete-" + kind + "-" + hex.EncodeToString(sum[:16])
}

// latestPartContent returns the edit that marks a part deleted, built from
// its newest version, or nil when it is already marked or can't carry one.
func (portal *Portal) latestPartContent(ctx context.Context, intent MatrixAPI, part *database.Message, marker string) (*event.MessageEventContent, error) {
	evt, err := intent.GetEvent(ctx, portal.MXID, part.MXID)
	if err != nil {
		return nil, err
	}
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok || evt.Type != event.EventMessage {
		return nil, errors.New("not a message event")
	}
	// Not every connector counts remote edits, so look for one even when
	// EditCount is 0; only a counted edit requires the lookup.
	if lookup, ok := intent.(MatrixAPIWithEditLookup); !ok && part.EditCount > 0 {
		return nil, errors.New("can't look up the newest edit")
	} else if ok {
		edit, err := lookup.GetLatestEdit(ctx, portal.MXID, part.MXID)
		if err != nil {
			return nil, err
		} else if edit != nil {
			if _, done := edit.Content.Raw[remoteDeletedKey]; done {
				return nil, nil
			}
			editContent, ok := edit.Content.Parsed.(*event.MessageEventContent)
			if !ok || editContent.NewContent == nil {
				return nil, errors.New("newest edit has no new content")
			}
			content = editContent.NewContent
		}
	}
	return markDeleted(content, part.MXID, marker), nil
}

// markDeleted returns an edit of the original event that keeps its content
// (text, or the same media) and prefixes the text or caption with marker.
func markDeleted(content *event.MessageEventContent, original id.EventID, marker string) *event.MessageEventContent {
	out := *content
	out.RelatesTo = nil
	out.NewContent = nil
	out.Mentions = &event.Mentions{}
	if out.MsgType.IsMedia() {
		// A body that is just the file name isn't a caption; the marker becomes it.
		if out.FileName == "" {
			out.FileName = out.Body
		}
		if out.Body == out.FileName {
			out.Body = ""
		}
	}
	htmlBody := out.FormattedBody
	if out.Format != event.FormatHTML {
		htmlBody = strings.ReplaceAll(html.EscapeString(out.Body), "\n", "<br>")
	}
	out.Format = event.FormatHTML
	out.FormattedBody = "<p><em>" + html.EscapeString(marker) + "</em></p>" + htmlBody
	out.Body = strings.TrimSuffix(marker+"\n"+out.Body, "\n")
	out.SetEdit(original)
	return &out
}
