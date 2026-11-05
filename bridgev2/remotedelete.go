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
// Some networks (Telegram, Meta) don't say who deleted a message; then a
// delete of a message the user sent counts as theirs, so it stays a real
// delete, even if a moderator did it.
func (portal *Portal) shouldMarkRemoteDelete(evt RemoteMessageRemove, source *UserLogin, parts []*database.Message) bool {
	switch portal.Bridge.Config.RemoteDeletes {
	case RemoteDeletesMarkAll:
		return true
	case RemoteDeletesMark:
		sender := evt.GetSender()
		if sender.IsFromMe {
			return false
		}
		if sender.Sender == "" && sender.SenderLogin == "" && source != nil {
			for _, part := range parts {
				if part.SenderMXID != "" && part.SenderMXID == source.UserMXID {
					return false
				}
			}
		}
		return true
	}
	return false
}

// markMessageParts edits every part of a message deleted on the remote
// network to its last content plus a visible marker. A part whose content
// can't be read back, such as when the bridge ratchets its keys on decrypt,
// is left untouched and the message gets a marker reply instead, after which
// the message's rows are deleted so a repeated delete is a no-op.
//
// Any send or database error fails the result and skips the reply and the row
// deletion, so the rows stay for a retry. Every send uses a transaction ID
// derived from the message part, so a retry can't duplicate an edit or reply,
// and parts already marked are skipped.
func (portal *Portal) markMessageParts(ctx context.Context, parts []*database.Message, intent MatrixAPI, ts time.Time) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	// Networks often stamp a delete with the message's own time, so the marker carries none.
	marker := "🗑 Deleted in " + portal.Bridge.Network.GetName().DisplayName
	var errorList []error
	var unreadable *database.Message
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		content, err := portal.latestPartContent(ctx, intent, part, marker)
		if err != nil {
			log.Warn().Err(err).Stringer("part_mxid", part.MXID).Msg("Can't read deleted message part back, leaving it untouched")
			if unreadable == nil {
				unreadable = part
			}
			continue
		} else if content == nil {
			continue
		}
		wrapped := &event.Content{Parsed: content, Raw: map[string]any{remoteDeletedKey: true}}
		_, err = intent.SendMessage(ctx, portal.MXID, event.EventMessage, wrapped, &MatrixSendExtra{
			Timestamp: ts, MessageMeta: part, TransactionID: remoteDeleteTxnID("edit", part),
		})
		if err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to mark message part deleted")
			errorList = append(errorList, fmt.Errorf("failed to mark part %q deleted: %w", part.PartID, err))
			continue
		}
		part.EditCount++
		if err = portal.Bridge.DB.Message.Update(ctx, part); err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to save marked message part")
			errorList = append(errorList, fmt.Errorf("failed to save marked part %q: %w", part.PartID, err))
		}
	}
	if unreadable != nil && len(errorList) == 0 {
		notice := &event.MessageEventContent{
			MsgType:   event.MsgNotice,
			Body:      marker + ". Kept here as it was.",
			Mentions:  &event.Mentions{},
			RelatesTo: (&event.RelatesTo{}).SetReplyTo(unreadable.MXID),
		}
		_, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{Parsed: notice}, &MatrixSendExtra{
			Timestamp: ts, TransactionID: remoteDeleteTxnID("notice", unreadable),
		})
		if err != nil {
			errorList = append(errorList, fmt.Errorf("failed to send deleted marker: %w", err))
		} else if err = portal.Bridge.DB.Message.DeleteAllParts(ctx, portal.Receiver, unreadable.ID); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to forget kept message: %w", err))
		}
	}
	if len(errorList) > 0 {
		return EventHandlingResultFailed.WithError(errors.Join(errorList...))
	}
	return EventHandlingResultSuccess
}

// remoteDeleteTxnID is the Matrix transaction ID of a kept-delete send for a
// message part, the same on every retry.
func remoteDeleteTxnID(kind string, part *database.Message) string {
	sum := sha256.Sum256([]byte(string(part.Room.ID) + "\x00" + string(part.Room.Receiver) + "\x00" + string(part.ID) + "\x00" + string(part.PartID)))
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
	if part.EditCount > 0 {
		lookup, ok := intent.(MatrixAPIWithEditLookup)
		if !ok {
			return nil, errors.New("can't look up the newest edit")
		}
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
