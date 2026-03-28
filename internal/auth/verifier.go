package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// MaxTimestampSkew 是消息时戳与当前服务器时间之间允许的最大偏差。
const MaxTimestampSkew = 30 * time.Second

// nonceTTL 是已使用的 nonce 保留用于去重的时间。
const nonceTTL = 60 * time.Second

// SignedMessage 包含签名验证需要的字段。
type SignedMessage struct {
	Type      string // 消息类型（例如 "open_terminal"）
	SessionID string
	Timestamp int64  // unix 秒数
	Nonce     string // 每条消息唯一
	Extra     string // 附加签名字段（例如文件操作的路径）
	Signature string // base64 编码的 Ed25519 签名
}

// CanonicalPayload 构造被签名的确定性字符串。
func (m *SignedMessage) CanonicalPayload() string {
	s := fmt.Sprintf("%s|%s|%d|%s", m.Type, m.SessionID, m.Timestamp, m.Nonce)
	if m.Extra != "" {
		s += "|" + m.Extra
	}
	return s
}

// Verifier 使用 CA 公钥验证 Ed25519 签名的控制框架。
type Verifier struct {
	pubKey ed25519.PublicKey

	mu     sync.Mutex
	nonces map[string]int64 // nonce 映射到过期时间戳
}

// NewVerifier 从原始 Ed25519 公钥创建一个 Verifier。
func NewVerifier(pubKey ed25519.PublicKey) *Verifier {
	v := &Verifier{
		pubKey: pubKey,
		nonces: make(map[string]int64),
	}
	go v.cleanupLoop()
	return v
}

// LoadVerifierFromFile 从文件中加载 CA 公钥（SSH authorized_key 格式）
// 并返回一个 Verifier。文件不存在时返回 nil, nil。
func LoadVerifierFromFile(path string) (*Verifier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read CA public key: %w", err)
	}

	pubKey, err := ParseSSHEd25519PublicKey(data)
	if err != nil {
		return nil, err
	}

	return NewVerifier(pubKey), nil
}

// ParseSSHEd25519PublicKey 从 SSH authorized_key 格式中提取 Ed25519 公钥。
func ParseSSHEd25519PublicKey(data []byte) (ed25519.PublicKey, error) {
	sshPub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse SSH public key: %w", err)
	}

	cryptoPub, ok := sshPub.(ssh.CryptoPublicKey)
	if !ok {
		return nil, fmt.Errorf("SSH key does not implement CryptoPublicKey")
	}

	edPub, ok := cryptoPub.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not Ed25519 (got %T)", cryptoPub.CryptoPublicKey())
	}

	return edPub, nil
}

// Verify 检查时戳、nonce 唯一性和 Ed25519 签名。
func (v *Verifier) Verify(msg *SignedMessage) error {
	// 1. Timestamp check
	now := time.Now().Unix()
	diff := now - msg.Timestamp
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(math.Ceil(MaxTimestampSkew.Seconds())) {
		return fmt.Errorf("timestamp expired: skew=%ds, max=%v", diff, MaxTimestampSkew)
	}

	// 2. Nonce dedup
	if msg.Nonce == "" {
		return fmt.Errorf("empty nonce")
	}
	if !v.recordNonce(msg.Nonce) {
		return fmt.Errorf("duplicate nonce: %s", msg.Nonce)
	}

	// 3. Decode signature
	sig, err := base64.StdEncoding.DecodeString(msg.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	// 4. Ed25519 verify
	payload := []byte(msg.CanonicalPayload())
	if !ed25519.Verify(v.pubKey, payload, sig) {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// recordNonce returns true if the nonce is new, false if it was already seen.
func (v *Verifier) recordNonce(nonce string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, exists := v.nonces[nonce]; exists {
		return false
	}
	v.nonces[nonce] = time.Now().Add(nonceTTL).Unix()
	return true
}

// cleanupLoop periodically removes expired nonces.
func (v *Verifier) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		v.mu.Lock()
		now := time.Now().Unix()
		for nonce, expiry := range v.nonces {
			if now >= expiry {
				delete(v.nonces, nonce)
			}
		}
		v.mu.Unlock()
	}
}
