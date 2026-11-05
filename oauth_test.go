package mautrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/oauth"
)

func TestRequestOAuthRefresh(t *testing.T) {
	saveErr := errors.New("token storage unavailable")
	for _, tc := range []struct {
		name         string
		refreshToken string
		status       int
		saveErr      error
	}{
		{name: "rotated refresh token", refreshToken: "rotated-refresh", status: http.StatusOK},
		{name: "omitted refresh token", status: http.StatusOK},
		{name: "persistence failure", refreshToken: "rotated-refresh", status: http.StatusOK, saveErr: saveErr},
		{name: "rejected refresh", status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refreshCalls, apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					refreshCalls.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Empty(t, r.Header.Get("Authorization"))
					assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
					if assert.NoError(t, r.ParseForm()) {
						assert.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
						assert.Equal(t, "old-refresh", r.PostForm.Get("refresh_token"))
						assert.Equal(t, "client-id", r.PostForm.Get("client_id"))
					}
					w.WriteHeader(tc.status)
					if tc.status != http.StatusOK {
						_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
					} else {
						response := map[string]any{"access_token": "new-access", "expires_in": 3600, "token_type": "Bearer"}
						if tc.refreshToken != "" {
							response["refresh_token"] = tc.refreshToken
						}
						assert.NoError(t, json.NewEncoder(w).Encode(response))
					}
					return
				}
				apiCalls.Add(1)
				assert.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
				_, _ = fmt.Fprint(w, `{"ok":true}`)
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server.URL)
			client.DefaultHTTPRetries = 0
			client.OAuthSetServerMetadata(&oauth.ServerMetadata{TokenEndpoint: server.URL + "/token"})
			oldExpiry := time.Now().Add(-time.Minute)
			client.OAuthSetTokens("client-id", "old-refresh", "old-access", oldExpiry)
			start := time.Now()
			saveCalls := 0
			client.SaveNewToken = func(ctx context.Context, refreshToken, accessToken string, expiry time.Time) error {
				saveCalls++
				wantRefresh := tc.refreshToken
				if wantRefresh == "" {
					wantRefresh = "old-refresh"
				}
				assert.Equal(t, wantRefresh, refreshToken)
				assert.Equal(t, "new-access", accessToken)
				assert.False(t, expiry.Before(start.Add(time.Hour)))
				assert.False(t, expiry.After(time.Now().Add(time.Hour)))
				// Persist rotated credentials before replacing the usable in-memory pair.
				assert.Equal(t, "old-access", client.AccessToken)
				assert.Equal(t, "old-refresh", client.refreshToken)
				return tc.saveErr
			}

			var response struct {
				OK bool `json:"ok"`
			}
			_, err := client.MakeRequest(t.Context(), http.MethodGet, server.URL+"/api", nil, &response)
			require.EqualValues(t, 1, refreshCalls.Load())
			if tc.saveErr != nil || tc.status != http.StatusOK {
				require.ErrorIs(t, err, ErrFailedToRefreshToken)
				if tc.saveErr != nil {
					require.ErrorIs(t, err, saveErr)
				} else {
					var httpErr HTTPError
					require.ErrorAs(t, err, &httpErr)
					require.True(t, httpErr.IsStatus(http.StatusBadRequest))
				}
				require.Zero(t, apiCalls.Load())
				require.Equal(t, "old-access", client.AccessToken)
				require.Equal(t, "old-refresh", client.refreshToken)
				require.Equal(t, oldExpiry, client.accessTokenExpiry)
			} else {
				require.NoError(t, err)
				require.True(t, response.OK)
				require.EqualValues(t, 1, apiCalls.Load())
				require.Equal(t, "new-access", client.AccessToken)
				wantRefresh := tc.refreshToken
				if wantRefresh == "" {
					wantRefresh = "old-refresh"
				}
				require.Equal(t, wantRefresh, client.refreshToken)
			}
			if tc.status == http.StatusOK {
				require.Equal(t, 1, saveCalls)
			} else {
				require.Zero(t, saveCalls)
			}
		})
	}
}

func TestConcurrentRequestsShareOAuthRefresh(t *testing.T) {
	var refreshCalls, apiCalls, saveCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			refreshCalls.Add(1)
			_, _ = fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		apiCalls.Add(1)
		assert.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	client.OAuthSetServerMetadata(&oauth.ServerMetadata{TokenEndpoint: server.URL + "/token"})
	client.OAuthSetTokens("client-id", "old-refresh", "old-access", time.Now().Add(-time.Minute))
	client.SaveNewToken = func(context.Context, string, string, time.Time) error {
		saveCalls.Add(1)
		return nil
	}

	const requests = 8
	start := make(chan struct{})
	errs := make(chan error, requests)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range requests {
		go func() {
			<-start
			_, err := client.MakeRequest(ctx, http.MethodGet, server.URL+"/api", nil, nil)
			errs <- err
		}()
	}
	close(start)
	for range requests {
		require.NoError(t, <-errs)
	}
	require.EqualValues(t, 1, refreshCalls.Load())
	require.EqualValues(t, 1, saveCalls.Load())
	require.EqualValues(t, requests, apiCalls.Load())
}
