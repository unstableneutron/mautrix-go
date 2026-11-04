// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestMarkDeletedKeepsContent(t *testing.T) {
	const original = id.EventID("$orig")
	const marker = "🗑 Deleted in Teams · 2026-11-04 15:45 +07"
	file := &event.EncryptedFileInfo{URL: "mxc://example.com/abc"}
	for _, tc := range []struct {
		name     string
		in       event.MessageEventContent
		body     string
		html     string
		fileName string
	}{{
		name: "plain text reply",
		in: event.MessageEventContent{MsgType: event.MsgText, Body: "a < b\nok",
			RelatesTo: (&event.RelatesTo{}).SetReplyTo("$parent")},
		body: marker + "\na < b\nok",
		html: "<p><em>" + marker + "</em></p>a &lt; b<br>ok",
	}, {
		name: "formatted text",
		in:   event.MessageEventContent{MsgType: event.MsgText, Body: "hi", Format: event.FormatHTML, FormattedBody: "<b>hi</b>"},
		body: marker + "\nhi",
		html: "<p><em>" + marker + "</em></p><b>hi</b>",
	}, {
		name:     "image without caption",
		in:       event.MessageEventContent{MsgType: event.MsgImage, Body: "shot.png", File: file},
		body:     marker,
		html:     "<p><em>" + marker + "</em></p>",
		fileName: "shot.png",
	}, {
		name:     "image with caption",
		in:       event.MessageEventContent{MsgType: event.MsgImage, Body: "the cause", FileName: "shot.png", File: file},
		body:     marker + "\nthe cause",
		html:     "<p><em>" + marker + "</em></p>the cause",
		fileName: "shot.png",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			out := markDeleted(&tc.in, original, marker)
			nc := out.NewContent
			if nc == nil || out.RelatesTo.GetReplaceID() != original {
				t.Fatalf("not an edit of %s: %+v", original, out.RelatesTo)
			}
			if nc.RelatesTo != nil {
				t.Errorf("new content keeps a relation: %+v", nc.RelatesTo)
			}
			if nc.Body != tc.body || nc.FormattedBody != tc.html || nc.FileName != tc.fileName {
				t.Errorf("got body %q html %q file %q", nc.Body, nc.FormattedBody, nc.FileName)
			}
			if nc.MsgType != tc.in.MsgType || nc.File != tc.in.File {
				t.Errorf("media not kept: %s %v", nc.MsgType, nc.File)
			}
		})
	}
}
