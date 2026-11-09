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
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The Switchboard bridge tap publishes this bridge's remote messages and `!sb` commands to NATS,
// and serves a narrow set of Matrix-side actions (silent annotation edits, bot notices) in an
// allowlist of rooms. It is off unless SWITCHBOARD_TAP_NATS_URL is set:
//
//	SWITCHBOARD_TAP_NATS_URL   nats://host:4222
//	SWITCHBOARD_TAP_NAME       subject token for this bridge, e.g. sh-zalo
//	SWITCHBOARD_TAP_RPC_ROOMS  comma-separated room IDs the actions may touch; empty serves none
//
// Events go to sb.tap.bridge.<name>.{message.created,message.edited,cmd,portals}; actions are
// served on sb.rpc.bridge.<name>.matrix.{annotate,notice}. Every publish carries Nats-Msg-Id.

// SwitchboardNotesKey holds Switchboard's notes (e.g. translations) and the original content.
const SwitchboardNotesKey = "app.0x12.switchboard.notes"

// switchboardNoteMarker (an invisible separator) starts the notes in the shown text, also when a
// client copies it; text from it on is never bridged out.
const switchboardNoteMarker = "\u2063"

// switchboardOriginalColor greys the original above the divider, as iMessage shows translations.
const switchboardOriginalColor = "#8e8e93"

var switchboardCommand = regexp.MustCompile(`^!sb(\s|$)`)
var switchboardNoteHTML = regexp.MustCompile(`(?s)<(hr|p) data-sb-note.*$`)
var switchboardOriginalHTML = regexp.MustCompile(`(?s)^<font data-sb-original[^>]*>(.*)</font>$`)

type SwitchboardTap struct {
	br       *Bridge
	nc       *nats.Conn
	name     string
	network  string
	rpcRooms map[id.RoomID]bool
	log      zerolog.Logger

	cacheLock  sync.Mutex
	cache      map[id.EventID]*event.MessageEventContent
	cacheOrder []id.EventID
}

const switchboardCacheSize = 2000

func startSwitchboardTap(ctx context.Context, br *Bridge) *SwitchboardTap {
	url := os.Getenv("SWITCHBOARD_TAP_NATS_URL")
	if url == "" {
		return nil
	}
	log := br.Log.With().Str("component", "switchboard tap").Logger()
	name := os.Getenv("SWITCHBOARD_TAP_NAME")
	if name == "" {
		name = br.Network.GetName().NetworkID
	}
	t := &SwitchboardTap{
		br:       br,
		name:     subjectToken(name),
		network:  subjectToken(br.Network.GetName().NetworkID),
		rpcRooms: map[id.RoomID]bool{},
		log:      log,
		cache:    map[id.EventID]*event.MessageEventContent{},
	}
	for _, room := range strings.Split(os.Getenv("SWITCHBOARD_TAP_RPC_ROOMS"), ",") {
		if room = strings.TrimSpace(room); room != "" {
			t.rpcRooms[id.RoomID(room)] = true
		}
	}
	nc, err := nats.Connect(url,
		nats.Name("bridge-tap-"+t.name),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.ConnectHandler(func(*nats.Conn) { go t.publishPortals(context.WithoutCancel(ctx)) }),
		nats.ReconnectHandler(func(*nats.Conn) { go t.publishPortals(context.WithoutCancel(ctx)) }),
	)
	if err != nil {
		log.Err(err).Msg("Failed to set up NATS connection, tap disabled")
		return nil
	}
	t.nc = nc
	if len(t.rpcRooms) > 0 {
		for op, handler := range map[string]func(context.Context, *switchboardRPC) (any, error){
			"matrix.annotate": t.annotate,
			"matrix.notice":   t.notice,
		} {
			if _, err = nc.QueueSubscribe(t.rpcSubject(op), "bridge-tap", t.serve(ctx, handler)); err != nil {
				log.Err(err).Str("op", op).Msg("Failed to subscribe to action subject")
			}
		}
	}
	log.Info().Str("name", t.name).Int("rpc_rooms", len(t.rpcRooms)).Msg("Switchboard tap started")
	return t
}

