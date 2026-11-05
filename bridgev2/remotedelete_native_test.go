// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"path/filepath"
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

type testNetwork struct{ NetworkConnector }

func (testNetwork) Init(*Bridge)                       {}
func (testNetwork) GetName() BridgeName                { return BridgeName{DisplayName: "Test"} }
func (testNetwork) GetDBMetaTypes() database.MetaTypes { return database.MetaTypes{} }

type testMatrix struct{ MatrixConnector }

func (testMatrix) Init(*Bridge)         {}
func (testMatrix) BotIntent() MatrixAPI { return nil }

// testIntent reads back events from a fixed set and records sends; edits
// fail while failEdits is set, while replies still go through.
type testIntent struct {
	MatrixAPI
	readable  map[id.EventID]*event.MessageEventContent
	failEdits bool
	sent      []*MatrixSendExtra
	sentTypes []string
}

func (ti *testIntent) GetEvent(_ context.Context, _ id.RoomID, eventID id.EventID) (*event.Event, error) {
	content, ok := ti.readable[eventID]
	if !ok {
		return nil, errors.New("can't decrypt the event")
	}
	return &event.Event{ID: eventID, Type: event.EventMessage, Content: event.Content{Parsed: content}}, nil
}

func (ti *testIntent) SendMessage(_ context.Context, _ id.RoomID, _ event.Type, content *event.Content, extra *MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	kind := "edit"
	if c, ok := content.Parsed.(*event.MessageEventContent); ok && c.MsgType == event.MsgNotice {
		kind = "notice"
	}
	if kind == "edit" && ti.failEdits {
		return nil, errors.New("homeserver unavailable")
	}
	ti.sent = append(ti.sent, extra)
	ti.sentTypes = append(ti.sentTypes, kind)
	return &mautrix.RespSendEvent{EventID: id.EventID("$sent" + extra.TransactionID)}, nil
}

func newTestPortal(t *testing.T, mode string) (*Portal, networkid.MessageID) {
	t.Helper()
	ctx := context.Background()
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "bridge.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	br := NewBridge("test", db, zerolog.Nop(), &bridgeconfig.BridgeConfig{RemoteDeletes: mode}, testMatrix{}, testNetwork{}, func(*Bridge) CommandProcessor { return nil })
	br.BackgroundCtx = ctx
	if err = br.DB.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	key := networkid.PortalKey{ID: "chat"}
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

func loadParts(t *testing.T, portal *Portal, msgID networkid.MessageID) []*database.Message {
	t.Helper()
	parts, err := portal.Bridge.DB.Message.GetAllPartsByID(context.Background(), portal.Receiver, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// A message with a readable part (inline edit) and an unreadable one (marker
// reply). When the edit fails, the reply must not be sent and the rows must
// stay even though the reply itself could go through, so a retry can finish;
// the retry must reuse transaction IDs.
func TestMarkMessagePartsMixedFailureThenRetry(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	intent := &testIntent{
		readable:  map[id.EventID]*event.MessageEventContent{"$part0": {MsgType: event.MsgText, Body: "hello"}},
		failEdits: true,
	}

	res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), intent, time.Now())
	if res.Success || res.Error == nil {
		t.Fatalf("first attempt = %+v, want a failure", res)
	}
	if len(intent.sent) != 0 {
		t.Fatalf("sent %v on a failed attempt", intent.sentTypes)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 2 || parts[0].EditCount != 0 {
		t.Fatalf("rows after failure = %+v, want both untouched", parts)
	}

	intent.failEdits = false
	res = portal.markMessageParts(ctx, loadParts(t, portal, msgID), intent, time.Now())
	if !res.Success || res.Error != nil {
		t.Fatalf("retry = %+v, want success", res)
	}
	if len(intent.sentTypes) != 2 || intent.sentTypes[0] != "edit" || intent.sentTypes[1] != "notice" {
		t.Fatalf("retry sent %v, want an edit then a notice", intent.sentTypes)
	}
	if parts := loadParts(t, portal, msgID); len(parts) != 0 {
		t.Fatalf("rows after the marker reply = %d, want none", len(parts))
	}

	// The same part gets the same transaction ID on another attempt, so the
	// homeserver deduplicates a resend; edit and reply IDs differ.
	editTxn, noticeTxn := intent.sent[0].TransactionID, intent.sent[1].TransactionID
	part0 := &database.Message{ID: msgID, PartID: "0", Room: portal.PortalKey}
	part1 := &database.Message{ID: msgID, PartID: "1", Room: portal.PortalKey}
	if editTxn == "" || editTxn != remoteDeleteTxnID("edit", part0) || noticeTxn != remoteDeleteTxnID("notice", part1) || editTxn == noticeTxn {
		t.Fatalf("transaction IDs edit=%q notice=%q aren't stable per part", editTxn, noticeTxn)
	}
}

// Every part readable: the parts are edited in place and the rows are kept
// with their edit count raised.
func TestMarkMessagePartsInlineKeepsRows(t *testing.T) {
	ctx := context.Background()
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	intent := &testIntent{readable: map[id.EventID]*event.MessageEventContent{
		"$part0": {MsgType: event.MsgImage, Body: "shot.png"},
		"$part1": {MsgType: event.MsgText, Body: "caption"},
	}}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, msgID), intent, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	parts := loadParts(t, portal, msgID)
	if len(parts) != 2 || parts[0].EditCount != 1 || parts[1].EditCount != 1 {
		t.Fatalf("rows = %+v, want both kept with one edit", parts)
	}
	if len(intent.sentTypes) != 2 || intent.sentTypes[0] != "edit" || intent.sentTypes[1] != "edit" {
		t.Fatalf("sent %v, want two edits", intent.sentTypes)
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

type removeEvent struct {
	RemoteEvent
	sender EventSender
}

func (re removeEvent) GetSender() EventSender                { return re.sender }
func (re removeEvent) GetTargetMessage() networkid.MessageID { return "msg" }

// Networks that don't name the deleter: a delete of the user's own message
// stays a real delete, anyone else's is kept.
func TestShouldMarkRemoteDeleteUnknownDeleter(t *testing.T) {
	portal, msgID := newTestPortal(t, RemoteDeletesMark)
	source := &UserLogin{UserLogin: &database.UserLogin{UserMXID: "@me:example.com"}}
	parts := loadParts(t, portal, msgID)
	if !portal.shouldMarkRemoteDelete(removeEvent{}, source, parts) {
		t.Error("someone else's message deleted by an unknown deleter wasn't kept")
	}
	for _, part := range parts {
		part.SenderMXID = "@me:example.com"
	}
	if portal.shouldMarkRemoteDelete(removeEvent{}, source, parts) {
		t.Error("the user's own message deleted by an unknown deleter was kept")
	}
	// A named deleter decides on its own: someone else deleting the user's message.
	if !portal.shouldMarkRemoteDelete(removeEvent{sender: EventSender{Sender: "alice"}}, source, parts) {
		t.Error("a delete by a named other user wasn't kept")
	}
	if portal.shouldMarkRemoteDelete(removeEvent{sender: EventSender{Sender: "me", IsFromMe: true}}, source, parts) {
		t.Error("the user's own delete was kept")
	}
}
