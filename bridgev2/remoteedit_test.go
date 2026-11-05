// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func testLogin(t *testing.T, portal *Portal) *UserLogin {
	t.Helper()
	login, err := portal.Bridge.GetExistingUserLoginByID(context.Background(), "login")
	if err != nil || login == nil {
		t.Fatalf("login = %v, %v", login, err)
	}
	return login
}

func editOf(part *database.Message, body string) *ConvertedEdit {
	return &ConvertedEdit{ModifiedParts: []*ConvertedEditPart{{
		Part: part, Type: event.EventMessage,
		Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
	}}}
}

func partByID(t *testing.T, portal *Portal, msgID networkid.MessageID, partID networkid.PartID) *database.Message {
	t.Helper()
	for _, part := range loadParts(t, portal, msgID) {
		if part.PartID == partID {
			return part
		}
	}
	t.Fatalf("part %q not found", partID)
	return nil
}

// Someone else's edit becomes a new message replying to the version it
// supersedes, which the bot marks with one ✏️; the row follows the newest
// version so the next edit chains from it.
func TestRemoteEditOthersKeepsVersions(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	login, bot, ghost := testLogin(t, portal), testBot(portal), &testIntent{}

	prev := id.EventID("$part1")
	for i, body := range []string{"second", "third"} {
		res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), body), ghost, time.Now(), 0)
		if !res.Success {
			t.Fatalf("edit %d = %+v", i, res)
		}
		if len(bot.contents) != i+1 || bot.sentTypes[i] != "reaction" {
			t.Fatalf("bot sent %v, want one reaction per edit", bot.sentTypes)
		}
		rel := bot.contents[i].Parsed.(*event.ReactionEventContent).RelatesTo
		if rel.EventID != prev || rel.Key != RemoteEditReactionKey || bot.contents[i].Raw[remoteEditedKey] != true {
			t.Fatalf("edit %d reaction = %+v %v, want a flagged ✏️ on %s", i, rel, bot.contents[i].Raw, prev)
		}
		sent := ghost.contents[i].Parsed.(*event.MessageEventContent)
		if sent.NewContent != nil || sent.RelatesTo.GetReplaceID() != "" || sent.RelatesTo.GetReplyTo() != prev || sent.Body != body {
			t.Fatalf("edit %d sent %+v, want a new message %q replying to %s", i, sent, body, prev)
		}
		if ghost.sent[i].TransactionID != remoteEditTxnID("version", prev) {
			t.Fatalf("edit %d transaction ID %q isn't derived from the superseded version", i, ghost.sent[i].TransactionID)
		}
		row := partByID(t, portal, msgID, "1")
		if row.MXID == prev || row.MXID != id.EventID("$sent"+ghost.sent[i].TransactionID) || row.EditCount != 0 {
			t.Fatalf("edit %d row = %+v, want it on the new version", i, row)
		}
		prev = row.MXID
	}
	if partByID(t, portal, msgID, "0").MXID != "$part0" {
		t.Fatal("an unedited part moved")
	}
}

// The user's own edits, and everyone's with remote_edits: replace, still
// edit in place without any marker.
func TestRemoteEditReplaces(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		own        bool
	}{
		{"own message", RemoteEditsNewMessageOthers, true},
		{"replace mode", RemoteEditsReplace, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := zerolog.Nop().WithContext(context.Background())
			portal, msgID := newTestPortal(t, RemoteDeletesMark)
			portal.Bridge.Config.RemoteEdits = tc.mode
			part := partByID(t, portal, msgID, "1")
			if tc.own {
				part.SenderMXID = testUserMXID
			}
			bot, ghost := testBot(portal), &testIntent{}
			if res := portal.sendConvertedEdit(ctx, testLogin(t, portal), msgID, "alice", editOf(part, "new"), ghost, time.Now(), 0); !res.Success {
				t.Fatalf("result = %+v", res)
			}
			if len(bot.sent) != 0 {
				t.Fatalf("bot sent %v on an in-place edit", bot.sentTypes)
			}
			sent := ghost.contents[0].Parsed.(*event.MessageEventContent)
			if sent.RelatesTo.GetReplaceID() != "$part1" || partByID(t, portal, msgID, "1").MXID != "$part1" {
				t.Fatalf("sent %+v, want an m.replace of $part1 with the row unchanged", sent.RelatesTo)
			}
		})
	}
}

