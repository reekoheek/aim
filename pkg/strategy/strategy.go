package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	sessionAffinityHeader = "X-Session-Affinity"
	noSession             = "-"
)

var ErrBodyNotJSONObject = errors.New("request body must be a JSON object")

type Strategy interface {
	Handle(ctx context.Context, w http.ResponseWriter, req *Request)
}

type Request struct {
	Stream  bool
	Body    []byte
	Headers http.Header
}

func (r *Request) SessionID() string {
	if id := strings.TrimSpace(r.Headers.Get(sessionAffinityHeader)); id != "" {
		return id
	}
	return noSession
}

func (r *Request) BodyFields() (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &fields); err != nil || fields == nil {
		return nil, ErrBodyNotJSONObject
	}
	return fields, nil
}
