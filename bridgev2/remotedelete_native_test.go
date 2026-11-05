// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// testNetwork's logins all use client, which records the reactions the bridge
// forwards to the network.
type testNetwork struct {
	NetworkConnector
	client *testClient
}

func (testNetwork) Init(*Bridge)                       {}
func (testNetwork) GetName() BridgeName                { return BridgeName{DisplayName: "Test"} }
func (testNetwork) GetDBMetaTypes() database.MetaTypes { return database.MetaTypes{} }
func (testNetwork) GetCapabilities() *NetworkGeneralCapabilities {
	return &NetworkGeneralCapabilities{}
}
func (tn testNetwork) LoadUserLogin(_ context.Context, login *UserLogin) error {
	login.Client = tn.client
	return nil
}

type testClient struct {
	NetworkAPI
	reactions []string
}

func (*testClient) IsLoggedIn() bool { return true }

// GetUserInfo has nothing to add, so a remote event's ghost keeps the info the
// test gave it.
func (*testClient) GetUserInfo(context.Context, *Ghost) (*UserInfo, error) { return nil, nil }
func (*testClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return userID == "me"
}
func (*testClient) GetCapabilities(context.Context, *Portal) *event.RoomFeatures {
	return &event.RoomFeatures{}
}
func (*testClient) PreHandleMatrixReaction(_ context.Context, msg *MatrixReaction) (MatrixReactionPreResponse, error) {
	return MatrixReactionPreResponse{SenderID: "me", Emoji: msg.Content.RelatesTo.Key}, nil
}
func (tc *testClient) HandleMatrixReaction(_ context.Context, msg *MatrixReaction) (*database.Reaction, error) {
	tc.reactions = append(tc.reactions, msg.Content.RelatesTo.Key)
	return &database.Reaction{}, nil
}
func (*testClient) HandleMatrixReactionRemove(context.Context, *MatrixReactionRemove) error {
	return nil
}

type testMatrix struct {
	MatrixConnector
	bot    MatrixAPI
	ghosts map[networkid.UserID]*testIntent
}

func (tm testMatrix) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	localpart, ok := strings.CutPrefix(strings.TrimSuffix(string(userID), ":example.com"), "@ghost_")
	return networkid.UserID(localpart), ok
}

func (tm testMatrix) GhostIntent(userID networkid.UserID) MatrixAPI {
	if tm.ghosts[userID] == nil {
		tm.ghosts[userID] = &testIntent{mxid: id.UserID("@ghost_" + string(userID) + ":example.com")}
	}
	return tm.ghosts[userID]
}

func (testMatrix) Init(*Bridge)                                                               {}
func (tm testMatrix) BotIntent() MatrixAPI                                                    { return tm.bot }
func (testMatrix) SendMessageStatus(context.Context, *MessageStatus, *MessageStatusEventInfo) {}

// testIntent reads back events from a fixed set and records sends; sends of
// the kinds in fail ("edit", "notice", "reaction") fail.
type testIntent struct {
	MatrixAPI
	mxid      id.UserID
	readable  map[id.EventID]*event.MessageEventContent
	fail      map[string]bool
	joins     int
	sent      []*MatrixSendExtra
	sentTypes []string
	contents  []*event.Content
}

func (ti *testIntent) GetMXID() id.UserID { return ti.mxid }

func (ti *testIntent) EnsureJoined(context.Context, id.RoomID, ...EnsureJoinedParams) error {
	ti.joins++
	return nil
}

func (ti *testIntent) GetEvent(_ context.Context, _ id.RoomID, eventID id.EventID) (*event.Event, error) {
	content, ok := ti.readable[eventID]
	if !ok {
		return nil, errors.New("can't decrypt the event")
	}
	return &event.Event{ID: eventID, Type: event.EventMessage, Content: event.Content{Parsed: content}}, nil
}

