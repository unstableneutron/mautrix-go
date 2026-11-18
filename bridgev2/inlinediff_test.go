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
		// Reconstructed from the screenshot; raw mention markup is unavailable.
		name: "remove everyone without repeating the name",
		old:  "e đang phỏng vấn anh Trịnh Thế Thành @Everyone",
		new:  "e đang phỏng vấn anh Trịnh Thế Thành",
		want: `<p>e đang phỏng vấn anh Trịnh Thế Thành ` + removedSpan + `<s>@Everyone</s></span></p>`,
	}, {
		// A plausible hidden-space variant, not a claim about the raw event.
		name: "mention display with nonbreaking space",
		old:  "e đang phỏng vấn anh\u00a0Trịnh Thế Thành @Everyone",
		new:  "e đang phỏng vấn anh Trịnh Thế Thành",
		want: `<p>e đang phỏng vấn anh Trịnh Thế Thành ` + removedSpan + `<s>@Everyone</s></span></p>`,
	}, {
		name: "decomposed Vietnamese and emoji stay whole",
		old:  "Hẹn cu\u0323 👩‍💻 hôm nay",
		new:  "Hẹn cu\u0303 👩‍💻 hôm nay",
		want: `<p>Hẹn ` + removedSpan + "<s>cu\u0323</s></span> " + addedSpan + "cu\u0303</span> 👩‍💻 hôm nay</p>",
	}, {
		name: "retain meaningful phrase instead of several tiny words",
		old:  "x a b c importantword z",
		new:  "x importantword a b c z",
		want: `<p>x ` + removedSpan + `<s>a b c</s></span> importantword ` + addedSpan + `a b c</span> z</p>`,
	}, {
		name: "moved phrase and punctuation keep surrounding words",
		old:  "con giao tiếp với vsd lại là con vsd-client của vsd ạ, mà nó chạy bằng servicemix chứ không phải vsd-gw nhà mình, mà con này thì lại java8 a ạ",
		new:  "con giao tiếp với vsd lại là con vsd-client của vsd chứ không phải vsd-gw nhà mình ạ, mà nó chạy bằng servicemix, mà con này thì lại java8 a ạ",
		want: `<p>con giao tiếp với vsd lại là con vsd-client của vsd ` + removedSpan + `<s>ạ, mà nó chạy bằng servicemix</s></span> chứ không phải vsd-gw nhà mình ` + addedSpan + `ạ, mà nó chạy bằng servicemix</span>, mà con này thì lại java8 a ạ</p>`,
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
	}, {
		name: "reflow with a real edit keeps latest line breaks",
		old:  "Hẹn anh Trịnh\nThế Thành lúc 5 giờ",
		new:  "Hẹn anh\nTrịnh Thế Thành lúc 10 giờ",
		want: `<p>Hẹn anh<br>Trịnh Thế Thành lúc ` + removedSpan + `<s>5</s></span> ` + addedSpan + `10</span> giờ</p>`,
	}, {
		name: "paragraph and list boundaries survive prose reflow",
		old:  "Hẹn anh\nTrịnh\n\n- Gửi log.\n- Chờ 5 phút.",
		new:  "Hẹn anh Trịnh\n\n- Gửi log.\n- Chờ 10 phút.",
		want: `<p>Hẹn anh Trịnh<br><br>- Gửi log.<br>- Chờ ` + removedSpan + `<s>5</s></span> ` + addedSpan + `10</span> phút.</p>`,
	}, {
		name: "code indentation remains a real edit",
		old:  "```\n  run()\n```",
		new:  "```\n    run()\n```",
		want: `<pre><code>` + removedSpan + `-  run()</span><br>` + addedSpan + `+    run()</span></code></pre>`,
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
	for _, pair := range [][2]string{
		{"e  đang phỏng vấn\nanh\u00a0Trịnh Thế Thành", "e đang phỏng vấn anh Trịnh Thế Thành"},
		{"Hẹn anh Trịnh Thế Thành", "Hẹn\nanh Trịnh  Thế Thành"},
		{"- Gửi  log.\n- Chờ 5 phút.", "- Gửi log.\n- Chờ 5 phút."},
	} {
		if got, ok := renderInlineDiff(pair[0], pair[1]); ok {
			t.Errorf("layout-only edit got a diff: %s", got)
		}
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

func TestInlineDiffFormattedMention(t *testing.T) {
	portal, login := inlineDiffPortal(t)
	part := partByID(t, portal, "msg", "1")
	rememberText(t, portal, login, part, "Hẹn Thành @Everyone")
	edit := editOf(part, "Hẹn Thành").ModifiedParts[0]
	edit.Content.Format = event.FormatHTML
	edit.Content.FormattedBody = `<p><strong>Hẹn</strong> <a href="https://matrix.to/#/@thanh:example.org">Thành</a></p>`
	edit.Content.Mentions = &event.Mentions{Room: true}
	cleanHTML := edit.Content.FormattedBody
	mentions := edit.Content.Mentions
	if err := portal.applyInlineDiff(context.Background(), edit); err != nil {
		t.Fatal(err)
	}
	if edit.Content.FormattedBody != `<p>Hẹn Thành `+removedSpan+`<s>@Everyone</s></span></p>` || edit.Content.Mentions != mentions {
		t.Fatalf("unexpected content: %+v", edit.Content)
	}
	stored, err := portal.getPartText(context.Background(), part)
	if err != nil || stored == nil || stored.FormattedBody != cleanHTML {
		t.Fatalf("lost clean formatted baseline: %+v, %v", stored, err)
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
	// Layout-only edits still send the latest content (including its markup).
	part := partByID(t, portal, "msg", "1")
	rememberText(t, portal, login, part, "Hẹn anh\nTrịnh  Thế Thành")
	layout := editOf(part, "Hẹn anh Trịnh Thế Thành")
	content := layout.ModifiedParts[0].Content
	content.Format = event.FormatHTML
	content.FormattedBody = `<p>Hẹn anh Trịnh <strong>Thế Thành</strong></p>`
	wantBody, wantHTML := content.Body, content.FormattedBody
	if res := portal.sendConvertedEdit(ctx, login, "msg", "alice", layout, ghost, time.Now(), 0); !res.Success {
		t.Fatalf("layout edit = %+v", res)
	}
	sent := ghost.contents[len(ghost.contents)-1].Parsed.(*event.MessageEventContent)
	if sent.RelatesTo.GetReplaceID() != part.MXID || sent.NewContent == nil || sent.NewContent.Body != wantBody || sent.NewContent.FormattedBody != wantHTML {
		t.Fatalf("layout edit changed latest content: %+v", sent.NewContent)
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
