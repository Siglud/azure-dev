// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"azureaiagent/internal/exterrors"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvokeSessionFlagConflicts(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"session override", []string{"--new-session", "--session-id", "saved"}, "--new-session with --session-id"},
		{"empty session override", []string{"--new-session", "--session-id="}, "--new-session with --session-id"},
		{"conversation override", []string{"--new-session", "--conversation-id", "saved"},
			"--new-session with --conversation-id"},
		{"empty conversation override", []string{"--new-session", "--conversation-id="},
			"--new-session with --conversation-id"},
		{"conversation reset override", []string{"--new-conversation", "--conversation-id", "saved"},
			"--new-conversation with --conversation-id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newInvokeCommand(nil)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append(tt.args, "hello"))
			err := cmd.ExecuteContext(t.Context())
			require.ErrorContains(t, err, tt.want)
			localErr, ok := errors.AsType[*azdext.LocalError](err)
			require.True(t, ok)
			assert.Equal(t, exterrors.CodeConflictingArguments, localErr.Code)
		})
	}
}

func TestInvokeSessionFlags(t *testing.T) {
	for _, tt := range []struct {
		name      string
		flags     invokeFlags
		wantReset bool
	}{
		{name: "reuse"},
		{name: "new session", flags: invokeFlags{newSession: true}, wantReset: true},
		{name: "new conversation", flags: invokeFlags{newConversation: true}, wantReset: true},
		{name: "both", flags: invokeFlags{newSession: true, newConversation: true}, wantReset: true},
		{name: "explicit IDs", flags: invokeFlags{session: "session", conversation: "conversation"}},
		{name: "new conversation in explicit session",
			flags: invokeFlags{session: "session", newConversation: true}, wantReset: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newInvokeCommand(nil)
			if tt.flags.session != "" {
				require.NoError(t, cmd.Flags().Set("session-id", tt.flags.session))
			}
			if tt.flags.conversation != "" {
				require.NoError(t, cmd.Flags().Set("conversation-id", tt.flags.conversation))
			}
			require.NoError(t, validateInvokeSessionFlags(cmd, &tt.flags))
			assert.Equal(t, tt.wantReset, tt.flags.startsNewConversation())
		})
	}
}

func TestResponsesRemoteSessionReset(t *testing.T) {
	for _, tt := range []struct {
		name            string
		newSession      bool
		newConversation bool
		legacy          bool
		createFails     bool
	}{
		{name: "reuse saved IDs"},
		{name: "new session", newSession: true},
		{name: "new conversation keeps session", newConversation: true},
		{name: "both flags", newSession: true, newConversation: true},
		{name: "new session skips legacy IDs", newSession: true, legacy: true},
		{name: "conversation creation fails", newSession: true, createFails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const agentKey = "agent-key"
			config := newInvokeUserConfigServer()
			storedKey := agentKey
			if tt.legacy {
				storedKey = "agent"
			}
			config.setJSON(t, configPath("sessions"), map[string]string{storedKey: "session-old"})
			config.setJSON(t, configPath("conversations"), map[string]string{storedKey: "conversation-old"})
			client := newInvokeTestAzdClient(t, config)

			var request map[string]any
			conversationCreates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				switch {
				case strings.HasSuffix(r.URL.Path, "/conversations"):
					conversationCreates++
					if tt.createFails {
						http.Error(w, "conversation creation failed", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"conversation-new"}`)
				case strings.HasSuffix(r.URL.Path, "/responses"):
					if err := json.NewDecoder(r.Body).Decode(&request); !assert.NoError(t, err) {
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					w.Header().Set("x-agent-session-id", "session-new")
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\n"+
						"data: {\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			action := &InvokeAction{
				flags: &invokeFlags{
					message: "hi", newSession: tt.newSession, newConversation: tt.newConversation,
				},
				credential: responseTestCredential{},
				resolvedRemoteContext: &remoteContext{
					projectEndpoint: server.URL, name: "agent", apiVersion: "v1",
					agentKey: agentKey, azdClient: client,
				},
			}
			_, err := captureStdout(t, func() error { return action.responsesRemote(t.Context()) })
			if tt.createFails {
				require.ErrorContains(t, err, "failed to create conversation")
				assert.Equal(t, 1, conversationCreates)
				assert.Nil(t, request, "must not invoke using the old conversation after a failed reset")
				var conversations map[string]string
				config.getJSON(t, configPath("conversations"), &conversations)
				assert.Equal(t, "conversation-old", conversations[storedKey])
				return
			}
			require.NoError(t, err)
			require.NotNil(t, request)
			wantSession, wantConversation := "session-old", "conversation-old"
			if tt.newSession {
				assert.NotContains(t, request, "agent_session_id")
				wantSession = "session-new"
			} else {
				assert.Equal(t, wantSession, request["agent_session_id"])
			}
			if tt.newSession || tt.newConversation {
				assert.Equal(t, 1, conversationCreates)
				wantConversation = "conversation-new"
			} else {
				assert.Zero(t, conversationCreates)
			}
			assert.Equal(t, map[string]any{"id": wantConversation}, request["conversation"])
			for field, want := range map[string]string{"sessions": wantSession, "conversations": wantConversation} {
				var saved map[string]string
				config.getJSON(t, configPath(field), &saved)
				assert.Equal(t, want, saved[agentKey], "persisted %s", field)
			}
		})
	}
}

func TestResponsesLocalSessionReset(t *testing.T) {
	for _, tt := range []struct {
		name            string
		newSession      bool
		newConversation bool
	}{
		{name: "reuse saved IDs"},
		{name: "new session", newSession: true},
		{name: "new conversation keeps session", newConversation: true},
		{name: "both flags", newSession: true, newConversation: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			project := &helpersProjectServer{project: &azdext.ProjectConfig{Path: t.TempDir()}}
			t.Setenv("AZD_SERVER", newInvokeRemoteContextTestAzdServer(
				t, project, &azdext.UnimplementedEnvironmentServiceServer{},
			))
			client, err := azdext.NewAzdClient()
			require.NoError(t, err)
			defer client.Close()
			agentKey := buildLocalAgentKey(DefaultPort, "agent", "", project.project.Path)
			saveContextValue(t.Context(), client, agentKey, "session-old", "sessions")
			saveContextValue(t.Context(), client, agentKey, "conversation-old", "conversations")

			var request struct {
				SessionID    string `json:"session_id"`
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/responses", r.URL.Path)
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"completed","output":[]}`)
			}))
			defer server.Close()
			action := &InvokeAction{
				flags: &invokeFlags{
					message: "hi", name: "agent", local: true, port: testPort(t, server.URL),
					newSession: tt.newSession, newConversation: tt.newConversation,
				},
				noPrompt: true,
			}
			_, err = captureStdout(t, func() error { return action.responsesLocal(t.Context()) })
			require.NoError(t, err)
			require.NotEmpty(t, request.SessionID)
			require.NotEmpty(t, request.Conversation.ID)
			assert.Equal(t, tt.newSession, request.SessionID != "session-old")
			assert.Equal(t, tt.newSession || tt.newConversation, request.Conversation.ID != "conversation-old")
			for field, want := range map[string]string{
				"sessions": request.SessionID, "conversations": request.Conversation.ID,
			} {
				saved, err := getContextValueWithFallback(t.Context(), client, field, agentKey, nil)
				require.NoError(t, err)
				assert.Equal(t, want, saved)
			}
		})
	}
}
