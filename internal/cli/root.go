// Package cli is the `remote` node-agent command line.
package cli

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags (make release); a
// `go install …@v0.1.0` build has no ldflags and takes the module version
// Go recorded instead; "dev" for go run and plain builds.
var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}

var homeDir string

// homePath resolves a path inside the agent home (~/.remote by default).
func homePath(elem ...string) string {
	return filepath.Join(append([]string{homeDir}, elem...)...)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:     "remote",
		Short:   "remote.network node agent — turn this machine into AI infrastructure",
		Version: Version,
	}
	defaultHome, err := os.UserHomeDir()
	if err != nil {
		defaultHome = "."
	}
	root.PersistentFlags().StringVar(&homeDir, "home",
		filepath.Join(defaultHome, ".remote"),
		"agent home directory (identity, router pin, state)")
	root.AddCommand(newNodeCommand(), newBackendCommand())
	return root
}

// envOr reads an environment variable with a fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Execute runs the CLI; it calls os.Exit on error.
func Execute() {
	if err := NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
