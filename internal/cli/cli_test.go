package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithDefaultCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"bare goal becomes run", []string{"fix the bug"}, []string{"run", "fix the bug"}},
		{"run subcommand untouched", []string{"run", "fix"}, []string{"run", "fix"}},
		{"status untouched", []string{"status"}, []string{"status"}},
		{"version untouched", []string{"version", "--json"}, []string{"version", "--json"}},
		{"doctor untouched", []string{"doctor"}, []string{"doctor"}},
		{"leading flag untouched", []string{"--help"}, []string{"--help"}},
		{"empty untouched", []string{}, []string{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := withDefaultCommand(tt.in)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("withDefaultCommand(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDataDir_XDGFallback(t *testing.T) {
	// No t.Parallel: this test uses t.Setenv.
	env := EntireEnv{PluginDataDir: "/explicit/dir"}
	if got := env.DataDir(); got != "/explicit/dir" {
		t.Errorf("explicit DataDir = %q, want /explicit/dir", got)
	}

	// When unset, fall back under XDG_DATA_HOME.
	xdg := EntireEnv{}
	t.Setenv("XDG_DATA_HOME", "/xdg")
	want := filepath.Join("/xdg", "entire", "plugins", "data", "loop")
	if got := xdg.DataDir(); got != want {
		t.Errorf("XDG DataDir = %q, want %q", got, want)
	}
}

func TestVersionCommand_JSON(t *testing.T) {
	t.Parallel()
	root := NewRootCommand("1.2.3")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"version":"1.2.3"`) || !strings.Contains(got, `"provider":"loop"`) {
		t.Errorf("version --json output = %q", got)
	}
}
