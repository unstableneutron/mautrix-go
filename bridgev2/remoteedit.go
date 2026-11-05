// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// editVersion is an earlier version of a message part that a remote edit
// superseded. Versions are kept in the kv_store rather than the message
// table, so connectors and lookups by event ID never see them.
type editVersion struct {
	PartID     networkid.PartID `json:"part_id"`
	MXID       id.EventID       `json:"mxid"`
	SenderMXID id.UserID        `json:"sender_mxid,omitempty"`
}

func (portal *Portal) editVersionsKey(msgID networkid.MessageID) database.Key {
	return database.Key("fi.mau.bridge.remote_edit_versions\x1f" + string(portal.Receiver) + "\x1f" + string(portal.ID) + "\x1f" + string(msgID))
}

func (portal *Portal) getEditVersions(ctx context.Context, msgID networkid.MessageID) ([]editVersion, error) {
	kv := portal.Bridge.DB.KV
	var raw string
	err := kv.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id = $1 AND key = $2`, kv.BridgeID, portal.editVersionsKey(msgID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get kept edit versions: %w", err)
	}
	var versions []editVersion
	if err = json.Unmarshal([]byte(raw), &versions); err != nil {
		return nil, fmt.Errorf("failed to parse kept edit versions: %w", err)
	}
	return versions, nil
}

func (portal *Portal) addEditVersions(ctx context.Context, msgID networkid.MessageID, added []editVersion) error {
	versions, err := portal.getEditVersions(ctx, msgID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(append(versions, added...))
	if err != nil {
		return err
	}
	if err = portal.setKV(ctx, portal.editVersionsKey(msgID), string(raw)); err != nil {
		return fmt.Errorf("failed to save kept edit versions: %w", err)
	}
	// Map each version's event back to the message, for replies and reactions.
	for _, version := range added {
		ref, err := json.Marshal(editVersionRef{Receiver: portal.Receiver, MessageID: msgID, PartID: version.PartID})
		if err != nil {
			return err
		}
		if err = portal.setKV(ctx, editVersionRefKey(version.MXID), string(ref)); err != nil {
			return fmt.Errorf("failed to save kept edit version reference: %w", err)
		}
	}
	return nil
}

// editVersionRef points a kept version's event ID back to its message.
type editVersionRef struct {
	Receiver  networkid.UserLoginID `json:"receiver"`
	MessageID networkid.MessageID   `json:"message_id"`
	PartID    networkid.PartID      `json:"part_id"`
}

func editVersionRefKey(mxid id.EventID) database.Key {
	return database.Key("fi.mau.bridge.remote_edit_version_of\x1f" + string(mxid))
}

func (portal *Portal) setKV(ctx context.Context, key database.Key, value string) error {
	kv := portal.Bridge.DB.KV
	_, err := kv.Exec(ctx, `
		INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (bridge_id, key) DO UPDATE SET value = $3
	`, kv.BridgeID, key, value)
	return err
}

// getPartByMXIDOrVersion is GetPartByMXID that also resolves an earlier
// version kept by remote_edits to the message's current part, so a Matrix
// reply or reaction to an old version reaches the remote message.
func (portal *Portal) getPartByMXIDOrVersion(ctx context.Context, mxid id.EventID) (*database.Message, error) {
	part, err := portal.Bridge.DB.Message.GetPartByMXID(ctx, mxid)
	if err != nil || part != nil {
		return part, err
	}
	kv := portal.Bridge.DB.KV
	var raw string
	err = kv.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id = $1 AND key = $2`, kv.BridgeID, editVersionRefKey(mxid)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get kept edit version reference: %w", err)
	}
	var ref editVersionRef
	if err = json.Unmarshal([]byte(raw), &ref); err != nil {
		return nil, fmt.Errorf("failed to parse kept edit version reference: %w", err)
	}
	parts, err := portal.Bridge.DB.Message.GetAllPartsByID(ctx, ref.Receiver, ref.MessageID)
	if err != nil || len(parts) == 0 {
		return nil, err
	}
	for _, candidate := range parts {
		if candidate.PartID == ref.PartID {
			return candidate, nil
		}
	}
	// The part was removed by the edit; the message's first part stands in.
	return parts[0], nil
}

func (portal *Portal) deleteEditVersions(ctx context.Context, msgID networkid.MessageID) error {
	versions, err := portal.getEditVersions(ctx, msgID)
	if err != nil {
		return err
	}
	kv := portal.Bridge.DB.KV
	keys := []database.Key{portal.editVersionsKey(msgID)}
	for _, version := range versions {
		keys = append(keys, editVersionRefKey(version.MXID))
	}
	for _, key := range keys {
		if _, err = kv.Exec(ctx, `DELETE FROM kv_store WHERE bridge_id = $1 AND key = $2`, kv.BridgeID, key); err != nil {
			return fmt.Errorf("failed to forget kept edit versions: %w", err)
		}
	}
	return nil
}

// withEditVersions returns a message's parts followed by its kept earlier
// versions as row-less parts (RowID 0), so a delete reaches every version.
func (portal *Portal) withEditVersions(ctx context.Context, parts []*database.Message) ([]*database.Message, error) {
	if len(parts) == 0 {
		return parts, nil
	}
	versions, err := portal.getEditVersions(ctx, parts[0].ID)
	if err != nil || len(versions) == 0 {
		return parts, err
	}
	all := append(make([]*database.Message, 0, len(parts)+len(versions)), parts...)
	for _, version := range versions {
		all = append(all, &database.Message{
			BridgeID: parts[0].BridgeID, ID: parts[0].ID, PartID: version.PartID, MXID: version.MXID,
			Room: parts[0].Room, SenderMXID: version.SenderMXID, Timestamp: parts[0].Timestamp,
		})
	}
	return all, nil
}
