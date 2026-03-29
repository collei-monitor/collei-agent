package fileapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gorilla/websocket"

	"github.com/collei-monitor/collei-agent/internal/auth"
)

const readChunkSize = 32768 // 32KB

// fileEntry 是 readdir 返回的目录项。
type fileEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Mode        uint32 `json:"mode"`
	IsDir       bool   `json:"is_dir"`
	Mtime       int64  `json:"mtime"`
	Permissions string `json:"permissions,omitempty"`
	LinkTarget  string `json:"link_target,omitempty"`
}

// safePath 验证并解析路径，防止目录遍历攻击。
// 返回清理后的绝对路径。
func safePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}

	// Windows: 前端使用 Unix 风格路径，需要转换盘符路径。
	// 例如 "/C:" → "C:\" , "/C:/Users" → "C:\Users"
	if runtime.GOOS == "windows" && len(raw) >= 3 && raw[0] == '/' && raw[2] == ':' {
		raw = raw[1:] // 去掉开头的 "/"
	}

	cleaned := filepath.Clean(raw)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	// 解析符号链接以检测通过符号链接目标的遍历
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// 文件可能尚不存在（例如用于写入/mkdir）。对于不存在的
		// 路径，评估父目录的符号链接。
		dir := filepath.Dir(abs)
		resolvedDir, dirErr := filepath.EvalSymlinks(dir)
		if dirErr != nil {
			// 父目录也不存在 — 使用清理后的绝对路径
			return abs, nil
		}
		return filepath.Join(resolvedDir, filepath.Base(abs)), nil
	}
	return resolved, nil
}

// handleReaddir 列出目录项。
func handleReaddir(ws *websocket.Conn, data map[string]interface{}) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	// Windows 特殊处理：当请求 "/" 时，列出所有可用驱动器
	if runtime.GOOS == "windows" && (path == "/" || path == "\\") {
		result := listWindowsDrives()
		resp, _ := json.Marshal(map[string]interface{}{
			"type":       "readdir_resp",
			"request_id": requestID,
			"session_id": sessionID,
			"entries":    result,
		})
		ws.WriteMessage(websocket.TextMessage, resp)
		return
	}

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "readdir_resp", requestID, sessionID, err.Error())
		return
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		sendFileError(ws, "readdir_resp", requestID, sessionID, err.Error())
		return
	}

	result := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		fe := fileEntry{
			Name:        e.Name(),
			Size:        info.Size(),
			Mode:        uint32(info.Mode()),
			Mtime:       info.ModTime().Unix(),
			Permissions: info.Mode().String(),
		}
		if e.Type()&os.ModeSymlink != 0 {
			fe.Type = "link"
			fe.IsDir = false
			fe.LinkTarget, _ = os.Readlink(filepath.Join(resolved, e.Name()))
		} else if e.IsDir() {
			fe.Type = "dir"
			fe.IsDir = true
		} else {
			fe.Type = "file"
			fe.IsDir = false
		}
		result = append(result, fe)
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "readdir_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"entries":    result,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// listWindowsDrives 枚举 Windows 上所有可用驱动器 (A:-Z:)。
func listWindowsDrives() []fileEntry {
	var drives []fileEntry
	for letter := 'A'; letter <= 'Z'; letter++ {
		root := string(letter) + ":\\"
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		drives = append(drives, fileEntry{
			Name:  string(letter) + ":",
			Type:  "drive",
			Size:  getDriveSize(root),
			Mode:  uint32(info.Mode()),
			IsDir: true,
			Mtime: info.ModTime().Unix(),
		})
	}
	return drives
}