// A failed ✏️ stops the edit before the new version is sent; the retry
// reuses the same transaction IDs.
func TestRemoteEditRetry(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	login, bot, ghost := testLogin(t, portal), testBot(portal), &testIntent{}
	bot.fail = map[string]bool{"reaction": true}
	if res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), "new"), ghost, time.Now(), 0); res.Success {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if len(ghost.sent) != 0 || partByID(t, portal, msgID, "1").MXID != "$part1" {
		t.Fatal("the new version went out or the row moved although the reaction failed")
	}
	bot.fail = nil
	if res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), "new"), ghost, time.Now(), 0); !res.Success {
		t.Fatalf("retry = %+v", res)
	}
	if len(bot.sent) != 1 || bot.sent[0].TransactionID != remoteEditTxnID("reaction", "$part1") || len(ghost.sent) != 1 {
		t.Fatalf("retry sent bot %v ghost %v", bot.sentTypes, ghost.sentTypes)
	}
}

// editedIntent has a newer edit of each readable event, which connectors
// that don't count edits leave at EditCount 0.
type editedIntent struct {
	testIntent
	latest map[id.EventID]string
}

func (ei *editedIntent) GetLatestEdit(_ context.Context, _ id.RoomID, eventID id.EventID) (*event.Event, error) {
	body, ok := ei.latest[eventID]
	if !ok {
		return nil, nil
	}
	return &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType: event.MsgText, Body: "* " + body,
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
	}}}, nil
}

