package probe

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"nexthop/internal/config"
)

// 探测连接应以 RST 关闭：服务端 read 应返回 connection reset（而非 EOF）。
func TestTCPProbeClosesWithRST(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	readErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			readErr <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		_, err = conn.Read(buf) // 客户端关闭后：EOF（FIN）或 ECONNRESET（RST）
		readErr <- err
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: atoi(portStr), Probe: config.ProbeTCP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if !res.OK {
		t.Fatalf("探测应成功: %v", res.Error)
	}

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("服务端收到 EOF（FIN 优雅关闭），期望 RST（connection reset）")
		}
		if !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("期望 connection reset，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端 read 超时")
	}
}

// 大量探测后本机 TCP TIME_WAIT 不应增长（RST 关闭跳过 TIME_WAIT）。
func TestTCPProbeNoTimeWait(t *testing.T) {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Skipf("无法读取 /proc/net/tcp（%v），跳过", err)
	}
	_ = data // 仅探测可读性

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
	p, err := New(config.Target{Name: "t", IP: "127.0.0.1", Port: atoi(portStr), Probe: config.ProbeTCP}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	before := countTimeWait(t)
	for i := 0; i < 50; i++ { // 50 次探测：若为 FIN 关闭会产生 ~50 个 TIME_WAIT
		if res := p.Probe(context.Background()); !res.OK {
			t.Fatalf("第 %d 次探测失败: %v", i, res.Error)
		}
	}
	// 给内核一点时间处理 RST。
	time.Sleep(100 * time.Millisecond)
	after := countTimeWait(t)

	if after > before {
		t.Fatalf("探测产生了 TIME_WAIT：%d -> %d（期望不增长）", before, after)
	}
}

// countTimeWait 统计 /proc/net/tcp 中 TIME_WAIT（st=06）的连接数。
func countTimeWait(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatalf("读取 /proc/net/tcp: %v", err)
	}
	count := 0
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if st, err := strconv.ParseUint(fields[3], 16, 8); err == nil && st == 6 { // 06 = TIME_WAIT
			count++
		}
	}
	return count
}
