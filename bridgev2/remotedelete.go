// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
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
// is left untouched and the message gets a marker reply instead.
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
		if _, err = intent.SendMessage(ctx, portal.MXID, event.EventMessage, wrapped, &MatrixSendExtra{Timestamp: ts, MessageMeta: part}); err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to mark message part deleted")
			errorList = append(errorList, err)
			continue
		}
		part.EditCount++
		if err = portal.Bridge.DB.Message.Update(ctx, part); err != nil {
			log.Err(err).Stringer("part_mxid", part.MXID).Msg("Failed to save marked message part")
		}
	}
	if unreadable != nil {
		notice := &event.MessageEventContent{
			MsgType:   event.MsgNotice,
			Body:      marker + ". Kept here as it was.",
			Mentions:  &event.Mentions{},
			RelatesTo: (&event.RelatesTo{}).SetReplyTo(unreadable.MXID),
		}
		if _, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{Parsed: notice}, &MatrixSendExtra{Timestamp: ts}); err != nil {
			errorList = append(errorList, err)
		}
	}
	if len(errorList) > 0 {
		return EventHandlingResultFailed.WithError(errors.Join(errorList...))
	}
	return EventHandlingResultSuccess
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