func subjectToken(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r == '*' || r == '>' || r <= ' ' {
			return '_'
		}
		return r
	}, s)
}

func (t *SwitchboardTap) rpcSubject(op string) string {
	return "sb.rpc.bridge." + t.name + "." + op
}

func (t *SwitchboardTap) stop() {
	if t != nil && t.nc != nil {
		_ = t.nc.Drain()
	}
}

func (t *SwitchboardTap) publish(kind, msgID string, payload map[string]any) {
	payload["v"] = 1
	payload["kind"] = kind
	payload["bridge"] = t.name
	payload["network"] = t.network
	data, err := json.Marshal(payload)
	if err != nil {
		t.log.Err(err).Str("kind", kind).Msg("Failed to marshal tap event")
		return
	}
	msg := nats.NewMsg("sb.tap.bridge." + t.name + "." + kind)
	msg.Data = data
	if msgID != "" {
		msg.Header.Set(nats.MsgIdHdr, msgID)
	}
	if err = t.nc.PublishMsg(msg); err != nil {
		t.log.Warn().Err(err).Str("kind", kind).Msg("Failed to publish tap event")
	}
}

func (t *SwitchboardTap) publishPortals(ctx context.Context) {
	portals, err := t.br.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		t.log.Err(err).Msg("Failed to list portals")
		return
	}
	rooms := make([]id.RoomID, 0, len(portals))
	for _, portal := range portals {
		rooms = append(rooms, portal.MXID)
	}
	t.publish("portals", "", map[string]any{"rooms": rooms, "ts": time.Now().UnixMilli()})
}

func (t *SwitchboardTap) remember(eventID id.EventID, content *event.MessageEventContent) {
	t.cacheLock.Lock()
	defer t.cacheLock.Unlock()
	if _, ok := t.cache[eventID]; !ok {
		t.cacheOrder = append(t.cacheOrder, eventID)
		if len(t.cacheOrder) > switchboardCacheSize {
			delete(t.cache, t.cacheOrder[0])
			t.cacheOrder = t.cacheOrder[1:]
		}
	}
	t.cache[eventID] = content
}

func (t *SwitchboardTap) recall(eventID id.EventID) *event.MessageEventContent {
	t.cacheLock.Lock()
	defer t.cacheLock.Unlock()
	return t.cache[eventID]
}

func tapContent(content *event.MessageEventContent) map[string]any {
	out := map[string]any{"msgtype": content.MsgType, "body": content.Body}
	if content.FormattedBody != "" {
		out["format"] = content.Format
		out["formatted_body"] = content.FormattedBody
	}
	return out
}

// messageBridged is called after a remote message was sent to Matrix.
func (t *SwitchboardTap) messageBridged(portal *Portal, sender EventSender, parts []*database.Message, converted *ConvertedMessage) {
	if t == nil {
		return
	}
	for _, msg := range parts {
		if msg.HasFakeMXID() {
			continue
		}
		for _, part := range converted.Parts {
			if part.ID != msg.PartID || part.Content == nil {
				continue
			}
			t.remember(msg.MXID, part.Content)
			t.publish("message.created", fmt.Sprintf("bridge:%s:%s:created", t.name, msg.MXID), map[string]any{
				"room_id":    portal.MXID,
				"event_id":   msg.MXID,
				"chat_title": portal.Name,
				"sender":     map[string]any{"mxid": msg.SenderMXID, "remote_id": msg.SenderID, "from_me": sender.IsFromMe},
				"content":    tapContent(part.Content),
				"remote":     map[string]any{"message_id": msg.ID, "part_id": msg.PartID, "portal_id": portal.ID},
				"ts":         msg.Timestamp.UnixMilli(),
			})
		}
	}
}