func (ti *testIntent) SendMessage(_ context.Context, _ id.RoomID, evtType event.Type, content *event.Content, extra *MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	kind := "edit"
	if evtType == event.EventReaction {
		kind = "reaction"
	} else if c, ok := content.Parsed.(*event.MessageEventContent); ok && c.MsgType == event.MsgNotice {
		kind = "notice"
	}
	if ti.fail[kind] {
		return nil, errors.New("homeserver unavailable")
	}
	ti.sent = append(ti.sent, extra)
	ti.sentTypes = append(ti.sentTypes, kind)
	ti.contents = append(ti.contents, content)
	return &mautrix.RespSendEvent{EventID: id.EventID("$sent" + extra.TransactionID)}, nil
}

const (
	testBotMXID  id.UserID = "@bot:example.com"
	testUserMXID id.UserID = "@user:example.com"
)

// newTestPortal makes a portal owned by testUserMXID's login with a two-part
// message from alice, both deletion markers enabled.
func newTestPortal(t *testing.T, mode string) (*Portal, networkid.MessageID) {
	t.Helper()
	ctx := context.Background()
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "bridge.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &bridgeconfig.BridgeConfig{
		RemoteDeletes:        mode,
		RemoteDeleteNotice:   true,
		RemoteDeleteReaction: true,
		RemoteEdits:          RemoteEditsNewMessageOthers,
		Permissions:          bridgeconfig.PermissionConfig{"*": &bridgeconfig.PermissionLevelUser},
	}
	matrix := testMatrix{bot: &testIntent{mxid: testBotMXID}, ghosts: map[networkid.UserID]*testIntent{}}
	br := NewBridge("test", db, zerolog.Nop(), cfg, matrix, testNetwork{client: &testClient{}}, func(*Bridge) CommandProcessor { return nil })
	br.BackgroundCtx = ctx
	if err = br.DB.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if err = br.DB.User.Insert(ctx, &database.User{BridgeID: br.ID, MXID: testUserMXID}); err != nil {
		t.Fatal(err)
	}
	if err = br.DB.UserLogin.Insert(ctx, &database.UserLogin{BridgeID: br.ID, UserMXID: testUserMXID, ID: "login"}); err != nil {
		t.Fatal(err)
	}
	key := networkid.PortalKey{ID: "chat", Receiver: "login"}
	if err = br.DB.Portal.Insert(ctx, &database.Portal{BridgeID: br.ID, PortalKey: key, MXID: "!chat:example.com"}); err != nil {
		t.Fatal(err)
	}
	if err = br.DB.Ghost.Insert(ctx, &database.Ghost{BridgeID: br.ID, ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	for i, part := range []networkid.PartID{"0", "1"} {
		if err = br.DB.Message.Insert(ctx, &database.Message{
			BridgeID: br.ID, ID: "msg", PartID: part, MXID: id.EventID("$part" + string(part)),
			Room: key, SenderID: "alice", Timestamp: time.UnixMilli(int64(1000 + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	portal, err := br.GetPortalByKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return portal, "msg"
}

func testBot(portal *Portal) *testIntent { return portal.Bridge.Bot.(*testIntent) }

func loadParts(t *testing.T, portal *Portal, msgID networkid.MessageID) []*database.Message {
	t.Helper()
	parts, err := portal.Bridge.DB.Message.GetAllPartsByID(context.Background(), portal.Receiver, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// A message with a readable part (inline edit) and an unreadable one (marker
// reply). When the edit fails, no marker may be sent and the rows must stay
// even though the markers themselves could go through, so a retry can finish;
// the retry must reuse transaction IDs.
func TestMarkMessagePartsMixedFailureThenRetry(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot := testBot(portal)
	ghost := &testIntent{
		readable: map[id.EventID]*event.MessageEventContent{"$part0": {MsgType: event.MsgText, Body: "hello"}},
		fail:     map[string]bool{"edit": true},
	}

	res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now())
	if res.Success || res.Error == nil {
		t.Fatalf("first attempt = %+v, want a failure", res)
	}
	if len(ghost.sent)+len(bot.sent) != 0 {
		t.Fatalf("sent %v / %v on a failed attempt", ghost.sentTypes, bot.sentTypes)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 2 || parts[0].EditCount != 0 {
		t.Fatalf("rows after failure = %+v, want both untouched", parts)
	}

	ghost.fail = nil
	res = portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now())
	if !res.Success || res.Error != nil {
		t.Fatalf("retry = %+v, want success", res)
	}
	if len(ghost.sentTypes) != 1 || ghost.sentTypes[0] != "edit" {
		t.Fatalf("ghost sent %v, want only the edit", ghost.sentTypes)
	}
	if len(bot.sentTypes) != 2 || bot.sentTypes[0] != "reaction" || bot.sentTypes[1] != "notice" || bot.joins == 0 {
		t.Fatalf("bot sent %v (joined %d times), want a reaction then a notice after joining", bot.sentTypes, bot.joins)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 0 {
		t.Fatalf("rows after the marker reply = %d, want none", len(parts))
	}

	// The same part gets the same transaction ID on another attempt, so the
	// homeserver deduplicates a resend; edit, reaction and reply IDs differ.
	editTxn, reactionTxn, noticeTxn := ghost.sent[0].TransactionID, bot.sent[0].TransactionID, bot.sent[1].TransactionID
	part0 := &database.Message{ID: msgID, PartID: "0", MXID: "$part0", Room: portal.PortalKey}
	part1 := &database.Message{ID: msgID, PartID: "1", MXID: "$part1", Room: portal.PortalKey}
	if editTxn != remoteDeleteTxnID("edit", part0) || reactionTxn != remoteDeleteTxnID("reaction", part0) || noticeTxn != remoteDeleteTxnID("notice", part1) {
		t.Fatalf("transaction IDs edit=%q reaction=%q notice=%q aren't stable per part", editTxn, reactionTxn, noticeTxn)
	}
	if editTxn == reactionTxn || reactionTxn == noticeTxn || editTxn == noticeTxn {
		t.Fatalf("transaction IDs edit=%q reaction=%q notice=%q aren't distinct", editTxn, reactionTxn, noticeTxn)
	}
}

// Every part readable: the parts are edited in place and the rows are kept
// with their edit count raised. The message gets one reaction, no reply.
func TestMarkMessagePartsInlineKeepsRows(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot := testBot(portal)
	ghost := &testIntent{readable: map[id.EventID]*event.MessageEventContent{
		"$part0": {MsgType: event.MsgImage, Body: "shot.png"},
		"$part1": {MsgType: event.MsgText, Body: "caption"},
	}}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	parts := loadParts(t, portal, msgID)
	if len(parts) != 2 || parts[0].EditCount != 1 || parts[1].EditCount != 1 {
		t.Fatalf("rows = %+v, want both kept with one edit", parts)
	}
	if len(ghost.sentTypes) != 2 || ghost.sentTypes[0] != "edit" || ghost.sentTypes[1] != "edit" {
		t.Fatalf("ghost sent %v, want two edits", ghost.sentTypes)
	}
	if len(bot.sentTypes) != 1 || bot.sentTypes[0] != "reaction" {
		t.Fatalf("bot sent %v, want one reaction", bot.sentTypes)
	}
}

// Each combination of remote_delete_reaction and remote_delete_notice sends
// exactly the configured markers from the bot, the reaction once on the
// message's first part, and still keeps the message.
func TestRemoteDeleteMarkerConfig(t *testing.T) {
	for _, tc := range []struct {
		name             string
		reaction, notice bool
		want             []string
	}{
		{"both", true, true, []string{"reaction", "notice"}},
		{"reaction only", true, false, []string{"reaction"}},
		{"notice only", false, true, []string{"notice"}},
		{"neither", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			portal, msgID := newTestPortal(t, RemoteDeletesMark)
			portal.Bridge.Config.RemoteDeleteReaction = tc.reaction
			portal.Bridge.Config.RemoteDeleteNotice = tc.notice
			bot := testBot(portal)
			ghost := &testIntent{} // nothing readable: the marker path
			if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now()); !res.Success {
				t.Fatalf("result = %+v", res)
			}
			if len(ghost.sent) != 0 {
				t.Fatalf("deleter's ghost sent %v; markers must come from the bot", ghost.sentTypes)
			}
			if len(bot.sentTypes) != len(tc.want) {
				t.Fatalf("bot sent %v, want %v", bot.sentTypes, tc.want)
			}
			for i, kind := range tc.want {
				if bot.sentTypes[i] != kind {
					t.Fatalf("bot sent %v, want %v", bot.sentTypes, tc.want)
				}
				content := bot.contents[i]
				if content.Raw[remoteDeletedKey] != true {
					t.Fatalf("%s lacks the %s flag", kind, remoteDeletedKey)
				}
				switch kind {
				case "reaction":
					rel := content.Parsed.(*event.ReactionEventContent).RelatesTo
					if rel.Type != event.RelAnnotation || rel.EventID != "$part0" || rel.Key != RemoteDeleteReactionKey {
						t.Fatalf("reaction relates to %+v, want a 🗑️ annotation on $part0", rel)
					}
				case "notice":
					if reply := content.Parsed.(*event.MessageEventContent).RelatesTo.GetReplyTo(); reply != "$part0" {
						t.Fatalf("notice replies to %q, want $part0", reply)
					}
				}
			}
			if parts := loadParts(t, portal, msgID); len(parts) != 0 {
				t.Fatalf("rows = %d, want the kept message forgotten", len(parts))
			}
		})
	}
}

// The reaction goes on the part with the lowest ID, which is "" for messages
// whose main part has the empty ID, even when it isn't the first row.
func TestRemoteDeleteReactionTargetsFirstPart(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	if err := portal.Bridge.DB.Message.Insert(ctx, &database.Message{
		BridgeID: portal.BridgeID, ID: msgID, PartID: "", MXID: "$text",
		Room: portal.PortalKey, SenderID: "alice", Timestamp: time.UnixMilli(1005),
	}); err != nil {
		t.Fatal(err)
	}
	bot := testBot(portal)
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), &testIntent{}, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	reactions := 0
	for i, kind := range bot.sentTypes {
		if kind == "reaction" {
			reactions++
			if target := bot.contents[i].Parsed.(*event.ReactionEventContent).RelatesTo.EventID; target != "$text" {
				t.Fatalf("reaction on %s, want $text", target)
			}
		}
	}
	if reactions != 1 {
		t.Fatalf("sent %d reactions for a three-part message, want 1", reactions)
	}
}

// When the reaction went out but the reply failed, the rows stay; the retry
// resends the reaction with the same transaction ID and then forgets the rows.
func TestRemoteDeleteMarkersPartialFailureThenRetry(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot := testBot(portal)
	bot.fail = map[string]bool{"notice": true}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), &testIntent{}, time.Now()); res.Success {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 2 {
		t.Fatalf("rows after a failed reply = %d, want 2 kept for the retry", len(parts))
	}
	bot.fail = nil
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), &testIntent{}, time.Now()); !res.Success {
		t.Fatalf("retry = %+v", res)
	}
	if len(bot.sentTypes) != 3 || bot.sentTypes[0] != "reaction" || bot.sentTypes[1] != "reaction" || bot.sentTypes[2] != "notice" {
		t.Fatalf("bot sent %v, want reaction, then reaction and notice on the retry", bot.sentTypes)
	}
	if bot.sent[0].TransactionID != bot.sent[1].TransactionID {
		t.Fatalf("retried reaction transaction ID %q != %q", bot.sent[1].TransactionID, bot.sent[0].TransactionID)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 0 {
		t.Fatalf("rows after the retry = %d, want none", len(parts))
	}
}

// Inline edits that went through before the reaction failed: the retry finds
// the parts already marked and still sends the reaction.
func TestRemoteDeleteInlineReactionRetry(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot := testBot(portal)
	bot.fail = map[string]bool{"reaction": true}
	ghost := &markedIntent{testIntent: testIntent{readable: map[id.EventID]*event.MessageEventContent{
		"$part0": {MsgType: event.MsgText, Body: "a"},
		"$part1": {MsgType: event.MsgText, Body: "b"},
	}}}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now()); res.Success {
		t.Fatalf("result = %+v, want a failure", res)
	}
	bot.fail = nil
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now()); !res.Success {
		t.Fatalf("retry = %+v", res)
	}
	if len(ghost.sentTypes) != 2 {
		t.Fatalf("ghost sent %v, want the two edits once", ghost.sentTypes)
	}
	if len(bot.sentTypes) != 1 || bot.sentTypes[0] != "reaction" {
		t.Fatalf("bot sent %v on the retry, want the reaction", bot.sentTypes)
	}
}

// markedIntent returns its own marker edits as the newest edit, like a
// homeserver would after they were sent.
type markedIntent struct{ testIntent }

func (mi *markedIntent) GetLatestEdit(_ context.Context, _ id.RoomID, eventID id.EventID) (*event.Event, error) {
	for i, extra := range mi.sent {
		if extra.MessageMeta != nil && extra.MessageMeta.MXID == eventID {
			return &event.Event{Type: event.EventMessage, Content: *mi.contents[i]}, nil
		}
	}
	return nil, nil
}

// Own deletes stay real redactions under mark, so they get no markers.
func TestRemoteDeleteOwnMessageNotMarked(t *testing.T) {
	portal, _ := newTestPortal(t, RemoteDeletesMark)
	if portal.shouldMarkRemoteDelete(&simpleRemoteRemove{sender: EventSender{IsFromMe: true}}) {
		t.Fatal("own delete would be kept under mark")
	}
	if !portal.shouldMarkRemoteDelete(&simpleRemoteRemove{sender: EventSender{Sender: "alice"}}) {
		t.Fatal("colleague's delete wouldn't be kept under mark")
	}
}

type simpleRemoteRemove struct {
	RemoteMessageRemove
	sender EventSender
}

func (s *simpleRemoteRemove) GetSender() EventSender                { return s.sender }
func (s *simpleRemoteRemove) GetTargetMessage() networkid.MessageID { return "msg" }
func (s *simpleRemoteRemove) GetType() RemoteEventType              { return RemoteEventMessageRemove }

// The bot's marker reaction must never reach the network's reaction API: not
// its echo, not a retried echo, not flagged content sent by someone else, and
// not the user tapping the 🗑️ chip while the marker is enabled. A plain
// reaction by the user is the control and is forwarded.
func TestRemoteDeleteReactionNeverForwarded(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	bot := testBot(portal)
	ghost := &testIntent{readable: map[id.EventID]*event.MessageEventContent{
		"$part0": {MsgType: event.MsgText, Body: "a"},
		"$part1": {MsgType: event.MsgText, Body: "b"},
	}}
	// Inline edits keep the rows, so the reaction's target is still known.
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), ghost, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if len(bot.contents) != 1 {
		t.Fatalf("bot sent %v, want one reaction", bot.sentTypes)
	}
	marker := bot.contents[0]
	client := portal.Bridge.Network.(testNetwork).client
	queue := func(sender id.UserID, key string, raw map[string]any, n int) EventHandlingResult {
		rel := marker.Parsed.(*event.ReactionEventContent).RelatesTo
		rel.Key = key
		return portal.Bridge.QueueMatrixEvent(ctx, &event.Event{
			ID: id.EventID(fmt.Sprintf("$reaction%d", n)), RoomID: portal.MXID, Sender: sender,
			Type: event.EventReaction, Timestamp: 2000,
			Content: event.Content{Parsed: &event.ReactionEventContent{RelatesTo: rel}, Raw: raw},
		})
	}
	flagged := map[string]any{remoteDeletedKey: true}
	trash := RemoteDeleteReactionKey
	for i, tc := range []struct {
		name   string
		sender id.UserID
		key    string
		raw    map[string]any
	}{
		{"bot echo", testBotMXID, trash, flagged},
		{"bot echo retried", testBotMXID, trash, flagged},
		{"bot reaction without the flag", testBotMXID, trash, nil},
		{"flagged reaction from the user", testUserMXID, "👍", flagged},
		{"user tapping the 🗑️ chip", testUserMXID, trash, nil},
		{"edited flag from the user", testUserMXID, "👍", map[string]any{remoteEditedKey: true}},
		{"bot ✏️ echo", testBotMXID, RemoteEditReactionKey, map[string]any{remoteEditedKey: true}},
		{"user tapping the ✏️ chip", testUserMXID, RemoteEditReactionKey, nil},
	} {
		if res := queue(tc.sender, tc.key, tc.raw, i); !res.Ignored {
			t.Errorf("%s: result = %+v, want it ignored", tc.name, res)
		}
	}
	if len(client.reactions) != 0 {
		t.Fatalf("network got reactions %v from markers", client.reactions)
	}
	if res := queue(testUserMXID, "👍", nil, 99); !res.Success || res.Ignored {
		t.Fatalf("control reaction result = %+v", res)
	}
	if len(client.reactions) != 1 || client.reactions[0] != "👍" {
		t.Fatalf("network got %v, want the user's own 👍", client.reactions)
	}
	// With the markers off, 🗑️ and ✏️ are ordinary reactions again.
	portal.Bridge.Config.RemoteDeleteReaction = false
	portal.Bridge.Config.RemoteEdits = RemoteEditsReplace
	for i, key := range []string{trash, RemoteEditReactionKey} {
		if res := queue(testUserMXID, key, nil, 100+i); !res.Success || res.Ignored {
			t.Fatalf("%s with the markers off: result = %+v", key, res)
		}
	}
	if len(client.reactions) != 3 || client.reactions[1] != trash || client.reactions[2] != RemoteEditReactionKey {
		t.Fatalf("network got %v, want the user's 🗑️ and ✏️ with the markers off", client.reactions)
	}
}

