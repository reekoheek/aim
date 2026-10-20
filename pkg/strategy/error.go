package strategy

import (
	"encoding/json"
	"net/http"
)

const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeServer         = "server_error"
	ErrorTypeUpstream       = "upstream_error"
)

const (
	anthropicErrorEnvelope = "error"
	anthropicErrorTypeAPI  = "api_error"
)

type ErrorWriter interface {
	WriteError(w http.ResponseWriter, status int, errorType string, message string)
}

type OpenAIErrors struct{}

func (OpenAIErrors) WriteError(w http.ResponseWriter, status int, errorType string, message string) {
	WriteError(w, status, errorType, message)
}

type AnthropicErrors struct{}

func (AnthropicErrors) WriteError(w http.ResponseWriter, status int, errorType string, message string) {
	writeJSON(w, status, anthropicErrorResponse{
		Type:  anthropicErrorEnvelope,
		Error: anthropicErrorDetail{Type: anthropicErrorType(errorType), Message: message},
	})
}

func anthropicErrorType(errorType string) string {
	if errorType == ErrorTypeInvalidRequest {
		return ErrorTypeInvalidRequest
	}
	return anthropicErrorTypeAPI
}

type anthropicErrorResponse struct {
	Type  string               `json:"type"`
	Error anthropicErrorDetail `json:"error"`
}

type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func WriteError(w http.ResponseWriter, status int, errorType string, message string) {
	writeError(w, status, errorDetail{Message: message, Type: errorType})
}

func WriteParamError(w http.ResponseWriter, status int, errorType string, param string, message string) {
	writeError(w, status, errorDetail{Message: message, Type: errorType, Param: &param})
}

func writeError(w http.ResponseWriter, status int, detail errorDetail) {
	writeJSON(w, status, errorResponse{Error: detail})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
