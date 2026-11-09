// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func annotated(t *testing.T, orig *event.MessageEventContent) (*event.MessageEventContent, map[string]any) {
	t.Helper()
	newContent := buildAnnotatedContent(orig, []switchboardNote{{Kind: "translation", Lang: "en", SourceLang: "vi", Text: "Are you free <tonight>?"}})
	data, err := json.Marshal(newContent)
	if err != nil {
		t.Fatal(err)
	}
	var content event.MessageEventContent
	var raw map[string]any
	if err = json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data, &raw)
	return &content, raw
}

func TestSanitizeSwitchboardNotesRestoresOriginal(t *testing.T) {
	for _, orig := range []*event.MessageEventContent{
		{MsgType: event.MsgText, Body: "Tối nay anh rảnh không?"},
		{MsgType: event.MsgText, Body: "**Tối** nay", Format: event.FormatHTML, FormattedBody: "<b>Tối</b> nay"},
		{MsgType: event.MsgText, Body: "> Ghi chú\n1. Kiểm tra\n\nSELECT 1;", Format: event.FormatHTML,
			FormattedBody: "<blockquote>Ghi chú</blockquote><ol><li>Kiểm tra</li></ol><pre><code>SELECT 1;</code></pre>"},
	} {
		content, raw := annotated(t, orig)
		if content.Body == orig.Body {
			t.Fatal("annotation didn't change the body")
		}
		// Forwarded or re-sent with the notes key: the original comes back exactly.
		if !SanitizeSwitchboardNotes(content, raw) || content.Body != orig.Body || content.FormattedBody != orig.FormattedBody {
			t.Errorf("with key: got %q / %q, want %q / %q", content.Body, content.FormattedBody, orig.Body, orig.FormattedBody)
		}
		if _, ok := raw[SwitchboardNotesKey]; ok {
			t.Error("notes key still present")
		}
		// Copied as shown text only (no key), as a client does when quoting or forwarding.
		content, _ = annotated(t, orig)
		if !SanitizeSwitchboardNotes(content, map[string]any{}) || content.Body != orig.Body {
			t.Errorf("without key: got body %q, want %q", content.Body, orig.Body)
		}
		if orig.FormattedBody != "" && content.FormattedBody != orig.FormattedBody {
			t.Errorf("without key: got html %q, want %q", content.FormattedBody, orig.FormattedBody)
		}
	}
}

func TestSanitizeSwitchboardNotesLeavesOrdinaryMessages(t *testing.T) {
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: "🌐 travel plans\n\nsee you", Format: event.FormatHTML, FormattedBody: "<p>🌐 travel</p>"}
	if SanitizeSwitchboardNotes(content, map[string]any{}) || content.Body != "🌐 travel plans\n\nsee you" || content.FormattedBody != "<p>🌐 travel</p>" {
		t.Errorf("ordinary message changed: %q / %q", content.Body, content.FormattedBody)
	}
}

func TestSwitchboardCommandMatch(t *testing.T) {
	for body, want := range map[string]bool{"!sb": true, "!sb translate on": true, "!sbx": false, "!zalo help": false, " !sb": false, "say !sb": false} {
		if got := switchboardCommand.MatchString(body); got != want {
			t.Errorf("%q: got %v, want %v", body, got, want)
		}
	}
}

func TestSwitchboardNotesNeverReachTheNetwork(t *testing.T) {
	portal, _ := newTestPortal(t, RemoteDeletesMark)
	ctx := context.Background()
	client := portal.Bridge.Network.(testNetwork).client
	orig := &event.MessageEventContent{MsgType: event.MsgText, Body: "Tối nay anh rảnh không?"}
	// Re-sent with the notes key (as a client copying the event does), then pasted as shown text only.
	withKey, raw := annotated(t, orig)
	shownOnly, _ := annotated(t, orig)
	for i, content := range []event.Content{{Parsed: withKey, Raw: raw}, {Parsed: shownOnly}} {
		res := portal.Bridge.QueueMatrixEvent(ctx, &event.Event{
			ID: id.EventID(fmt.Sprintf("$out%d", i)), RoomID: portal.MXID, Sender: testUserMXID,
			Type: event.EventMessage, Timestamp: int64(3000 + i), Content: content,
		})
		if !res.Success {
			t.Fatalf("send %d = %+v", i, res)
		}
	}
	if len(client.bodies) != 2 || client.bodies[0] != orig.Body || client.bodies[1] != orig.Body {
		t.Fatalf("network got %q, want the original twice", client.bodies)
	}
}

