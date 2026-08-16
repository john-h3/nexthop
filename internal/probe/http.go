package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// httpProber 通过 HTTP 探测上游可用性。
//
// 连接策略：启用 keep-alive 连接池（net/http 自带池化，每个 target 一个
// Client/Transport），避免高频探测的连接风暴；同时通过 rebuildEvery
// 周期性强制清空连接池，定期验证"现在能否新建连接"，防止池化掩盖服务故障。
type httpProber struct {
	url      string
	timeout  time.Duration
	client   *http.Client
	transport *http.Transport

	probeCount  atomic.Uint64
	rebuildEvery uint64 // 每 N 次探测强制重建连接（0 = 禁用）
}

func newHTTPProber(url string, timeout time.Duration) (Prober, error) {
	// 连通性探测：跳过 TLS 证书校验（目标是裸 IP 网关，证书通常对不上）。
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		// keep-alive 连接池：减少连接建立频率（局域网探测每次复用同一连接）。
		MaxIdleConns:       2,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // 连通性探测，非安全校验
		},
	}
	return &httpProber{
		url:          url,
		timeout:      timeout,
		client:       &http.Client{Timeout: timeout, Transport: transport},
		transport:    transport,
		rebuildEvery: 10, // 每 10 次探测（如 200ms 间隔 ≈ 2s）强制新连接验证一次
	}, nil
}

// Probe 以 HTTP 状态码 < 400 判定可达。
func (p *httpProber) Probe(ctx context.Context) Result {
	// 周期性强制重建连接：防止 keep-alive 复用掩盖"服务已停止接受新连接"。
	if p.rebuildEvery > 0 && p.probeCount.Add(1)%p.rebuildEvery == 0 {
		p.transport.CloseIdleConnections()
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	defer resp.Body.Close()
	// 尽量读完，便于连接复用。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return Result{OK: false, RTT: time.Since(start), Error: errStatus(resp.StatusCode)}
	}
	return Result{OK: true, RTT: time.Since(start)}
}

type statusError int

func (e statusError) Error() string { return "http 状态码 " + itoa(int(e)) }

func errStatus(code int) error { return statusError(code) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
