package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSeconds(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{name: "sub-millisecond rounds to zero", duration: 2965 * time.Nanosecond, want: "0s"},
		{name: "milliseconds", duration: 827667654 * time.Nanosecond, want: "0.828s"},
		{name: "seconds keep millisecond precision", duration: 3286612119 * time.Nanosecond, want: "3.287s"},
		{name: "trailing zeros dropped", duration: 1500 * time.Millisecond, want: "1.5s"},
		{name: "minutes", duration: 5 * time.Minute, want: "300s"},
		{name: "hours", duration: time.Hour, want: "3600s"},
		{name: "negative", duration: -1500 * time.Millisecond, want: "-1.5s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Seconds("took", tt.duration)
			if got.Key != "took" || got.Value.Kind() != slog.KindString || got.Value.String() != tt.want {
				t.Errorf("got %s=%v, want took=%s", got.Key, got.Value, tt.want)
			}
		})
	}
}

func TestLogResponseWritesDurationInSeconds(t *testing.T) {
	var out bytes.Buffer
	l := &Logger{slog.New(&humanHandler{w: &out, level: slog.LevelInfo})}

	l.LogResponse(200, 2500*time.Millisecond)

	if got := out.String(); !strings.Contains(got, "duration=2.5s") {
		t.Errorf("log %q, want duration=2.5s", got)
	}
}
