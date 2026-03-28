package terminal

import (
	"io"
	"os"
)

// Terminal 表示一个伪控制台会话 (Windows 上是 ConPTY)。
type Terminal interface {
	io.ReadWriteCloser
	// Resize 改变终端尺寸。
	Resize(cols, rows int) error
	// Wait 一直阻塞到 shell 进程退出。
	Wait() (*os.ProcessState, error)
}
