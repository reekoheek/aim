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
	body, _ := json.Marshal(errorResponse{Error: detail})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