type completionEvent struct {
	RemoteEvent
	result *EventHandlingResult
	panics bool
	calls  []EventHandlingResult
}

func (ce *completionEvent) PreHandleWithResult(context.Context, *Portal) *EventHandlingResult {
	if ce.panics {
		panic("connector bug")
	}
	return ce.result
}

func (ce *completionEvent) HandleCompleted(_ context.Context, res EventHandlingResult) {
	ce.calls = append(ce.calls, res)
}

func TestRemoteEventCompletion(t *testing.T) {
	portal, _ := newTestPortal(t, RemoteDeletesRedact)
	ctx := zerolog.Nop().WithContext(context.Background())
	failed := EventHandlingResultFailed.WithError(errors.New("network refused"))
	for _, tc := range []struct {
		name    string
		evt     *completionEvent
		success bool
		errIs   error
	}{
		{"success", &completionEvent{result: &EventHandlingResultSuccess}, true, nil},
		{"failure", &completionEvent{result: &failed}, false, failed.Error},
		{"panic", &completionEvent{panics: true}, false, ErrPanicInEventHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var done []EventHandlingResult
			portal.handleSingleEvent(ctx, &portalRemoteEvent{evt: tc.evt, evtType: RemoteEventMessageRemove}, func(res EventHandlingResult) {
				done = append(done, res)
			})
			if len(done) != 1 {
				t.Fatalf("done callback called %d times", len(done))
			}
			if len(tc.evt.calls) != 1 {
				t.Fatalf("HandleCompleted called %d times, want once", len(tc.evt.calls))
			}
			got := tc.evt.calls[0]
			if got.Success != tc.success || (tc.errIs == nil) != (got.Error == nil) || (tc.errIs != nil && !errors.Is(got.Error, tc.errIs)) {
				t.Fatalf("HandleCompleted got %+v, want success=%v error %v", got, tc.success, tc.errIs)
			}
		})
	}
}
