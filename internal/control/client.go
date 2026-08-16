package control

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"nexthop/internal/config"
)

// Client 是 CLI 侧使用的 unix socket HTTP 客户端。
type Client struct {
	sock string
	http *http.Client
}

// NewClient 构造连接 sockPath 的客户端。
func NewClient(sock string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{sock: sock, http: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
}

// GetStatus 查询 daemon 状态。
func (c *Client) GetStatus() (*StatusResponse, error) {
	var out StatusResponse
	if err := c.getJSON("/api/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetConfig 获取当前生效配置的 YAML。
func (c *Client) GetConfig() ([]byte, error) {
	resp, err := c.http.Get("http://unix/api/config")
	if err != nil {
		return nil, fmt.Errorf("连接 daemon（%s）失败: %w", c.sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon 返回 %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// PostConfig 提交新配置（daemon 校验、保存并热加载）。
func (c *Client) PostConfig(data []byte) error {
	resp, err := c.http.Post("http://unix/api/config", "application/yaml", bytesReader(data))
	if err != nil {
		return fmt.Errorf("连接 daemon（%s）失败: %w", c.sock, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon 拒绝新配置: %s", trimBody(body))
	}
	return nil
}

// Reload 触发热加载。
func (c *Client) Reload() error {
	resp, err := c.http.Post("http://unix/api/reload", "", nil)
	if err != nil {
		return fmt.Errorf("连接 daemon（%s）失败: %w", c.sock, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon 重载失败: %s", trimBody(body))
	}
	return nil
}

func (c *Client) getJSON(path string, out any) error {
	resp, err := c.http.Get("http://unix" + path)
	if err != nil {
		return fmt.Errorf("连接 daemon（%s）失败: %w", c.sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon 返回 %s: %s", resp.Status, trimBody(body))
	}
	return jsonDecode(resp.Body, out)
}

// ParseConfig 解析 YAML 配置（CLI 侧校验用）。
func ParseConfig(data []byte) (*config.Config, error) {
	return config.Parse(data)
}