// A kept delete marks the newest version even when the row's edit count is
// 0, and the marking edit comes from the part's own sender rather than the
// deleter, because clients ignore edits by anyone else.
func TestMarkUsesNewestEditFromSender(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	const aliceMXID id.UserID = "@ghost_alice:example.com"
	aliceEdits := &editedIntent{latest: map[id.EventID]string{"$part0": "edited text"}}
	aliceEdits.testIntent = testIntent{mxid: aliceMXID, readable: map[id.EventID]*event.MessageEventContent{
		"$part0": {MsgType: event.MsgText, Body: "original text"},
		"$part1": {MsgType: event.MsgText, Body: "other part"},
	}}
	ghost, err := portal.Bridge.GetGhostByID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ghost.Intent = aliceEdits
	parts := loadParts(t, portal, msgID)
	for _, part := range parts {
		part.SenderMXID = aliceMXID
	}
	deleter := &testIntent{}
	if res := portal.markMessageParts(ctx, parts, deleter, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if len(deleter.sent) != 0 {
		t.Fatalf("deleter sent %v; edits must come from the part's sender", deleter.sentTypes)
	}
	if len(aliceEdits.contents) != 2 {
		t.Fatalf("sender sent %v, want two marking edits", aliceEdits.sentTypes)
	}
	marked := aliceEdits.contents[0].Parsed.(*event.MessageEventContent).NewContent
	if !strings.Contains(marked.Body, "edited text") || strings.Contains(marked.Body, "original text") {
		t.Fatalf("marked body %q, want the newest edit's text", marked.Body)
	}
}

// A part that someone else's edit removes stays on Matrix as an earlier
// version instead of being redacted; only its row is forgotten.
func TestRemoteEditKeepsRemovedPart(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot, ghost := testBot(portal), &testIntent{}
	edit := editOf(partByID(t, portal, msgID, "1"), "caption only")
	edit.DeletedParts = []*database.Message{partByID(t, portal, msgID, "0")}
	if res := portal.sendConvertedEdit(ctx, testLogin(t, portal), msgID, "alice", edit, ghost, time.Now(), 0); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	for _, content := range ghost.contents {
		if _, ok := content.Parsed.(*event.RedactionEventContent); ok {
			t.Fatal("the removed part was redacted")
		}
	}
	if len(bot.contents) != 1 || bot.contents[0].Parsed.(*event.ReactionEventContent).RelatesTo.EventID != "$part0" {
		t.Fatalf("bot sent %v, want one ✏️ on the lowest superseded part $part0", bot.sentTypes)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 1 || parts[0].PartID != "1" {
		t.Fatalf("rows = %+v, want only part 1", parts)
	}
}

// Two edits by someone else keep two earlier versions of part 1. When they
// delete the message, every version is kept and gets its own 🗑️; with
// remote_deletes: redact every version is redacted. Either way the kept
// version list is forgotten with the message.
func TestRemoteDeleteCoversEditVersions(t *testing.T) {
	for _, mode := range []string{RemoteDeletesMark, RemoteDeletesRedact} {
		t.Run(mode, func(t *testing.T) {
			ctx := zerolog.Nop().WithContext(context.Background())
			portal, msgID := newTestPortal(t, mode)
			login, bot := testLogin(t, portal), testBot(portal)
			alice := portal.Bridge.Matrix.GhostIntent("alice").(*testIntent)
			for _, body := range []string{"v2", "v3"} {
				if res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), body), alice, time.Now(), 0); !res.Success {
					t.Fatalf("edit = %+v", res)
				}
			}
			newest := partByID(t, portal, msgID, "1").MXID
			v2 := alice.sent[0].TransactionID
			want := map[id.EventID]bool{"$part0": true, newest: true, "$part1": true, id.EventID("$sent" + v2): true}
			bot.sent, bot.sentTypes, bot.contents = nil, nil, nil
			alice.sent, alice.sentTypes, alice.contents = nil, nil, nil

			if res := portal.handleRemoteMessageRemove(ctx, login, &simpleRemoteRemove{sender: EventSender{Sender: "alice"}}); !res.Success {
				t.Fatalf("delete = %+v", res)
			}
			got := map[id.EventID]bool{}
			if mode == RemoteDeletesMark {
				for i, kind := range bot.sentTypes {
					if kind == "reaction" {
						got[bot.contents[i].Parsed.(*event.ReactionEventContent).RelatesTo.EventID] = true
					}
				}
				for _, content := range alice.contents {
					if _, ok := content.Parsed.(*event.RedactionEventContent); ok {
						t.Fatal("a version was redacted under mark")
					}
				}
			} else {
				for _, content := range alice.contents {
					if redaction, ok := content.Parsed.(*event.RedactionEventContent); ok {
						got[redaction.Redacts] = true
					}
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%s reached %v, want every version %v", mode, got, want)
			}
			for mxid := range want {
				if !got[mxid] {
					t.Fatalf("%s missed %s; reached %v", mode, mxid, got)
				}
			}
			if versions, err := portal.getEditVersions(ctx, msgID); err != nil || len(versions) != 0 {
				t.Fatalf("kept versions after the delete = %v, %v", versions, err)
			}
			if part, err := portal.getPartByMXIDOrVersion(ctx, "$part1"); err != nil || part != nil {
				t.Fatalf("old version still resolves after the delete: %+v, %v", part, err)
			}
		})
	}
}

