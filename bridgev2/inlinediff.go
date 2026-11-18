// Copyright (c) 2026 unstableneutron
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
)

// RemoteEditsInlineDiff is a bridge.remote_edits value: other people's edits
// replace the message in place and show what changed inside it.
const RemoteEditsInlineDiff = "inline_diff"

// Colors of inline edit and delete markers, chosen on Beeper Desktop and iOS.
const (
	inlineDiffAdded   = "#00FF00"
	inlineDiffRemoved = "#FF6B6B"
)

// inlineDiffMaxCells bounds the line and word diffs; a bigger edit replaces
// the message without a diff.
const inlineDiffMaxCells = 250_000

// partText is the text of a message part the bridge sent, kept so a later
// edit or delete can show it although the bridge can't read its own
// encrypted events back.
type partText struct {
	Body          string `json:"body"`
	FormattedBody string `json:"formatted_body,omitempty"`
}

func (portal *Portal) partTextKey(msgID networkid.MessageID, partID networkid.PartID) database.Key {
	return database.Key("fi.mau.bridge.part_text\x1f" + string(portal.Receiver) + "\x1f" + string(portal.ID) + "\x1f" + string(msgID) + "\x1f" + string(partID))
}

// textOfContent returns the text a part shows, or false for content that has
// none to compare, such as media.
func textOfContent(content *event.MessageEventContent) (partText, bool) {
	if content == nil || content.MsgType.IsMedia() || (content.MsgType != event.MsgText && content.MsgType != event.MsgNotice && content.MsgType != event.MsgEmote) {
		return partText{}, false
	}
	text := partText{Body: content.Body}
	if content.Format == event.FormatHTML {
		text.FormattedBody = content.FormattedBody
	}
	return text, true
}

