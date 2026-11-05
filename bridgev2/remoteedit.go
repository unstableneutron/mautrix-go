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
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Values for bridge.remote_edits.
const (
	RemoteEditsReplace          = "replace"
	RemoteEditsNewMessageOthers = "new_message_others"
)

// RemoteEditReactionKey is the reaction the bridge bot puts on a message
// version that a remote edit superseded.
const RemoteEditReactionKey = "✏️"

// remoteEditedKey flags the bot's ✏️ reaction so it is never bridged.
const remoteEditedKey = "fi.mau.bridge.remote_edited"

// keepEditVersion reports whether a remote edit of part is sent as a new
// message instead of replacing it, keeping the previous version readable:
// bridge.remote_edits is new_message_others and the message isn't the user's.
func (portal *Portal) keepEditVersion(ctx context.Context, source *UserLogin, senderID networkid.UserID, part *database.Message) bool {
	if portal.Bridge.Config.RemoteEdits != RemoteEditsNewMessageOthers || part.HasFakeMXID() {
		return false
	}
	if source != nil && (part.SenderMXID == source.UserMXID || (senderID != "" && source.Client.IsThisUser(ctx, senderID))) {
		return false
	}
	return true
}

// sendRemoteEditReaction puts the bot's ✏️ reaction on a superseded version.
// Its transaction ID is derived from that version, so a retried edit doesn't
// add another.
func (portal *Portal) sendRemoteEditReaction(ctx context.Context, superseded id.EventID, ts time.Time) error {
	bot := portal.Bridge.Bot
	if err := bot.EnsureJoined(ctx, portal.MXID); err != nil {
		return fmt.Errorf("failed to ensure bot is in the room for the edited reaction: %w", err)
	}
	reaction := &event.Content{
		Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{
			Type: event.RelAnnotation, EventID: superseded, Key: RemoteEditReactionKey,
		}},
		Raw: map[string]any{remoteEditedKey: true},
	}
	_, err := bot.SendMessage(ctx, portal.MXID, event.EventReaction, reaction, &MatrixSendExtra{
		Timestamp: ts, TransactionID: remoteEditTxnID("reaction", superseded),
	})
	if err != nil {
		return fmt.Errorf("failed to send edited reaction: %w", err)
	}
	return nil
}

// remoteEditTxnID is the transaction ID of a send that supersedes a version,
// the same on every retry of the same edit.
func remoteEditTxnID(kind string, superseded id.EventID) string {
	sum := sha256.Sum256([]byte(superseded))
	return "mau-remote-edit-" + kind + "-" + hex.EncodeToString(sum[:16])
}
