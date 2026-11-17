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
	"maunium.net/go/mautrix/event"
)

const (
	addedSpan   = `<span data-mx-color="#00FF00">`
	removedSpan = `<span data-mx-color="#FF6B6B">`
)

func TestRenderInlineDiff(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{{
		// A one-letter Vietnamese correction marks only that word.
		name: "typo",
		old:  "anh thử check xem cu thể lỗi gì ạ.",
		new:  "anh thử check xem cụ thể lỗi gì ạ.",
		want: `<p>anh thử check xem ` + removedSpan + `<s>cu</s></span> ` + addedSpan + `cụ</span> thể lỗi gì ạ.</p>`,
	}, {
		name: "list item added, removed and swapped",
		old:  "Checklist:\n- Kiểm tra network.\n- Khởi động lại bridge.\n- Gửi log.",
		new:  "Checklist:\n- Kiểm tra network.\n- Kiểm tra DNS.\n- Gửi log.\n- Báo anh.",
		want: `<p>Checklist:<br>- Kiểm tra network.<br>- ` + removedSpan + `<s>Khởi động lại bridge.</s></span><br>- ` +
			addedSpan + `+ Kiểm tra DNS.</span><br>- Gửi log.<br>- ` + addedSpan + `+ Báo anh.</span></p>`,
	}, {
		// Code keeps one block with explicit row breaks and −/+ prefixes.
		name: "code block value",
		old:  "Config:\n```\nconst config = {\n  timeoutMs: 5000,\n};\n```",
		new:  "Config:\n```\nconst config = {\n  timeoutMs: 10000,\n};\n```",
		want: `<p>Config:</p><pre><code> const config = {<br>` + removedSpan + `-  timeoutMs: 5000,</span><br>` +
			addedSpan + `+  timeoutMs: 10000,</span><br> };</code></pre>`,
	}, {
		name: "html in text is escaped",
		old:  "a <b>",
		new:  "a <i>",
		want: `<p>a ` + removedSpan + `<s>&lt;b&gt;</s></span> ` + addedSpan + `&lt;i&gt;</span></p>`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := renderInlineDiff(tc.old, tc.new)
			if !ok || got != tc.want {
				t.Fatalf("got %v\n%s\nwant\n%s", ok, got, tc.want)
			}
		})
	}
	if _, ok := renderInlineDiff("same", "same"); ok {
		t.Error("an unchanged text got a diff")
	}
	huge := strings.Repeat("line\n", 1000)
	if _, ok := renderInlineDiff(huge+"a", "b\n"+huge); ok {
		t.Error("an edit too large to diff got one")
	}
}

// A deleted message keeps its text in red only, without formatting marks,
// and code keeps its rows.
func TestDeletedContent(t *testing.T) {
	body, formatted := deletedContent(partText{
		Body:          "Hẹn **anh**",
		FormattedBody: "<p>Hẹn <strong>anh</strong> lúc 15:00</p><pre><code>a\n  b</code></pre>",
	})
	want := `<p>` + removedSpan + `Hẹn anh lúc 15:00</span></p><pre><code>` + removedSpan + `a</span><br>` + removedSpan + `  b</span></code></pre>`
	if formatted != want {
		t.Fatalf("formatted\n%s\nwant\n%s", formatted, want)
	}
	if strings.Contains(body, "**") || !strings.Contains(body, "Hẹn anh lúc 15:00") {
		t.Fatalf("body %q", body)
	}
}

func inlineDiffPortal(t *testing.T) (*Portal, *UserLogin) {
	t.Helper()
	portal, _ := newTestPortal(t, RemoteDeletesMark)
	portal.Bridge.Config.RemoteEdits = RemoteEditsInlineDiff
	return portal, testLogin(t, portal)
}

func rememberText(t *testing.T, portal *Portal, login *UserLogin, part *database.Message, body string) {
	t.Helper()
	err := portal.rememberPartTexts(context.Background(), login, "alice", []*database.Message{part},
		[]*ConvertedMessagePart{{ID: part.PartID, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body}}})
	if err != nil {
		t.Fatal(err)
	}
}