// handleStat 返回文件/目录信息。
func handleStat(ws *websocket.Conn, data map[string]interface{}) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "stat_resp", requestID, sessionID, err.Error())
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		sendFileError(ws, "stat_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "stat_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"name":       info.Name(),
		"size":       info.Size(),
		"mode":       uint32(info.Mode()),
		"is_dir":     info.IsDir(),
		"mtime":      info.ModTime().Unix(),
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// handleRead 读取文件，并将其作为分块二进制帧发送。
func handleRead(ws *websocket.Conn, data map[string]interface{}) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)
	offset := int64FromJSON(data["offset"], 0)
	length := int64FromJSON(data["length"], -1) // -1 = entire file

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "read_resp", requestID, sessionID, err.Error())
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		sendFileError(ws, "read_resp", requestID, sessionID, err.Error())
		return
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			sendFileError(ws, "read_resp", requestID, sessionID, err.Error())
			return
		}
	}

	info, err := f.Stat()
	if err != nil {
		sendFileError(ws, "read_resp", requestID, sessionID, err.Error())
		return
	}

	totalSize := info.Size() - offset
	if length >= 0 && length < totalSize {
		totalSize = length
	}

	// Send the header with total size
	header, _ := json.Marshal(map[string]interface{}{
		"type":       "read_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"size":       totalSize,
	})
	if err := ws.WriteMessage(websocket.TextMessage, header); err != nil {
		return
	}

	// Send binary chunks
	buf := make([]byte, readChunkSize)
	var remaining int64 = totalSize
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, err := f.Read(buf[:toRead])
		if n > 0 {
			if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
				return
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			slog.Debug("FileAPI: read error", "path", resolved, "error", err)
			return
		}
	}
}

// handleWrite writes data from subsequent binary frames to a file.
// The next binary message after this control frame contains the data.
// Returns true to indicate the next binary frame should be captured for write.
func handleWrite(ws *websocket.Conn, data map[string]interface{}, verifier *auth.Verifier) bool {
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	// Signature verification for write operations
	if verifier != nil {
		path, _ := data["path"].(string)
		timestamp, _ := data["timestamp"].(float64)
		nonce, _ := data["nonce"].(string)
		signature, _ := data["signature"].(string)

		err := verifier.Verify(&auth.SignedMessage{
			Type:      "write",
			SessionID: sessionID,
			Timestamp: int64(timestamp),
			Nonce:     nonce,
			Extra:     path,
			Signature: signature,
		})
		if err != nil {
			slog.Warn("FileAPI: write signature rejected", "error", err)
			sendFileError(ws, "write_resp", requestID, sessionID, "signature_rejected: "+err.Error())
			return false
		}
	}

	return true // signal to capture next binary frame
}

