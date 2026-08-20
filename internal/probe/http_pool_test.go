package probe

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// dialCounter 统计客户端实际建立的连接数（Dial 次数 = 真实连接数，
// 服务端 Accept 调用数含阻塞等待，不能作为连接数判据）。
type dialCounter struct{ n atomic.Int64 }

func (d *dialCounter) inc()       { d.n.Add(1) }
func (d *dialCounter) count() int { return int(d.n.Load()) }

// newPooledHTTPProber 构造带 Dial 计数的 httpProber（复用正式参数）。
func newPooledHTTPProber(t *testing.T, url string) (*httpProber, *dialCounter) {
	t.Helper()
	dials := &dialCounter{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.inc()
			return (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, network, addr)
		},
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	}
	p := &httpProber{
		url:     url,
		timeout: 500 * time.Millisecond,
		client:  &http.Client{Timeout: 500 * time.Millisecond, Transport: transport},
	}
	return p, dials
}

func newPoolTestServer(t *testing.T) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	return srv, "http://" + ln.Addr().String() + "/healthz"
}

// 连续探测应复用同一条 keep-alive 连接（客户端只 Dial 一次）。
func TestHTTPProbeReusesConnection(t *testing.T) {
	srv, url := newPoolTestServer(t)
	defer srv.Close()

	p, dials := newPooledHTTPProber(t, url)
	for i := 0; i < 5; i++ {
		res := p.Probe(context.Background())
		if !res.OK {
			t.Fatalf("第 %d 次探测失败: %v", i, res.Error)
		}
	}
	if got := dials.count(); got != 1 {
		t.Fatalf("连接复用失效：客户端建立了 %d 条连接，期望 1", got)
	}
}

// 服务停止（含 keep-alive 连接全部断开）后，复用 client 的探测应判 down。
func TestHTTPProbeDetectsServerStop(t *testing.T) {
	srv, url := newPoolTestServer(t)

	p, _ := newPooledHTTPProber(t, url)
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("服务运行中探测应成功: %v", res.Error)
	}

	// 关闭服务：所有活跃连接（含 keep-alive）被强制断开。
	srv.Close()
	time.Sleep(50 * time.Millisecond) // 让对端感知 RST

	res = p.Probe(context.Background())
	if res.OK {
		t.Fatal("服务停止后探测不应成功")
	}
}