// messageEdited is called after a remote edit was sent to Matrix.
func (t *SwitchboardTap) messageEdited(portal *Portal, sender EventSender, converted *ConvertedEdit, ts time.Time) {
	if t == nil {
		return
	}
	for _, part := range converted.ModifiedParts {
		if part.Part == nil || part.Part.HasFakeMXID() || part.Content == nil {
			continue
		}
		t.remember(part.Part.MXID, part.Content)
		t.publish("message.edited", fmt.Sprintf("bridge:%s:%s:edited:%d", t.name, part.Part.MXID, ts.UnixMilli()), map[string]any{
			"room_id":    portal.MXID,
			"event_id":   part.Part.MXID,
			"chat_title": portal.Name,
			"sender":     map[string]any{"mxid": part.Part.SenderMXID, "remote_id": part.Part.SenderID, "from_me": sender.IsFromMe},
			"content":    tapContent(part.Content),
			"remote":     map[string]any{"message_id": part.Part.ID, "part_id": part.Part.PartID, "portal_id": portal.ID},
			"ts":         ts.UnixMilli(),
		})
	}
}

// interceptCommand publishes a `!sb …` typed in a bridged room. It is never bridged to the network.
func (t *SwitchboardTap) interceptCommand(ctx context.Context, evt *event.Event, sender *User, body string) bool {
	if t == nil || !switchboardCommand.MatchString(body) {
		return false
	}
	title := ""
	if portal, err := t.br.GetPortalByMXID(ctx, evt.RoomID); err == nil && portal != nil {
		title = portal.Name
	}
	t.publish("cmd", fmt.Sprintf("bridge:%s:%s:cmd", t.name, evt.ID), map[string]any{
		"room_id":    evt.RoomID,
		"event_id":   evt.ID,
		"chat_title": title,
		"sender":     map[string]any{"mxid": sender.MXID, "admin": sender.Permissions.Admin},
		"text":       body,
		"ts":         evt.Timestamp,
	})
	return true
}

// SanitizeSwitchboardNotes removes Switchboard notes from content going out to a network, so
// translations never reach a contact through a quote, forward or edit copied from an annotated message.
func SanitizeSwitchboardNotes(content *event.MessageEventContent, raw map[string]any) bool {
	if content == nil {
		return false
	}
	changed := false
	if notes, ok := raw[SwitchboardNotesKey].(map[string]any); ok {
		if orig, ok := notes["original"].(map[string]any); ok {
			if body, ok := orig["body"].(string); ok {
				content.Body = body
				content.FormattedBody, _ = orig["formatted_body"].(string)
				if content.FormattedBody == "" {
					content.Format = ""
				}
				changed = true
			}
		}
		delete(raw, SwitchboardNotesKey)
	}
	if idx := strings.Index(content.Body, switchboardNoteMarker); idx >= 0 {
		content.Body = strings.TrimRight(content.Body[:idx], " \n")
		changed = true
	}
	if content.FormattedBody != "" {
		cut := switchboardNoteHTML.ReplaceAllString(content.FormattedBody, "")
		if idx := strings.Index(cut, switchboardNoteMarker); idx >= 0 {
			cut = cut[:idx]
		}
		cut = switchboardOriginalHTML.ReplaceAllString(strings.TrimRight(cut, " \n"), "$1")
		if cut != content.FormattedBody {
			content.FormattedBody = strings.TrimRight(cut, " \n")
			changed = true
		}
	}
	return changed
}

type switchboardRPC struct {
	RoomID  id.RoomID         `json:"room_id"`
	EventID id.EventID        `json:"event_id"`
	TxnID   string            `json:"txn_id"`
	Text    string            `json:"text"`
	Notes   []switchboardNote `json:"notes"`
}

type switchboardNote struct {
	Kind       string `json:"kind"`
	Lang       string `json:"lang,omitempty"`
	SourceLang string `json:"source_lang,omitempty"`
	Text       string `json:"text"`
}

func (t *SwitchboardTap) serve(ctx context.Context, handler func(context.Context, *switchboardRPC) (any, error)) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req switchboardRPC
		var res any
		err := json.Unmarshal(msg.Data, &req)
		if err == nil && !t.rpcRooms[req.RoomID] {
			err = fmt.Errorf("room %s is not in SWITCHBOARD_TAP_RPC_ROOMS", req.RoomID)
		} else if err == nil && req.TxnID == "" {
			err = fmt.Errorf("txn_id is required")
		}
		if err == nil {
			res, err = handler(ctx, &req)
		}
		reply := map[string]any{"ok": err == nil, "result": res}
		if err != nil {
			reply["error"] = err.Error()
			t.log.Warn().Err(err).Str("subject", msg.Subject).Stringer("room_id", req.RoomID).Msg("Switchboard action failed")
		} else {
			t.log.Debug().Str("subject", msg.Subject).Stringer("room_id", req.RoomID).Msg("Switchboard action done")
		}
		data, _ := json.Marshal(reply)
		_ = msg.Respond(data)
	}
}

