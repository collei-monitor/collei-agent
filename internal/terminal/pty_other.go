//go:build !windows

package terminal

import (
	"fmt"
	"os"
)

// Start is not supported on non-Windows platforms.
// Linux uses SSH tunnel mode instead of direct terminal.
func Start(_ string, _, _ int) (Terminal, error) {
	return nil, fmt.Errorf("direct terminal not supported on this platform (use SSH tunnel)")
}

// Supported reports whether direct terminal mode is available.
func Supported() bool { return false }

// defaultShell returns an empty string on non-Windows (unused).
func defaultShell() string { return "" }

// placeholder to satisfy any interface checks on non-Windows.
type stubTerminal struct{}

func (s *stubTerminal) Read(_ []byte) (int, error)      { return 0, fmt.Errorf("not supported") }
func (s *stubTerminal) Write(_ []byte) (int, error)     { return 0, fmt.Errorf("not supported") }
func (s *stubTerminal) Resize(_, _ int) error           { return fmt.Errorf("not supported") }
func (s *stubTerminal) Wait() (*os.ProcessState, error) { return nil, fmt.Errorf("not supported") }
func (s *stubTerminal) Close() error                    { return nil }