// Someone else's edit replaces the message in place, without a new bubble or
// a bot reaction, and its new content shows what changed. The next edit diffs
// against this one, not the original.
func TestInlineDiffEditsInPlace(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, login := inlineDiffPortal(t)
	bot, ghost := testBot(portal), &testIntent{}
	rememberText(t, portal, login, partByID(t, portal, "msg", "1"), "Chờ 5 phút")

	for i, step := range []struct{ body, changed string }{
		{"Chờ 10 phút", removedSpan + "<s>5</s></span> " + addedSpan + "10</span>"},
		{"Chờ 15 phút", removedSpan + "<s>10</s></span> " + addedSpan + "15</span>"},
	} {
		res := portal.sendConvertedEdit(ctx, login, "msg", "alice", editOf(partByID(t, portal, "msg", "1"), step.body), ghost, time.Now(), 0)
		if !res.Success {
			t.Fatalf("edit %d = %+v", i, res)
		}
		sent := ghost.contents[i].Parsed.(*event.MessageEventContent)
		if sent.RelatesTo.GetReplaceID() != "$part1" || sent.NewContent == nil {
			t.Fatalf("edit %d sent %+v, want an m.replace of $part1", i, sent.RelatesTo)
		}
		if !strings.Contains(sent.NewContent.FormattedBody, step.changed) || sent.NewContent.Format != event.FormatHTML {
			t.Fatalf("edit %d new content %q, want it to mark %q", i, sent.NewContent.FormattedBody, step.changed)
		}
	}
	if len(bot.sent) != 0 || partByID(t, portal, "msg", "1").MXID != "$part1" {
		t.Fatalf("bot sent %v or the row moved; inline diffs edit in place", bot.sentTypes)
	}
}

// The user's own edits, and edits of messages whose text wasn't kept (sent
// before inline diffs), replace in place without markers.
func TestInlineDiffPlainEdits(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	portal, login := inlineDiffPortal(t)
	own := partByID(t, portal, "msg", "0")
	own.SenderMXID = testUserMXID
	rememberText(t, portal, login, own, "mine")
	ghost := &testIntent{}
	for _, part := range []*database.Message{own, partByID(t, portal, "msg", "1")} {
		if res := portal.sendConvertedEdit(ctx, login, "msg", "alice", editOf(part, "new"), ghost, time.Now(), 0); !res.Success {
			t.Fatalf("result = %+v", res)
		}
	}
	for i, content := range ghost.contents {
		if sent := content.Parsed.(*event.MessageEventContent); sent.NewContent.FormattedBody != "" {
			t.Fatalf("edit %d marked %q, want a plain replacement", i, sent.NewContent.FormattedBody)
		}
	}
	// The unkept message's text is kept now, so its next edit is diffed.
	if text, err := portal.getPartText(ctx, partByID(t, portal, "msg", "1")); err != nil || text == nil || text.Body != "new" {
		t.Fatalf("kept text after first edit = %+v, %v", text, err)
	}
}

// A kept delete turns the message red from its kept text even though the
// bridge can't read its own events back: no 🗑️, no marker reply, rows kept.
func TestInlineDiffDeleteIsRedText(t *testing.T) {
	ctx := context.Background()
	portal, login := inlineDiffPortal(t)
	for _, part := range loadParts(t, portal, "msg") {
		rememberText(t, portal, login, part, "Hẹn anh lúc 15:00 part "+string(part.PartID))
	}
	bot, ghost := testBot(portal), &testIntent{}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, "msg"), ghost, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if len(bot.sent) != 0 {
		t.Fatalf("bot sent %v, want red text as the only marker", bot.sentTypes)
	}
	if len(ghost.contents) != 2 {
		t.Fatalf("ghost sent %v, want an edit of each part", ghost.sentTypes)
	}
	for i, content := range ghost.contents {
		sent := content.Parsed.(*event.MessageEventContent)
		if sent.RelatesTo.GetReplaceID() == "" || !strings.HasPrefix(sent.NewContent.FormattedBody, "<p>"+removedSpan+"Hẹn anh lúc 15:00") ||
			strings.Contains(sent.NewContent.Body, "Deleted") || content.Raw[remoteDeletedKey] != true {
			t.Fatalf("part %d sent %+v %v", i, sent.NewContent, content.Raw)
		}
	}
	if len(loadParts(t, portal, "msg")) != 2 {
		t.Fatal("the rows of a message kept in red were deleted")
	}
	// A repeated delete stacks nothing: the kept text is still the original.
	text, err := portal.getPartText(ctx, partByID(t, portal, "msg", "0"))
	if err != nil || text == nil || strings.Contains(text.FormattedBody, "FF6B6B") {
		t.Fatalf("kept text after delete = %+v, %v", text, err)
	}
}

// A part without kept text (such as media) still gets the usual marker reply.
func TestInlineDiffDeleteFallsBackToMarkers(t *testing.T) {
	ctx := context.Background()
	portal, login := inlineDiffPortal(t)
	rememberText(t, portal, login, partByID(t, portal, "msg", "0"), "caption")
	bot, ghost := testBot(portal), &testIntent{}
	if res := portal.markMessageParts(ctx, loadParts(t, portal, "msg"), ghost, time.Now()); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if len(ghost.sentTypes) != 1 || len(bot.sentTypes) != 2 || bot.sentTypes[1] != "notice" {
		t.Fatalf("ghost sent %v, bot %v; want the red edit plus the reaction and reply for the unreadable part", ghost.sentTypes, bot.sentTypes)
	}
}
