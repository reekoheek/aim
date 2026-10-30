package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewServeCmd(t *testing.T) {
	errServe := errors.New("serve failed")

	tests := []struct {
		name        string
		args        []string
		serveErr    error
		wantErr     bool
		wantErrIs   error
		wantCalled  bool
		wantCfgFile string
	}{
		{
			name:        "runs with config shorthand flag",
			args:        []string{"serve", "-c", "config.yaml"},
			wantCalled:  true,
			wantCfgFile: "config.yaml",
		},
		{
			name:        "runs with config long flag",
			args:        []string{"serve", "--config", "path/to/config.yaml"},
			wantCalled:  true,
			wantCfgFile: "path/to/config.yaml",
		},
		{
			name:       "runs without config flag",
			args:       []string{"serve"},
			wantCalled: true,
		},
		{
			name:        "returns serve error",
			args:        []string{"serve", "-c", "config.yaml"},
			serveErr:    errServe,
			wantErr:     true,
			wantErrIs:   errServe,
			wantCalled:  true,
			wantCfgFile: "config.yaml",
		},
		{
			name:    "rejects positional arguments",
			args:    []string{"serve", "config.yaml"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var gotCfgFile string
			cmd := newRootCmd(func(cfgFile string) error {
				called = true
				gotCfgFile = cfgFile
				return tt.serveErr
			}, backendDependencies{})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()

			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("Execute() error = %v, want %v", err, tt.wantErrIs)
			}
			if called != tt.wantCalled {
				t.Fatalf("serve called = %v, want %v", called, tt.wantCalled)
			}
			if gotCfgFile != tt.wantCfgFile {
				t.Fatalf("config file = %q, want %q", gotCfgFile, tt.wantCfgFile)
			}
		})
	}
}

func TestServe(t *testing.T) {
	tests := []struct {
		name    string
		cfgFile string
		wantErr string
	}{
		{
			name:    "missing config file",
			cfgFile: filepath.Join(t.TempDir(), "missing.yaml"),
			wantErr: "load config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := serve(tt.cfgFile)

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
