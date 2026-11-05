// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package mautrix_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestDefaultSyncerStateAfterOrderingAndMetadata(t *testing.T) {
	syncer := mautrix.NewDefaultSyncer()
	syncer.ParseEventContent = false
	roomID := id.RoomID("!room:example.org")
	oldStateKey, newStateKey := "old", "new"
	oldState := &event.Event{ID: "$old", Type: event.StateTopic, StateKey: &oldStateKey}
	timeline := &event.Event{ID: "$timeline", Type: event.EventMessage}
	stateAfter := &event.Event{ID: "$state-after", Type: event.StateTopic, StateKey: &newStateKey}

	type observedEvent struct {
		id        id.EventID
		typeClass event.TypeClass
		source    event.Source
		ignore    bool
		roomID    id.RoomID
	}
	var got []observedEvent
	syncer.OnEvent(func(_ context.Context, evt *event.Event) {
		got = append(got, observedEvent{
			id: evt.ID, typeClass: evt.Type.Class, source: evt.Mautrix.EventSource,
			ignore: evt.Mautrix.IgnoreState, roomID: evt.RoomID,
		})
	})

	err := syncer.ProcessResponse(context.Background(), &mautrix.RespSync{
		Rooms: mautrix.RespSyncRooms{Join: map[id.RoomID]*mautrix.SyncJoinedRoom{
			roomID: {
				State:      mautrix.SyncEventsList{Events: []*event.Event{oldState}},
				Timeline:   mautrix.SyncTimeline{SyncEventsList: mautrix.SyncEventsList{Events: []*event.Event{timeline}}},
				StateAfter: &mautrix.SyncEventsList{Events: []*event.Event{stateAfter}},
			},
		}},
	}, "since-token")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []id.EventID{"$timeline", "$state-after"}, []id.EventID{got[0].id, got[1].id})
	assert.Equal(t, []event.TypeClass{event.MessageEventType, event.StateEventType}, []event.TypeClass{got[0].typeClass, got[1].typeClass})
	assert.Equal(t, []event.Source{event.SourceJoin | event.SourceTimeline, event.SourceJoin | event.SourceState}, []event.Source{got[0].source, got[1].source})
	assert.Equal(t, []bool{true, false}, []bool{got[0].ignore, got[1].ignore})
	assert.Equal(t, roomID, got[0].roomID)
	assert.Equal(t, roomID, got[1].roomID)
}

func TestDefaultSyncerMalformedKnownAndUnknownEvents(t *testing.T) {
	syncer := mautrix.NewDefaultSyncer()
	roomID := id.RoomID("!room:example.org")
	knownMalformed := &event.Event{
		ID: "$malformed-known", Type: event.EventMessage,
		Content: event.Content{VeryRaw: json.RawMessage("{")},
	}
	unknown := &event.Event{
		ID: "$unknown", Type: event.Type{Type: "org.example.unknown", Class: event.MessageEventType},
		Content: event.Content{VeryRaw: json.RawMessage("{")},
	}
	var globalEvents []*event.Event
	var typedEvents []*event.Event
	syncer.OnEvent(func(_ context.Context, evt *event.Event) { globalEvents = append(globalEvents, evt) })
	unknownAccountDataType := event.Type{Type: "org.example.unknown", Class: event.AccountDataEventType}
	syncer.OnEventType(unknownAccountDataType, func(_ context.Context, evt *event.Event) {
		typedEvents = append(typedEvents, evt)
	})

	err := syncer.ProcessResponse(context.Background(), &mautrix.RespSync{
		Rooms: mautrix.RespSyncRooms{Join: map[id.RoomID]*mautrix.SyncJoinedRoom{
			roomID: {Timeline: mautrix.SyncTimeline{SyncEventsList: mautrix.SyncEventsList{Events: []*event.Event{knownMalformed}}}},
		}},
		AccountData: mautrix.SyncEventsList{Events: []*event.Event{unknown}},
	}, "since-token")
	require.NoError(t, err)
	require.Len(t, globalEvents, 1)
	require.Len(t, typedEvents, 1)
	assert.Same(t, unknown, globalEvents[0])
	assert.Same(t, unknown, typedEvents[0])
	assert.Equal(t, unknownAccountDataType, unknown.Type)
	assert.Equal(t, event.SourceAccountData, unknown.Mautrix.EventSource)
	assert.Equal(t, roomID, knownMalformed.RoomID)
}

func TestDefaultSyncerProcessResponseStopsAndRecoversPanics(t *testing.T) {
	t.Run("sync handler stops event processing", func(t *testing.T) {
		syncer := mautrix.NewDefaultSyncer()
		syncer.ParseEventContent = false
		called := false
		syncer.OnSync(func(context.Context, *mautrix.RespSync, string) bool { return false })
		syncer.OnEvent(func(context.Context, *event.Event) { called = true })
		err := syncer.ProcessResponse(context.Background(), &mautrix.RespSync{
			ToDevice: mautrix.SyncEventsList{Events: []*event.Event{{Type: event.ToDeviceDummy}}},
		}, "since-token")
		require.NoError(t, err)
		assert.False(t, called)
	})

	t.Run("listener panic becomes an error", func(t *testing.T) {
		syncer := mautrix.NewDefaultSyncer()
		syncer.ParseEventContent = false
		syncer.OnEvent(func(context.Context, *event.Event) { panic("listener failed") })
		err := syncer.ProcessResponse(context.Background(), &mautrix.RespSync{
			ToDevice: mautrix.SyncEventsList{Events: []*event.Event{{Type: event.ToDeviceDummy}}},
		}, "since-token")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ProcessResponse panicked!")
		assert.Contains(t, err.Error(), "since=since-token")
		assert.Contains(t, err.Error(), "listener failed")
	})
}