// Under mark, the user's own delete stays a real deletion: the message and
// every kept edit version are redacted, nothing is marked, and the kept
// version list and its references are forgotten.
func TestRemoteDeleteOwnMessageRedactsVersions(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	login := testLogin(t, portal)
	alice := portal.Bridge.Matrix.GhostIntent("alice").(*testIntent)
	for _, body := range []string{"v2", "v3"} {
		if res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), body), alice, time.Now(), 0); !res.Success {
			t.Fatalf("edit = %+v", res)
		}
	}
	v2 := id.EventID("$sent" + alice.sent[0].TransactionID)
	want := map[id.EventID]bool{"$part0": true, "$part1": true, v2: true, partByID(t, portal, msgID, "1").MXID: true}
	intents := func() []*testIntent {
		all := []*testIntent{testBot(portal)}
		for _, ghost := range portal.Bridge.Matrix.(testMatrix).ghosts {
			all = append(all, ghost)
		}
		return all
	}
	for _, intent := range intents() {
		intent.sent, intent.sentTypes, intent.contents = nil, nil, nil
	}

	if res := portal.handleRemoteMessageRemove(ctx, login, &simpleRemoteRemove{sender: EventSender{IsFromMe: true, Sender: "me"}}); !res.Success {
		t.Fatalf("delete = %+v", res)
	}
	got := map[id.EventID]bool{}
	for _, intent := range intents() {
		for i, content := range intent.contents {
			if redaction, ok := content.Parsed.(*event.RedactionEventContent); ok {
				got[redaction.Redacts] = true
			} else {
				t.Fatalf("%s sent a %s on an own delete", intent.mxid, intent.sentTypes[i])
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("redacted %v, want every version %v", got, want)
	}
	for mxid := range want {
		if !got[mxid] {
			t.Fatalf("%s wasn't redacted; redacted %v", mxid, got)
		}
	}
	if versions, err := portal.getEditVersions(ctx, msgID); err != nil || len(versions) != 0 {
		t.Fatalf("kept versions after the delete = %v, %v", versions, err)
	}
	for _, mxid := range []id.EventID{"$part1", v2} {
		if part, err := portal.getPartByMXIDOrVersion(ctx, mxid); err != nil || part != nil {
			t.Fatalf("%s still resolves after the delete: %+v, %v", mxid, part, err)
		}
	}
}

// A Matrix reply or reaction to an earlier version reaches the remote
// message: the version resolves to the message's current part. (A live
// pilot reply to the original went out without its reply relation.)
func TestEditVersionResolvesForRepliesAndReactions(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	login := testLogin(t, portal)
	if res := portal.sendConvertedEdit(ctx, login, msgID, "alice", editOf(partByID(t, portal, msgID, "1"), "v2"), &testIntent{}, time.Now(), 0); !res.Success {
		t.Fatalf("edit = %+v", res)
	}
	current := partByID(t, portal, msgID, "1")
	got, err := portal.getPartByMXIDOrVersion(ctx, "$part1")
	if err != nil || got == nil || got.ID != msgID || got.PartID != "1" || got.MXID != current.MXID {
		t.Fatalf("old version resolved to %+v, %v; want the current part 1 (%s)", got, err, current.MXID)
	}
	if got, _ = portal.getPartByMXIDOrVersion(ctx, "$unknown"); got != nil {
		t.Fatalf("unknown event resolved to %+v", got)
	}
	client := portal.Bridge.Network.(testNetwork).client
	res := portal.Bridge.QueueMatrixEvent(ctx, &event.Event{
		ID: "$thumbs", RoomID: portal.MXID, Sender: testUserMXID, Type: event.EventReaction, Timestamp: 3000,
		Content: event.Content{Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{
			Type: event.RelAnnotation, EventID: "$part1", Key: "👍",
		}}},
	})
	if !res.Success || res.Ignored || len(client.reactions) != 1 {
		t.Fatalf("reaction on the old version = %+v, network got %v", res, client.reactions)
	}
	res = portal.Bridge.QueueMatrixEvent(ctx, &event.Event{
		ID: "$reply", RoomID: portal.MXID, Sender: testUserMXID, Type: event.EventMessage, Timestamp: 3001,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText, Body: "You see this?",
			RelatesTo: (&event.RelatesTo{}).SetReplyTo("$part1"),
		}},
	})
	if !res.Success || len(client.replies) != 1 || client.replies[0] != msgID {
		t.Fatalf("reply to the old version = %+v, network got reply targets %v; want %q", res, client.replies, msgID)
	}
}