// annotate silently edits a bridged message, as its sender, to show notes under the original text.
func (t *SwitchboardTap) annotate(ctx context.Context, req *switchboardRPC) (any, error) {
	if len(req.Notes) == 0 {
		return nil, fmt.Errorf("no notes")
	}
	portal, err := t.br.GetPortalByMXID(ctx, req.RoomID)
	if err != nil || portal == nil {
		return nil, fmt.Errorf("not a portal room: %v", err)
	}
	part, err := t.br.DB.Message.GetPartByMXID(ctx, req.EventID)
	if err != nil || part == nil || part.Room != portal.PortalKey {
		return nil, fmt.Errorf("message not found in this room: %v", err)
	}
	orig := t.recall(req.EventID)
	if orig == nil {
		return nil, fmt.Errorf("original content not cached (bridged before the last restart)")
	}
	intent, err := portal.getIntentForMXID(ctx, part.SenderMXID)
	if err != nil || intent == nil {
		return nil, fmt.Errorf("no intent for sender %s: %v", part.SenderMXID, err)
	}
	newContent := buildAnnotatedContent(orig, req.Notes)
	resp, err := intent.SendMessage(ctx, req.RoomID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType:   orig.MsgType,
			Body:      "* " + newContent["body"].(string),
			Mentions:  &event.Mentions{},
			RelatesTo: (&event.RelatesTo{}).SetReplace(req.EventID),
		},
		Raw: map[string]any{
			"m.new_content":                 newContent,
			"com.beeper.dont_render_edited": true,
		},
	}, &MatrixSendExtra{Timestamp: time.Now(), TransactionID: "sb-" + req.TxnID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"event_id": resp.EventID}, nil
}

func buildAnnotatedContent(orig *event.MessageEventContent, notes []switchboardNote) map[string]any {
	html := orig.FormattedBody
	if html == "" {
		html = event.TextToHTML(orig.Body)
	}
	// Like iMessage: the original greyed, a divider, then the translation.
	html = fmt.Sprintf(`<font data-sb-original color="%[1]s" data-mx-color="%[1]s">%[2]s</font><hr data-sb-note>`, switchboardOriginalColor, html)
	body := orig.Body
	items := make([]map[string]any, 0, len(notes))
	for _, note := range notes {
		body += "\n\n" + switchboardNoteMarker + "🌐 " + note.Text
		html += fmt.Sprintf(`<p data-sb-note="%s" lang="%s">%s<em>%s</em></p>`,
			event.TextToHTML(note.Kind), event.TextToHTML(note.Lang), switchboardNoteMarker, event.TextToHTML(note.Text))
		items = append(items, map[string]any{"kind": note.Kind, "lang": note.Lang, "source_lang": note.SourceLang, "text": note.Text})
	}
	original := map[string]any{"body": orig.Body}
	if orig.FormattedBody != "" {
		original["formatted_body"] = orig.FormattedBody
	}
	return map[string]any{
		"msgtype":           orig.MsgType,
		"body":              body,
		"format":            event.FormatHTML,
		"formatted_body":    html,
		"m.mentions":        map[string]any{},
		SwitchboardNotesKey: map[string]any{"v": 1, "original": original, "items": items},
	}
}

// notice sends a bot notice (grey system text) to an allowlisted portal room.
func (t *SwitchboardTap) notice(ctx context.Context, req *switchboardRPC) (any, error) {
	if req.Text == "" {
		return nil, fmt.Errorf("no text")
	}
	resp, err := t.br.Bot.SendMessage(ctx, req.RoomID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: req.Text, Mentions: &event.Mentions{}},
	}, &MatrixSendExtra{Timestamp: time.Now(), TransactionID: "sb-" + req.TxnID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"event_id": resp.EventID}, nil
}