// rememberPartTexts keeps the text of other people's message parts while
// inline diffs are on, so their edits and deletes can show it.
func (portal *Portal) rememberPartTexts(ctx context.Context, source *UserLogin, senderID networkid.UserID, parts []*database.Message, converted []*ConvertedMessagePart) error {
	if portal.Bridge.Config.RemoteEdits != RemoteEditsInlineDiff {
		return nil
	}
	for _, part := range parts {
		if part.HasFakeMXID() || portal.isOwnPart(ctx, source, senderID, part) {
			continue
		}
		for _, convertedPart := range converted {
			if convertedPart.ID != part.PartID {
				continue
			}
			if text, ok := textOfContent(convertedPart.Content); ok {
				if err := portal.setPartText(ctx, part, text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (portal *Portal) isOwnPart(ctx context.Context, source *UserLogin, senderID networkid.UserID, part *database.Message) bool {
	return source != nil && (part.SenderMXID == source.UserMXID || (senderID != "" && source.Client.IsThisUser(ctx, senderID)))
}

func (portal *Portal) setPartText(ctx context.Context, part *database.Message, text partText) error {
	raw, err := json.Marshal(text)
	if err != nil {
		return err
	}
	if err = portal.setKV(ctx, portal.partTextKey(part.ID, part.PartID), string(raw)); err != nil {
		return fmt.Errorf("failed to save message part text: %w", err)
	}
	return nil
}

func (portal *Portal) getPartText(ctx context.Context, part *database.Message) (*partText, error) {
	kv := portal.Bridge.DB.KV
	var raw string
	err := kv.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id = $1 AND key = $2`, kv.BridgeID, portal.partTextKey(part.ID, part.PartID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get message part text: %w", err)
	}
	var text partText
	if err = json.Unmarshal([]byte(raw), &text); err != nil {
		return nil, fmt.Errorf("failed to parse message part text: %w", err)
	}
	return &text, nil
}

func (portal *Portal) deletePartTexts(ctx context.Context, parts []*database.Message) error {
	kv := portal.Bridge.DB.KV
	for _, part := range parts {
		if _, err := kv.Exec(ctx, `DELETE FROM kv_store WHERE bridge_id = $1 AND key = $2`, kv.BridgeID, portal.partTextKey(part.ID, part.PartID)); err != nil {
			return fmt.Errorf("failed to forget message part text: %w", err)
		}
	}
	return nil
}

// applyInlineDiff rewrites an edit's new content to show what changed since
// the kept text, then keeps the new text for the next edit. Without kept text,
// or for content without text, the edit goes out unchanged.
func (portal *Portal) applyInlineDiff(ctx context.Context, part *ConvertedEditPart) error {
	newText, ok := textOfContent(part.Content)
	if !ok {
		return nil
	}
	oldText, err := portal.getPartText(ctx, part.Part)
	if err != nil || oldText == nil {
		if err == nil {
			err = portal.setPartText(ctx, part.Part, newText)
		}
		return err
	}
	if formatted, ok := renderInlineDiff(plainText(*oldText), plainText(newText)); ok {
		part.Content.Format = event.FormatHTML
		part.Content.FormattedBody = formatted
		part.Content.Body = format.HTMLToText(formatted)
	}
	return portal.setPartText(ctx, part.Part, newText)
}

// plainText is the text of a part as the reader sees it, without formatting
// marks but with code blocks fenced so the diff can tell them apart.
func plainText(text partText) string {
	if text.FormattedBody == "" {
		return text.Body
	}
	return readerTextParser.Parse(text.FormattedBody, format.NewContext(context.Background()))
}

// readerTextParser turns Matrix HTML into the text a reader sees, keeping only
// list bullets and code fences.
var readerTextParser = &format.HTMLParser{
	TabsToSpaces:           4,
	Newline:                "\n",
	HorizontalLine:         "\n---\n",
	PillConverter:          func(displayname, _, _ string, _ format.Context) string { return displayname },
	BoldConverter:          func(s string, _ format.Context) string { return s },
	ItalicConverter:        func(s string, _ format.Context) string { return s },
	StrikethroughConverter: func(s string, _ format.Context) string { return s },
	UnderlineConverter:     func(s string, _ format.Context) string { return s },
	MonospaceConverter:     func(s string, _ format.Context) string { return s },
	SpoilerConverter:       func(s, _ string, _ format.Context) string { return s },
	ColorConverter:         func(s, _, _ string, _ format.Context) string { return s },
	LinkConverter:          func(text, _ string, _ format.Context) string { return text },
}

// deletedPartEdit is an edit of a part deleted on the remote network that
// keeps its text in red, as the only marker.
func deletedPartEdit(text partText, part *database.Message) *event.MessageEventContent {
	body, formatted := deletedContent(text)
	content := &event.MessageEventContent{
		MsgType: event.MsgText, Body: body, Format: event.FormatHTML, FormattedBody: formatted,
		Mentions: &event.Mentions{},
	}
	content.SetEdit(part.MXID)
	return content
}

// deletedContent is a whole kept message, from its kept text, in red.
func deletedContent(text partText) (body, formatted string) {
	plain := plainText(text)
	var out strings.Builder
	for _, block := range splitBlocks(strings.Split(plain, "\n")) {
		if block.code {
			out.WriteString("<pre><code>")
			for i, line := range block.lines {
				if i > 0 {
					out.WriteString("<br>")
				}
				out.WriteString(colored(inlineDiffRemoved, html.EscapeString(line)))
			}
			out.WriteString("</code></pre>")
			continue
		}
		lines := block.lines
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) == 0 {
			continue
		}
		out.WriteString("<p>")
		for i, line := range lines {
			if i > 0 {
				out.WriteString("<br>")
			}
			if line != "" {
				out.WriteString(colored(inlineDiffRemoved, html.EscapeString(line)))
			}
		}
		out.WriteString("</p>")
	}
	return plain, out.String()
}

func colored(color, escaped string) string {
	return `<span data-mx-color="` + color + `">` + escaped + `</span>`
}

type textBlock struct {
	code  bool
	lines []string
}

// splitBlocks groups lines into prose and fenced code blocks; the fences
// themselves are dropped.
func splitBlocks(lines []string) []textBlock {
	var blocks []textBlock
	inCode := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			blocks = append(blocks, textBlock{code: inCode})
			continue
		}
		if len(blocks) == 0 || blocks[len(blocks)-1].code != inCode {
			blocks = append(blocks, textBlock{code: inCode})
		}
		blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, line)
	}
	return blocks
}

type diffOp struct {
	kind byte // '=', '-', '+'
	text string
}

// diffTokens returns the edit script from a to b, or false when it would be
// too large to compute. Prose retains the most non-whitespace text, rather
// than preferring several tiny matches over a meaningful word or phrase.
func diffTokens(a, b []string, prose bool) ([]diffOp, bool) {
	equal := func(a, b string) bool {
		return a == b || prose && strings.TrimSpace(a) == "" && strings.TrimSpace(b) == ""
	}
	var ops []diffOp
	var suffix []string
	if prose {
		// Anchor the unchanged edges before aligning repeated words inside
		// a replacement. Otherwise a moved phrase can steal its own suffix.
		for len(a) > 0 && len(b) > 0 && equal(a[0], b[0]) {
			ops = append(ops, diffOp{'=', b[0]})
			a, b = a[1:], b[1:]
		}
		end := 0
		for end < min(len(a), len(b)) && equal(a[len(a)-1-end], b[len(b)-1-end]) {
			end++
		}
		suffix = b[len(b)-end:]
		a, b = a[:len(a)-end], b[:len(b)-end]
	}
	if (len(a)+1)*(len(b)+1) > inlineDiffMaxCells {
		return nil, false
	}
	weight := func(token string) int {
		if !prose {
			return 1
		}
		if strings.TrimSpace(token) == "" {
			return 0
		}
		return utf8.RuneCountInString(token)
	}
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		w := weight(a[i])
		for j := len(b) - 1; j >= 0; j-- {
			if equal(a[i], b[j]) {
				lcs[i][j] = lcs[i+1][j+1] + w
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case equal(a[i], b[j]):
			ops = append(ops, diffOp{'=', b[j]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	for _, token := range suffix {
		ops = append(ops, diffOp{'=', token})
	}
	return ops, true
}

// inlineDiffUnits groups soft-wrapped prose for alignment. Blank lines, list
// rows, and fenced code still participate in the line diff individually.
// Compare prose without layout whitespace, but render from the original units.
func inlineDiffUnits(lines []string) (units, keys []string) {
	inCode := false
	for i := 0; i < len(lines); {
		line := lines[i]
		start := i
		i++
		marker, rest := splitListMarker(line)
		key := line
		switch {
		case isFence(line):
			inCode = !inCode
		case inCode:
		case marker != "":
			// Keep list markers and indentation; only normalize the item text.
			key = marker + strings.Join(strings.Fields(rest), " ")
		case strings.TrimSpace(line) == "":
			key = ""
		default:
			for ; i < len(lines); i++ {
				marker, _ := splitListMarker(lines[i])
				if marker != "" || isFence(lines[i]) || strings.TrimSpace(lines[i]) == "" {
					break
				}
			}
			line = strings.Join(lines[start:i], "\n")
			key = strings.Join(strings.Fields(line), " ")
		}
		units = append(units, line)
		keys = append(keys, key)
	}
	return units, keys
}

// renderInlineDiff renders the new text with the changes since the old one
// marked: struck red removals and green additions in prose, and −/+ lines
// inside code blocks. It reports false when only prose layout changed or the
// diff would be too large, so the edit replaces the message without markers.
func renderInlineDiff(oldText, newText string) (string, bool) {
	if oldText == newText {
		return "", false
	}
	oldLines, newLines := strings.Split(oldText, "\n"), strings.Split(newText, "\n")
	if (len(oldLines)+1)*(len(newLines)+1) > inlineDiffMaxCells {
		return "", false
	}
	oldUnits, oldKeys := inlineDiffUnits(oldLines)
	newUnits, newKeys := inlineDiffUnits(newLines)
	if slices.Equal(oldKeys, newKeys) {
		return "", false
	}
	lineOps, ok := diffTokens(oldKeys, newKeys, false)
	if !ok {
		return "", false
	}
	oldIndex, newIndex := 0, 0
	for i := range lineOps {
		if lineOps[i].kind == '-' {
			lineOps[i].text = oldUnits[oldIndex]
		} else {
			lineOps[i].text = newUnits[newIndex]
			newIndex++
		}
		if lineOps[i].kind != '+' {
			oldIndex++
		}
	}
	var out strings.Builder
	inCode, open := false, ""
	openBlock := func(code bool) {
		want := "p"
		if code {
			want = "pre"
		}
		if open == want {
			out.WriteString("<br>")
			return
		}
		closeBlock(&out, open)
		if code {
			out.WriteString("<pre><code>")
		} else {
			out.WriteString("<p>")
		}
		open = want
	}
	for k := 0; k < len(lineOps); k++ {
		op := lineOps[k]
		if strings.HasPrefix(strings.TrimSpace(op.text), "```") {
			// A fence toggles the block for both versions only when it is kept.
			if op.kind == '=' {
				closeBlock(&out, open)
				open = ""
				inCode = !inCode
			}
			continue
		}
		if inCode {
			openBlock(true)
			escaped := html.EscapeString(op.text)
			switch op.kind {
			case '-':
				out.WriteString(colored(inlineDiffRemoved, "-"+escaped))
			case '+':
				out.WriteString(colored(inlineDiffAdded, "+"+escaped))
			default:
				out.WriteString(" " + escaped)
			}
			continue
		}
		// A run of removed lines followed by added ones is a changed
		// paragraph: diff it by words so a typo marks only the word.
		if op.kind == '-' {
			removed := []string{}
			for k < len(lineOps) && lineOps[k].kind == '-' && !isFence(lineOps[k].text) {
				removed = append(removed, lineOps[k].text)
				k++
			}
			added := []string{}
			for k < len(lineOps) && lineOps[k].kind == '+' && !isFence(lineOps[k].text) {
				added = append(added, lineOps[k].text)
				k++
			}
			k--
			for n := 0; n < max(len(removed), len(added)); n++ {
				switch {
				case n >= len(added):
					openBlock(false)
					out.WriteString(struck(removed[n]))
				case n >= len(removed):
					openBlock(false)
					out.WriteString(addedLine(added[n]))
				case similarLines(removed[n], added[n]):
					openBlock(false)
					out.WriteString(wordDiff(removed[n], added[n]))
				default:
					// A replaced line, such as a list item swapped for another:
					// the old one struck, the new one added below it.
					openBlock(false)
					out.WriteString(struck(removed[n]))
					openBlock(false)
					out.WriteString(addedLine(added[n]))
				}
			}
			continue
		}
		openBlock(false)
		if op.kind == '+' {
			out.WriteString(addedLine(op.text))
		} else {
			out.WriteString(escapedProse(op.text))
		}
	}
	closeBlock(&out, open)
	return out.String(), true
}

// struck is a removed line, struck through in red after its list marker.
func struck(line string) string {
	marker, rest := splitListMarker(line)
	return html.EscapeString(marker) + colored(inlineDiffRemoved, "<s>"+escapedProse(rest)+"</s>")
}

// addedLine is an added line, in green with a + after its list marker.
func addedLine(line string) string {
	marker, rest := splitListMarker(line)
	return html.EscapeString(marker) + colored(inlineDiffAdded, "+ "+escapedProse(rest))
}

func escapedProse(text string) string {
	return strings.ReplaceAll(html.EscapeString(text), "\n", "<br>")
}

// splitListMarker splits a list bullet or number, with its indentation, off a line.
func splitListMarker(line string) (marker, rest string) {
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	for _, bullet := range []string{"* ", "- ", "• "} {
		if strings.HasPrefix(trimmed, bullet) {
			return line[:indent+len(bullet)], trimmed[len(bullet):]
		}
	}
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits > 0 && strings.HasPrefix(trimmed[digits:], ". ") {
		end := digits + 2
		for end < len(trimmed) && trimmed[end] == ' ' {
			end++
		}
		return line[:indent+end], trimmed[end:]
	}
	return "", line
}

// similarLines reports whether two lines share most of their tokens, so the
// newer one is an edit of the older rather than a different line.
func similarLines(oldLine, newLine string) bool {
	a, b := splitWords(oldLine), splitWords(newLine)
	ops, ok := diffTokens(a, b, true)
	if !ok {
		return false
	}
	same := 0
	for _, op := range ops {
		if op.kind == '=' && strings.TrimSpace(op.text) != "" {
			same++
		}
	}
	count := func(tokens []string) int {
		count := 0
		for _, token := range tokens {
			if strings.TrimSpace(token) != "" {
				count++
			}
		}
		return count
	}
	return 2*same >= max(count(a), count(b))
}

func isFence(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

func closeBlock(out *strings.Builder, open string) {
	switch open {
	case "p":
		out.WriteString("</p>")
	case "pre":
		out.WriteString("</code></pre>")
	}
}

// wordDiff marks the words that changed between two versions of a line.
func wordDiff(oldLine, newLine string) string {
	ops, ok := diffTokens(splitWords(oldLine), splitWords(newLine), true)
	if !ok {
		return colored(inlineDiffRemoved, "<s>"+html.EscapeString(oldLine)+"</s>") + " " + colored(inlineDiffAdded, html.EscapeString(newLine))
	}
	var out strings.Builder
	for k := 0; k < len(ops); k++ {
		var run strings.Builder
		kind := ops[k].kind
		for k < len(ops) && ops[k].kind == kind {
			run.WriteString(ops[k].text)
			k++
		}
		k--
		text := run.String()
		switch kind {
		case '-', '+':
			if kind == '-' {
				// Old soft wraps should not introduce breaks in the latest layout.
				text = strings.ReplaceAll(text, "\n", " ")
			}
			// Keep surrounding whitespace outside the colored text.
			trimmed := strings.TrimSpace(text)
			if trimmed == "" {
				// A whitespace-only replacement shows the latest spacing,
				// not both versions (notably NBSP versus regular space).
				if kind == '+' {
					out.WriteString(escapedProse(text))
				}
				continue
			}
			leading := text[:len(text)-len(strings.TrimLeftFunc(text, unicode.IsSpace))]
			trailing := text[len(strings.TrimRightFunc(text, unicode.IsSpace)):]
			out.WriteString(escapedProse(leading))
			if kind == '-' {
				out.WriteString(colored(inlineDiffRemoved, "<s>"+html.EscapeString(trimmed)+"</s>"))
			} else {
				out.WriteString(colored(inlineDiffAdded, escapedProse(trimmed)))
			}
			out.WriteString(escapedProse(trailing))
			if kind == '-' && trailing == "" && k+1 < len(ops) && ops[k+1].kind == '+' {
				out.WriteString(" ")
			}
		default:
			out.WriteString(escapedProse(text))
		}
	}
	return out.String()
}

// splitWords keeps whitespace and punctuation separate from words, so a
// changed comma or trailing space doesn't mark the adjacent word. Letters
// and combining marks stay together, including decomposed Vietnamese text.
func splitWords(line string) []string {
	var words []string
	start := 0
	previous := -1
	for i, r := range line {
		kind := 0
		if unicode.IsSpace(r) {
			kind = 1
		} else if unicode.IsPunct(r) {
			kind = 2
		} else if unicode.IsMark(r) && previous >= 0 {
			kind = previous
		}
		if previous >= 0 && (kind != previous || unicode.IsPunct(r)) {
			words = append(words, line[start:i])
			start = i
		}
		previous = kind
	}
	if start < len(line) {
		words = append(words, line[start:])
	}
	return words
}
