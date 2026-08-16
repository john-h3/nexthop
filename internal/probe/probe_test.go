package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"nexthop/internal/config"
)

func TestNewPingProberValidation(t *testing.T) {
	if _, err := newPingProber("not-an-ip", time.Second); err == nil {
		t.Fatal("非法 IP 未报错")
	}
	if _, err := newPingProber("::1", time.Second); err == nil {
		t.Fatal("IPv6 未报错（仅支持 IPv4）")
	}
	if _, err := newPingProber("10.0.0.1", time.Second); err != nil {
		t.Fatalf("合法 IP 报错: %v", err)
	}
}

func TestNewUnknownProbe(t *testing.T) {
	_, err := New(config.Target{Name: "x", Probe: "smtp"}, time.Second)
	if err == nil {
		t.Fatal("未知探测方式未报错")
	}
}

func TestTCPProbeOK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: atoi(port), Probe: config.ProbeTCP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("探测应成功: %v", res.Error)
	}
}

func TestTCPProbeRefused(t *testing.T) {
	// 找一个未监听的端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // 立即关闭，端口变为未监听

	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: port, Probe: config.ProbeTCP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if res.OK {
		t.Fatal("未监听端口探测不应成功")
	}
}

func TestUDPProbeReply(t *testing.T) {
	// UDP 服务：收到数据报就回一个字节。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()

	port := pc.LocalAddr().(*net.UDPAddr).Port
	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: port, Probe: config.ProbeUDP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("有回复的 UDP 应视为通: %v", res.Error)
	}
}

func TestUDPProbeSilentTimeout(t *testing.T) {
	// UDP 端口开放但静默不回复 → 超时视为通。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	// 不启动读取 goroutine，内核不会回 ICMP（端口有 socket），探测应超时并判通。

	port := pc.LocalAddr().(*net.UDPAddr).Port
	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: port, Probe: config.ProbeUDP}, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("静默 UDP 应视为通（超时启发式）: %v", res.Error)
	}
}

func TestUDPProbeRefused(t *testing.T) {
	// 未监听端口 → 立即收到 ICMP port unreachable。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: port, Probe: config.ProbeUDP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if res.OK {
		t.Fatal("未监听 UDP 端口不应视为通")
	}
}

func TestHTTPProbeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p, err := New(config.Target{Name: "t", URL: srv.URL + "/healthz", Probe: config.ProbeHTTP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("200 应视为通: %v", res.Error)
	}
}

func TestHTTPProbeServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	p, err := New(config.Target{Name: "t", URL: srv.URL, Probe: config.ProbeHTTP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if res.OK {
		t.Fatal("503 不应视为通")
	}
}

func TestHTTPProbeConnectFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p, err := New(config.Target{Name: "t", URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/", Probe: config.ProbeHTTP}, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if res.OK {
		t.Fatal("连接失败不应视为通")
	}
}

// 并发探测不应互相干扰（探测循环每轮并行调用）。
func TestProbeConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	probers := []Prober{
		mustNew(t, config.Target{Name: "h", URL: srv.URL, Probe: config.ProbeHTTP}),
		mustNew(t, config.Target{Name: "t", IP: "127.0.0.1", Port: atoi(portStr), Probe: config.ProbeTCP}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, p := range probers {
			wg.Add(1)
			go func(p Prober) {
				defer wg.Done()
				res := p.Probe(context.Background())
				if !res.OK {
					t.Errorf("并发探测失败: %v", res.Error)
				}
			}(p)
		}
	}
	wg.Wait()
}

func mustNew(t *testing.T, target config.Target) Prober {
	t.Helper()
	p, err := New(target, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
