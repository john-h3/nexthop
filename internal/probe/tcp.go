package probe

import (
	"context"
	"fmt"
	"net"
	"time"
)

type tcpProber struct {
	addr    string
	timeout time.Duration
}

func newTCPProber(ip string, port int, timeout time.Duration) (Prober, error) {
	return &tcpProber{addr: net.JoinHostPort(ip, fmt.Sprint(port)), timeout: timeout}, nil
}

// Probe 通过 TCP 连接成功判定可达。
// 连接以 RST 关闭（SetLinger(0)）：探测连接无业务数据，无需优雅关闭，
// 可跳过 TIME_WAIT（本机不留 60s 状态），并使对端 conntrack 条目即时清除。
func (p *tcpProber) Probe(ctx context.Context) Result {
	start := time.Now()
	dialer := net.Dialer{Timeout: p.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", p.addr)
	rtt := time.Since(start)
	if err != nil {
		return Result{OK: false, RTT: rtt, Error: err}
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // Close 时发 RST，跳过 TIME_WAIT
	}
	conn.Close()
	return Result{OK: true, RTT: rtt}
}
