package cli

import (
	"os"
	"path/filepath"
)

const (
	envCLIVersion    = "ENTIRE_CLI_VERSION"
	envRepoRoot      = "ENTIRE_REPO_ROOT"
	envPluginDataDir = "ENTIRE_PLUGIN_DATA_DIR"
	envXDGDataHome   = "XDG_DATA_HOME"
)

// EntireEnv captures the environment variables Entire supplies when it dispatches
// an external plugin command. The raw values are preserved (empty when unset) so
// `doctor` can report exactly what the parent passed; DataDir applies the XDG
// fallback callers actually use.
type EntireEnv struct {
	CLIVersion    string
	RepoRoot      string
	PluginDataDir string // raw ENTIRE_PLUGIN_DATA_DIR (may be empty)
}

// EnvFromOS reads the Entire-supplied environment from the process.
func EnvFromOS() EntireEnv {
	return EntireEnv{
		CLIVersion:    os.Getenv(envCLIVersion),
		RepoRoot:      os.Getenv(envRepoRoot),
		PluginDataDir: os.Getenv(envPluginDataDir),
	}
}

// DataDir returns the plugin data directory, applying the XDG fallback when
// ENTIRE_PLUGIN_DATA_DIR is unset: $XDG_DATA_HOME/entire/plugins/data/loop, or
// ~/.local/share/entire/plugins/data/loop.
func (e EntireEnv) DataDir() string {
	if e.PluginDataDir != "" {
		return e.PluginDataDir
	}
	if xdg := os.Getenv(envXDGDataHome); xdg != "" {
		return filepath.Join(xdg, "entire", "plugins", "data", "loop")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "entire", "plugins", "data", "loop")
}

func valueOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}
	return value
}
