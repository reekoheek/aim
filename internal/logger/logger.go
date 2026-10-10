package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Logger wraps a slog.Logger with convenient methods.
type Logger struct {
	*slog.Logger
}

// New creates a new logger with human-readable format.
func New(level string) *Logger {
	l := slog.LevelInfo
	switch strings.ToUpper(level) {
	case "DEBUG":
		l = slog.LevelDebug
	case "WARN", "WARNING":
		l = slog.LevelWarn
	case "ERROR":
		l = slog.LevelError
	}

	handler := &humanHandler{
		w:       os.Stderr,
		level:   l,
		timeFmt: "15:04:05",
	}
	return &Logger{slog.New(handler)}
}

// humanHandler is a slog.Handler that outputs human-readable logs.
type humanHandler struct {
	w       io.Writer
	level   slog.Level
	timeFmt string
}

func (h *humanHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *humanHandler) Handle(_ context.Context, r slog.Record) error {
	timestamp := r.Time.Format("2006-01-02T15:04:05.000")
	level := r.Level.String()

	var attrs []string
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value))
		return true
	})

	attrsStr := ""
	if len(attrs) > 0 {
		attrsStr = " " + fmt.Sprint(attrs)
	}

	_, err := fmt.Fprintf(h.w, "[%s] [%s] %s%s\n", timestamp, level, r.Message, attrsStr)
	return err
}

func (h *humanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &humanHandler{
		w:       h.w,
		level:   h.level,
		timeFmt: h.timeFmt,
	}
}

func (h *humanHandler) WithGroup(name string) slog.Handler {
	return &humanHandler{
		w:       h.w,
		level:   h.level,
		timeFmt: h.timeFmt,
	}
}

// LogRequest logs an incoming request.
func (l *Logger) LogRequest(method, path, model, remoteAddr string) {
	l.Info("request",
		"method", method,
		"path", path,
		"model", model,
		"remote_addr", remoteAddr,
	)
}

// LogResponse logs the response status and duration.
func (l *Logger) LogResponse(status int, duration time.Duration) {
	l.Info("response",
		"status", status,
		"duration", duration.String(),
	)
}

// LogError logs an error.
func (l *Logger) LogError(msg string, err error, attrs ...any) {
	l.Error(msg, append(attrs, "error", err)...)
}

// LogHeaders logs the incoming request headers (key-value pairs).
func (l *Logger) LogHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	for k, vals := range headers {
		for _, v := range vals {
			l.Info("header", k, v)
		}
	}
}
