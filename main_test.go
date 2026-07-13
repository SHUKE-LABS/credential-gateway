package main

import (
	"log/slog"
	"testing"
)

func TestResolveLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		flagVal string
		envVal  string
		want    slog.Level
		wantErr bool
	}{
		{name: "default when both empty", want: slog.LevelInfo},
		{name: "flag debug", flagVal: "debug", want: slog.LevelDebug},
		{name: "flag info", flagVal: "info", want: slog.LevelInfo},
		{name: "flag warn", flagVal: "warn", want: slog.LevelWarn},
		{name: "flag error", flagVal: "error", want: slog.LevelError},
		{name: "env debug", envVal: "debug", want: slog.LevelDebug},
		{name: "env warn", envVal: "warn", want: slog.LevelWarn},
		{name: "flag overrides env", flagVal: "error", envVal: "debug", want: slog.LevelError},
		{name: "case insensitive", flagVal: "DEBUG", want: slog.LevelDebug},
		{name: "empty flag falls back to env", flagVal: "", envVal: "error", want: slog.LevelError},
		{name: "invalid flag", flagVal: "verbose", wantErr: true},
		{name: "invalid env", envVal: "loud", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLogLevel(tt.flagVal, tt.envVal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got level %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
