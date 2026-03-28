//go:build windows

package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/UserExistsError/conpty"
)

// conptyTerminal 封装 ConPTY 伪控制台。
type conptyTerminal struct {
	cpty *conpty.ConPty
	cmd  *exec.Cmd
}

// Start 创建 ConPTY 并启动给定的 shell。
func Start(shell string, cols, rows int) (Terminal, error) {
	if shell == "" {
		shell = defaultShell()
	}

	cpty, err := conpty.Start(shell, conpty.ConPtyDimensions(cols, rows))
	if err != nil {
		return nil, fmt.Errorf("conpty start: %w", err)
	}

	return &conptyTerminal{cpty: cpty}, nil
}

func (t *conptyTerminal) Read(p []byte) (int, error) {
	return t.cpty.Read(p)
}

func (t *conptyTerminal) Write(p []byte) (int, error) {
	return t.cpty.Write(p)
}

func (t *conptyTerminal) Resize(cols, rows int) error {
	return t.cpty.Resize(cols, rows)
}

func (t *conptyTerminal) Wait() (*os.ProcessState, error) {
	_, err := t.cpty.Wait(context.Background())
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (t *conptyTerminal) Close() error {
	return t.cpty.Close()
}

// Supported 报告直接终端模式是否可用（在 Windows 上始终为 true）。
func Supported() bool { return true }

// defaultShell 返回 Windows 上的首选 Shell。
func defaultShell() string {
	if ps, err := exec.LookPath("powershell.exe"); err == nil {
		return ps
	}
	return "cmd.exe"
}
