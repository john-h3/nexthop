package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// UDP 探测采用业界通用的启发式：
//   - 发送一个空数据报；
//   - 若收到 ICMP port unreachable（表现为 ECONNREFUSED）→ 不通；
//   - 超时未收到拒绝 → 视为通（多数 UDP 服务对空包静默丢弃或回复）。
type udpProber struct {
	addr    string
	timeout time.Duration
}

func newUDPProber(ip string, port int, timeout time.Duration) (Prober, error) {
	return &udpProber{addr: net.JoinHostPort(ip, fmt.Sprint(port)), timeout: timeout}, nil
}

func (p *udpProber) Probe(ctx context.Context) Result {
	start := time.Now()
	dialer := net.Dialer{Timeout: p.timeout}
	conn, err := dialer.DialContext(ctx, "udp", p.addr)
	if err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{0}); err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}

	// 等待 ICMP 拒绝或超时。
	_ = conn.SetReadDeadline(time.Now().Add(p.timeout))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
			// 超时无拒绝 → 视为通。
			return Result{OK: true, RTT: time.Since(start)}
		}
		// ECONNREFUSED 说明端口不可达。
		if isConnRefused(err) {
			return Result{OK: false, RTT: time.Since(start), Error: err}
		}
		// 其他错误视为不通。
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	// 收到了数据回复 → 通。
	return Result{OK: true, RTT: time.Since(start)}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isConnRefused(err error) bool {
	return errors.Is(err, net.ErrClosed) == false && containsStr(err.Error(), "connection refused")
}

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || containsStr(s[1:], sub))
}
