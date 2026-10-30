package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewRootCmd(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		wantOutput string
	}{
		{
			name:       "without subcommand prints help",
			args:       []string{},
			wantOutput: "serve",
		},
		{
			name:    "rejects config shorthand flag",
			args:    []string{"-c", "config.yaml"},
			wantErr: true,
		},
		{
			name:    "rejects config long flag",
			args:    []string{"--config", "config.yaml"},
			wantErr: true,
		},
		{
			name:       "prints version",
			args:       []string{"--version"},
			wantOutput: version,
		},
		{
			name:    "rejects unknown subcommand",
			args:    []string{"daemon"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveCalled := false
			cmd := newRootCmd(func(cfgFile string) error {
				serveCalled = true
				return nil
			})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()

			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if serveCalled {
				t.Fatal("serve was called")
			}
			if !strings.Contains(output.String(), tt.wantOutput) {
				t.Fatalf("output = %q, want it to contain %q", output.String(), tt.wantOutput)
			}
		})
	}
}
