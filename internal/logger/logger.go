package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Logger struct {
	*slog.Logger
}

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

func (l *Logger) LogRequest(method, path, model, remoteAddr string) {
	l.Info("request",
		"method", method,
		"path", path,
		"model", model,
		"remote_addr", remoteAddr,
	)
}

func (l *Logger) LogResponse(status int, duration time.Duration) {
	l.Info("response",
		"status", status,
		Seconds("duration", duration),
	)
}

func Seconds(key string, duration time.Duration) slog.Attr {
	return slog.String(key, formatSeconds(duration))
}

func formatSeconds(duration time.Duration) string {
	return strconv.FormatFloat(duration.Round(time.Millisecond).Seconds(), 'f', -1, 64) + "s"
}

func (l *Logger) LogError(msg string, err error, attrs ...any) {
	l.Error(msg, append(attrs, "error", err)...)
}

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