func TestSwitchboardAnnotateEditsSilentlyAsTheSender(t *testing.T) {
	portal, _ := newTestPortal(t, RemoteDeletesMark)
	ctx := context.Background()
	if err := portal.Bridge.DB.Message.Update(ctx, &database.Message{
		RowID: partByID(t, portal, "msg", "0").RowID, BridgeID: portal.Bridge.ID, ID: "msg", PartID: "0", MXID: "$part0",
		Room: portal.PortalKey, SenderID: "alice", SenderMXID: "@ghost_alice:example.com", Timestamp: time.UnixMilli(1000),
	}); err != nil {
		t.Fatal(err)
	}
	tap := &SwitchboardTap{br: portal.Bridge, cache: map[id.EventID]*event.MessageEventContent{}}
	origHTML := "<blockquote>Ghi chú</blockquote><ul><li>Kiểm tra</li></ul><pre><code>SELECT 1;</code></pre>"
	tap.remember("$part0", &event.MessageEventContent{MsgType: event.MsgText, Body: "> Ghi chú\n- Kiểm tra\n\nSELECT 1;", Format: event.FormatHTML, FormattedBody: origHTML})
	_, err := tap.annotate(ctx, &switchboardRPC{RoomID: portal.MXID, EventID: "$part0", TxnID: "k1",
		Notes: []switchboardNote{{Kind: "translation", Lang: "en", Text: "Note\n- Check <status>"}}})
	if err != nil {
		t.Fatal(err)
	}
	alice := portal.Bridge.Matrix.(testMatrix).ghosts["alice"]
	if alice == nil || len(alice.contents) != 1 || testBot(portal).contents != nil {
		t.Fatal("the edit wasn't sent by alice's ghost alone")
	}
	sent := alice.contents[0]
	newContent, _ := sent.Raw["m.new_content"].(map[string]any)
	if sent.Raw["com.beeper.dont_render_edited"] != true || alice.sent[0].TransactionID != "sb-k1" ||
		sent.Parsed.(*event.MessageEventContent).RelatesTo.GetReplaceID() != "$part0" ||
		!strings.Contains(newContent["body"].(string), "Note\n- Check <status>") || newContent[SwitchboardNotesKey] == nil {
		t.Fatalf("unexpected edit: %+v / %+v", sent.Raw, alice.sent[0])
	}
	html := newContent["formatted_body"].(string)
	if !strings.Contains(html, origHTML+"</font><p data-sb-note>"+switchboardNoteMarker+"────────</p>") ||
		strings.Contains(html, "<hr") ||
		!strings.Contains(newContent["body"].(string), "\n\n"+switchboardNoteMarker+"────────\n") ||
		!strings.Contains(html, "<em>Note<br/>- Check &lt;status&gt;</em>") {
		t.Fatalf("original formatting, text separator or italic escaped translation lost: %s", html)
	}
	_, err = tap.annotate(ctx, &switchboardRPC{RoomID: portal.MXID, EventID: "$part0", TxnID: "k2",
		Notes: []switchboardNote{{Kind: "translation", Lang: "en", Format: "markdown", Text: "*Use* `printf('tiếng Việt')`\n\n3. *Check report*\n   - *Send before 5 PM*\n4. *Done*\n\n<script>alert(1)</script>"}}})
	if err != nil {
		t.Fatal(err)
	}
	structured := alice.contents[1].Raw["m.new_content"].(map[string]any)
	html = structured["formatted_body"].(string)
	for _, want := range []string{origHTML, `<ol start="3">`, "<ul>", "<em>Check report</em>", "<code>printf('tiếng Việt')</code>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("structured note lost %q: %s", want, html)
		}
	}
	if strings.Contains(html, "<script>") {
		t.Fatal("note rendered raw HTML")
	}
	// Forwarding the structured annotation still restores only the original, with its formatting.
	data, _ := json.Marshal(structured)
	var outgoing event.MessageEventContent
	_ = json.Unmarshal(data, &outgoing)
	SanitizeSwitchboardNotes(&outgoing, nil)
	if outgoing.FormattedBody != origHTML || outgoing.Body != "> Ghi chú\n- Kiểm tra\n\nSELECT 1;" {
		t.Fatalf("structured note leaked into a copy: %+v", outgoing)
	}
}
