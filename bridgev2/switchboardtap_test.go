// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/event"
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
