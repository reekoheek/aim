package cors

import (
	"net/http"
	"slices"
)

const (
	anyOrigin              = "*"
	allowedMethods         = "POST, OPTIONS"
	preflightMaxAgeSeconds = "600"
)

type handler struct {
	origins []string
	next    http.Handler
}

func New(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	return &handler{origins: origins, next: next}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	allowOrigin, allowed := h.allowOrigin(origin)
	if !allowed {
		h.next.ServeHTTP(w, r)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
	if !isPreflight(r) {
		h.next.ServeHTTP(w, r)
		return
	}

	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.Header().Set("Access-Control-Allow-Methods", allowedMethods)
	if requestedHeaders := r.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", requestedHeaders)
	}
	w.Header().Set("Access-Control-Max-Age", preflightMaxAgeSeconds)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) allowOrigin(origin string) (string, bool) {
	if origin == "" {
		return "", false
	}
	if slices.Contains(h.origins, anyOrigin) {
		return anyOrigin, true
	}
	if slices.Contains(h.origins, origin) {
		return origin, true
	}
	return "", false
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}
