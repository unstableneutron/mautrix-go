// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package commands

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/event"
)

func TestRawTextToEventWhitespace(t *testing.T) {
	for _, input := range []string{
		"!SEND first  second\nthird ",
		"  !SEND first  second\nthird ",
		"\t!SEND\tfirst  second\nthird ",
		"\n!SEND\nfirst  second\nthird ",
		"\u2003!SEND\u2003first  second\nthird ",
	} {
		t.Run(input, func(t *testing.T) {
			parsed := RawTextToEvent[struct{}](context.Background(), nil, input)
			assert.Equal(t, input, parsed.RawInput)
			assert.Equal(t, "!send", parsed.Command)
			assert.Equal(t, []string{"first", "second", "third"}, parsed.Args)
			assert.Equal(t, "first  second\nthird ", parsed.RawArgs)
		})
	}
	for _, input := range []string{"", " \t\n\u2003"} {
		parsed := RawTextToEvent[struct{}](context.Background(), nil, input)
		assert.Empty(t, parsed.Command)
		assert.Empty(t, parsed.Args)
		assert.Empty(t, parsed.RawArgs)
	}
}

func TestProcessorWhitespaceSubcommands(t *testing.T) {
	for _, prefixCommand := range []bool{false, true} {
		for _, separator := range []string{" ", "\t", "\n", "\u2003"} {
			t.Run(separator, func(t *testing.T) {
				proc := NewProcessor[struct{}](nil)
				command := "!ROOM"
				if prefixCommand {
					proc.PreValidator = ValidatePrefixCommand[struct{}]("!bot")
					command = "!BOT" + separator + "ROOM"
				}
				called := false
				proc.Register(&Handler[struct{}]{Name: "room", Subcommands: []*Handler[struct{}]{
					{Name: "send", Func: func(ce *Event[struct{}]) {
						called = true
						assert.Equal(t, "send", ce.Command)
						require.Equal(t, []string{"first", "second", "third"}, ce.Args)
						assert.Equal(t, "first  second\nthird ", ce.RawArgs)
						assert.Equal(t, "first", ce.ShiftArg())
						assert.Equal(t, "second\nthird ", ce.RawArgs)
						assert.Equal(t, "second", ce.ShiftArg())
						assert.Equal(t, "third ", ce.RawArgs)
						assert.Equal(t, "third", ce.ShiftArg())
						assert.Empty(t, ce.RawArgs)
						assert.Empty(t, ce.ShiftArg())
					}},
				}})
				input := "  " + command + separator + "SEND" + separator + "first  second\nthird "
				proc.Process(context.Background(), &event.Event{
					Type:    event.EventMessage,
					Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: input}},
				})
				assert.True(t, called, "subcommand handler should run")
			})
		}
	}
}
