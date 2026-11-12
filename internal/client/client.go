// Package client 是访问服务端的客户端，本机走 unix socket，远端走 HTTPS 加令牌
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cmdgate/internal/api"
	"cmdgate/internal/store"
)

// Config 是 cmdgate login 保存的远端连接信息
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// 自签证书时用来校验服务端的 CA 文件
	CAFile string `json:"ca_file,omitempty"`
}

// ConfigPath 返回客户端配置文件的路径
func ConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, store.Name, "client.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", store.Name, "client.json")
}

// LoadConfig 读取客户端配置，没有登录过时返回 nil
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	return &cfg, nil
}

// SaveConfig 保存客户端配置，文件只有本人可读
func SaveConfig(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ConfigPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0o600)
}

// Client 是一个到服务端的连接
type Client struct {
	http *http.Client
	// 请求地址的前缀
	base  string
	token string
	// 出错时用来提示的目标描述
	target string
}

// New 按环境决定连接方式
//
// 显式指定了数据目录时连它的 socket；否则登录过远端就连远端，没有则连默认的本机 socket
func New() (*Client, error) {
	if os.Getenv(store.EnvDataDir) == "" {
		cfg, err := LoadConfig()
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			return NewRemote(cfg)
		}
	}
	return NewLocal(store.Open().Socket()), nil
}

// NewLocal 创建连接本机 unix socket 的客户端
func NewLocal(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}, base: "http://" + store.Name, target: socket}
}

// NewRemote 创建连接远端服务端的客户端
func NewRemote(cfg *Config) (*Client, error) {
	transport := &http.Transport{}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contains no certificates", cfg.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &Client{
		http:   &http.Client{Transport: transport},
		base:   strings.TrimRight(cfg.Server, "/"),
		token:  cfg.Token,
		target: cfg.Server,
	}, nil
}

// do 发出一个请求并把响应解码到 out
//
// 参数 body 为 nil 时不带请求体，out 为 nil 时丢弃响应体
// 服务端返回错误状态时，返回的错误就是服务端给出的说明
func (c *Client) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the %s server at %s: %w", store.Name, c.target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e api.Error
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// WhoAmI 返回服务端识别出的当前身份
func (c *Client) WhoAmI() (*api.WhoAmI, error) {
	var out api.WhoAmI
	return &out, c.do("GET", "/v1/whoami", nil, &out)
}

// List 返回当前身份可以发起的操作
func (c *Client) List() ([]api.ActionView, error) {
	var out []api.ActionView
	return out, c.do("GET", "/v1/actions", nil, &out)
}

// Describe 返回一个操作的说明
func (c *Client) Describe(name string) (*api.ActionView, error) {
	var out api.ActionView
	return &out, c.do("GET", "/v1/actions/"+url.PathEscape(name), nil, &out)
}

// Submit 发起一个请求
func (c *Client) Submit(req api.SubmitRequest) (*api.RequestView, error) {
	var out api.RequestView
	return &out, c.do("POST", "/v1/requests", req, &out)
}

// Get 查询请求状态，wait 大于 0 时服务端会等到请求结束或超时再返回
func (c *Client) Get(id string, wait time.Duration) (*api.RequestView, error) {
	path := "/v1/requests/" + url.PathEscape(id)
	if wait > 0 {
		path += "?wait=" + wait.String()
	}
	var out api.RequestView
	return &out, c.do("GET", path, nil, &out)
}

// Cancel 取消一个请求
func (c *Client) Cancel(id string) (*api.RequestView, error) {
	var out api.RequestView
	return &out, c.do("POST", "/v1/requests/"+url.PathEscape(id)+"/cancel", nil, &out)
}

// AdminDecide 通过管理员应急入口批准或拒绝一个请求
func (c *Client) AdminDecide(id string, approve bool) (*api.RequestView, error) {
	var out api.RequestView
	return &out, c.do("POST", "/v1/admin/requests/"+url.PathEscape(id)+"/decision", api.Decision{Approve: approve}, &out)
}