// executeWrite performs the actual file write with the binary data.
func executeWrite(ws *websocket.Conn, data map[string]interface{}, payload []byte) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)
	offset := int64FromJSON(data["offset"], 0)

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "write_resp", requestID, sessionID, err.Error())
		return
	}

	flag := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flag |= os.O_TRUNC
	}

	f, err := os.OpenFile(resolved, flag, 0o644)
	if err != nil {
		sendFileError(ws, "write_resp", requestID, sessionID, err.Error())
		return
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			sendFileError(ws, "write_resp", requestID, sessionID, err.Error())
			return
		}
	}

	n, err := f.Write(payload)
	if err != nil {
		sendFileError(ws, "write_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":          "write_resp",
		"request_id":    requestID,
		"session_id":    sessionID,
		"bytes_written": n,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// handleRemove deletes a file.
func handleRemove(ws *websocket.Conn, data map[string]interface{}, verifier *auth.Verifier) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	if verifier != nil {
		timestamp, _ := data["timestamp"].(float64)
		nonce, _ := data["nonce"].(string)
		signature, _ := data["signature"].(string)

		err := verifier.Verify(&auth.SignedMessage{
			Type:      "remove",
			SessionID: sessionID,
			Timestamp: int64(timestamp),
			Nonce:     nonce,
			Extra:     path,
			Signature: signature,
		})
		if err != nil {
			slog.Warn("FileAPI: remove signature rejected", "error", err)
			sendFileError(ws, "remove_resp", requestID, sessionID, "signature_rejected: "+err.Error())
			return
		}
	}

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "remove_resp", requestID, sessionID, err.Error())
		return
	}

	if err := os.Remove(resolved); err != nil {
		sendFileError(ws, "remove_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "remove_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"ok":         true,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// handleRename renames/moves a file or directory.
func handleRename(ws *websocket.Conn, data map[string]interface{}, verifier *auth.Verifier) {
	oldPath, _ := data["old"].(string)
	newPath, _ := data["new"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	if verifier != nil {
		timestamp, _ := data["timestamp"].(float64)
		nonce, _ := data["nonce"].(string)
		signature, _ := data["signature"].(string)

		err := verifier.Verify(&auth.SignedMessage{
			Type:      "rename",
			SessionID: sessionID,
			Timestamp: int64(timestamp),
			Nonce:     nonce,
			Extra:     oldPath + "|" + newPath,
			Signature: signature,
		})
		if err != nil {
			slog.Warn("FileAPI: rename signature rejected", "error", err)
			sendFileError(ws, "rename_resp", requestID, sessionID, "signature_rejected: "+err.Error())
			return
		}
	}

	resolvedOld, err := safePath(oldPath)
	if err != nil {
		sendFileError(ws, "rename_resp", requestID, sessionID, err.Error())
		return
	}
	resolvedNew, err := safePath(newPath)
	if err != nil {
		sendFileError(ws, "rename_resp", requestID, sessionID, err.Error())
		return
	}

	if err := os.Rename(resolvedOld, resolvedNew); err != nil {
		sendFileError(ws, "rename_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "rename_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"ok":         true,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// handleMkdir creates a directory.
func handleMkdir(ws *websocket.Conn, data map[string]interface{}, verifier *auth.Verifier) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	if verifier != nil {
		timestamp, _ := data["timestamp"].(float64)
		nonce, _ := data["nonce"].(string)
		signature, _ := data["signature"].(string)

		err := verifier.Verify(&auth.SignedMessage{
			Type:      "mkdir",
			SessionID: sessionID,
			Timestamp: int64(timestamp),
			Nonce:     nonce,
			Extra:     path,
			Signature: signature,
		})
		if err != nil {
			slog.Warn("FileAPI: mkdir signature rejected", "error", err)
			sendFileError(ws, "mkdir_resp", requestID, sessionID, "signature_rejected: "+err.Error())
			return
		}
	}

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "mkdir_resp", requestID, sessionID, err.Error())
		return
	}

	if err := os.MkdirAll(resolved, 0o755); err != nil {
		sendFileError(ws, "mkdir_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "mkdir_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"ok":         true,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// handleRmdir removes a directory.
func handleRmdir(ws *websocket.Conn, data map[string]interface{}, verifier *auth.Verifier) {
	path, _ := data["path"].(string)
	requestID, _ := data["request_id"].(string)
	sessionID, _ := data["session_id"].(string)

	if verifier != nil {
		timestamp, _ := data["timestamp"].(float64)
		nonce, _ := data["nonce"].(string)
		signature, _ := data["signature"].(string)

		err := verifier.Verify(&auth.SignedMessage{
			Type:      "rmdir",
			SessionID: sessionID,
			Timestamp: int64(timestamp),
			Nonce:     nonce,
			Extra:     path,
			Signature: signature,
		})
		if err != nil {
			slog.Warn("FileAPI: rmdir signature rejected", "error", err)
			sendFileError(ws, "rmdir_resp", requestID, sessionID, "signature_rejected: "+err.Error())
			return
		}
	}

	resolved, err := safePath(path)
	if err != nil {
		sendFileError(ws, "rmdir_resp", requestID, sessionID, err.Error())
		return
	}

	if err := os.RemoveAll(resolved); err != nil {
		sendFileError(ws, "rmdir_resp", requestID, sessionID, err.Error())
		return
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"type":       "rmdir_resp",
		"request_id": requestID,
		"session_id": sessionID,
		"ok":         true,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// sendFileError sends a JSON error response for file operations.
func sendFileError(ws *websocket.Conn, respType, requestID, sessionID, errMsg string) {
	resp, _ := json.Marshal(map[string]interface{}{
		"type":       respType,
		"request_id": requestID,
		"session_id": sessionID,
		"error":      errMsg,
	})
	ws.WriteMessage(websocket.TextMessage, resp)
}

// int64FromJSON extracts an int64 from a JSON-decoded value (float64).
func int64FromJSON(v interface{}, fallback int64) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return fallback
}
