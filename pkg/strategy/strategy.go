package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	ChatCompletionsPath = "/v1/chat/completions"
	SystemOnePath       = "/v1/systemone"
	MessagesPath        = "/v1/messages"
	CountTokensPath     = "/v1/messages/count_tokens"
)

const (
	sessionAffinityHeader   = "X-Session-Affinity"
	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
	claudeCodeAgentHeader   = "X-Claude-Code-Agent-Id"
	sessionAgentSeparator   = "/"
	noSession               = "-"
)

var ErrBodyNotJSONObject = errors.New("request body must be a JSON object")

type Strategy interface {
	Handle(ctx context.Context, w http.ResponseWriter, req *Request)
}

type Request struct {
	Stream  bool
	Body    []byte
	Headers http.Header
	Query   string
	Errors  ErrorWriter
}

func (r *Request) SessionID() string {
	session := firstHeaderValue(r.Headers, sessionAffinityHeader, claudeCodeSessionHeader)
	if session == "" {
		session = noSession
	}
	if agent := strings.TrimSpace(r.Headers.Get(claudeCodeAgentHeader)); agent != "" {
		return session + sessionAgentSeparator + agent
	}
	return session
}

func firstHeaderValue(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func (r *Request) BodyFields() (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &fields); err != nil || fields == nil {
		return nil, ErrBodyNotJSONObject
	}
	return fields, nil
}

func (r *Request) WriteError(w http.ResponseWriter, status int, errorType string, message string) {
	errorWriter := r.Errors
	if errorWriter == nil {
		errorWriter = OpenAIErrors{}
	}
	errorWriter.WriteError(w, status, errorType, message)
}
