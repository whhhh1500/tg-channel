// Package reply 生成和校验"回复链接"的 token：内容压缩后 base64 放在 URL 的 # 后面，
// 带 HMAC 签名和过期时间，服务端不存任何东西。
package reply

import (
	"bytes"
	"compress/flate"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	PreviewRunes = 1000           // 超过 1000 字才截断（Telegram 单条上限 4096 字）
	TTL          = 24 * time.Hour // 链接有效期
	MaxUses      = 3              // 每条消息最多回 3 次
)

var (
	ErrBadToken = errors.New("链接无效")
	ErrExpired  = errors.New("链接已过期")
)

// Payload 回复页需要的全部信息。字段名刻意短，省 URL 长度。
type Payload struct {
	Account    uint   `json:"a"`
	PeerKind   string `json:"k"`
	PeerID     int64  `json:"p"`
	AccessHash int64  `json:"h"`
	MsgID      int    `json:"m"`
	Exp        int64  `json:"e"`
	PeerTitle  string `json:"c"`
	Sender     string `json:"f"`
	SenderUser string `json:"u,omitempty"`
	SenderID   int64  `json:"i,omitempty"`
	Text       string `json:"t"`
}

var enc = base64.RawURLEncoding

// MaxTokenLen 通知整包约 8KB，正文占 3.6KB，token 控制在 3500 字符以内。
const MaxTokenLen = 3500

// Sign 压缩 + 签名，返回放进 URL 的 token（payload.sig）。
// 正文超过 PreviewRunes 才截断；token 仍超长（比如不可压缩的随机文本）就把预览逐级降到 500、200 字。
func Sign(secret string, p Payload) (string, error) {
	for _, n := range []int{PreviewRunes, 500, 200} {
		tok, err := signN(secret, p, n)
		if err != nil || len(tok) <= MaxTokenLen {
			return tok, err
		}
	}
	return signN(secret, p, 100)
}

func signN(secret string, p Payload, maxRunes int) (string, error) {
	if r := []rune(p.Text); len(r) > maxRunes {
		p.Text = string(r[:maxRunes]) + "…"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	_, _ = w.Write(raw)
	_ = w.Close()
	body := enc.EncodeToString(buf.Bytes())
	return body + "." + sig(secret, body), nil
}

// Parse 验签、解压、检查过期。
func Parse(secret, token string) (Payload, error) {
	body, s, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(s), []byte(sig(secret, body))) {
		return Payload{}, ErrBadToken
	}
	zipped, err := enc.DecodeString(body)
	if err != nil {
		return Payload{}, ErrBadToken
	}
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(zipped)), 64<<10))
	if err != nil {
		return Payload{}, ErrBadToken
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, ErrBadToken
	}
	if time.Now().Unix() > p.Exp {
		return p, ErrExpired
	}
	return p, nil
}

// Key 用签名做计数的键，不暴露内容。
func Key(token string) string {
	_, s, _ := strings.Cut(token, ".")
	return s
}

func sig(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return enc.EncodeToString(m.Sum(nil)[:16])
}

// DeepLink 打开 Telegram 对应会话的链接；普通小群没有深链，返回空。
func DeepLink(kind string, peerID int64, msgID int) string {
	switch kind {
	case "user":
		return fmt.Sprintf("tg://user?id=%d", peerID)
	case "channel":
		return fmt.Sprintf("https://t.me/c/%d/%d", peerID, msgID)
	}
	return ""
}
