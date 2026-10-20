package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ntfyClient 既发消息也管用户，用平台 admin 账号做 Basic Auth。
type ntfyClient struct {
	base, user, pass string
	http             *http.Client
}

func newNtfy(base, user, pass string) *ntfyClient {
	return &ntfyClient{base: strings.TrimRight(base, "/"), user: user, pass: pass, http: &http.Client{Timeout: 15 * time.Second}}
}

func (n *ntfyClient) do(ctx context.Context, method, path string, body any) error {
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.base+path, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.user != "" {
		req.SetBasicAuth(n.user, n.pass)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// action 通知上的按钮，ntfy 最多 3 个。
type action struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	Clear  bool   `json:"clear,omitempty"`
}

// publish 用 JSON 方式发布：标题和按钮里的中文不用再做 header 编码。
func (n *ntfyClient) publish(ctx context.Context, server, topic, title, body, click string, actions []action) error {
	c := n
	if server != "" && strings.TrimRight(server, "/") != n.base {
		c = newNtfy(server, "", "") // 用户自填的服务器绝不带平台凭据
	}
	msg := map[string]any{"topic": topic, "title": title, "message": body}
	if click != "" {
		msg["click"] = click
	}
	if len(actions) > 0 {
		msg["actions"] = actions
	}
	return c.do(ctx, http.MethodPost, "/", msg)
}

func (n *ntfyClient) createUser(ctx context.Context, username, password string) error {
	err := n.do(ctx, http.MethodPut, "/v1/users", map[string]string{"username": username, "password": password})
	if err != nil && strings.Contains(err.Error(), "409") { // 已存在
		return nil
	}
	return err
}

func (n *ntfyClient) deleteUser(ctx context.Context, username string) error {
	return n.do(ctx, http.MethodDelete, "/v1/users", map[string]string{"username": username})
}

func (n *ntfyClient) revoke(ctx context.Context, username, topic string) error {
	return n.do(ctx, http.MethodDelete, "/v1/users/access", map[string]string{"username": username, "topic": topic})
}

func (n *ntfyClient) grantRead(ctx context.Context, username, topic string) error {
	return n.do(ctx, http.MethodPost, "/v1/users/access", map[string]string{"username": username, "topic": topic, "permission": "read-only"})
}
