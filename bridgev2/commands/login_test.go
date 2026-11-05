// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type attachmentMatrixAPI struct {
	bridgev2.MatrixAPI
	uploadErr error
	sendErr   error
	sent      int
}

func (api *attachmentMatrixAPI) UploadMedia(context.Context, id.RoomID, []byte, string, string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	return "mxc://example.com/attachment", nil, api.uploadErr
}

func (api *attachmentMatrixAPI) SendMessage(context.Context, id.RoomID, event.Type, *event.Content, *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	api.sent++
	return &mautrix.RespSendEvent{EventID: "$attachment"}, api.sendErr
}

func TestSendUserInputAttachments(t *testing.T) {
	failure := errors.New("delivery failed")
	for _, tc := range []struct {
		name      string
		filename  string
		uploadErr error
		sendErr   error
		wantErr   string
		wantSent  int
	}{
		{name: "success", filename: "captcha.png", wantSent: 2},
		{name: "missing filename", wantErr: "missing attachment filename"},
		{name: "upload failure", filename: "captcha.png", uploadErr: failure, wantErr: `failed to upload attachment "captcha.png"`},
		{name: "send failure", filename: "captcha.png", sendErr: failure, wantErr: `failed to send attachment "captcha.png"`, wantSent: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &attachmentMatrixAPI{uploadErr: tc.uploadErr, sendErr: tc.sendErr}
			ce := &Event{Ctx: context.Background(), Bot: api, RoomID: "!room:example.com"}
			attachment := &bridgev2.LoginUserInputAttachment{FileName: tc.filename, Type: event.MsgImage}
			err := sendUserInputAttachments(ce, []*bridgev2.LoginUserInputAttachment{attachment, attachment})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				if tc.uploadErr != nil || tc.sendErr != nil {
					assert.ErrorIs(t, err, failure)
				}
			}
			assert.Equal(t, tc.wantSent, api.sent)
		})
	}
}
