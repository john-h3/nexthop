package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// pingProber 通过 ICMP Echo 探测可达性。
// 优先尝试特权 raw socket（ip4:icmp，需要 root / CAP_NET_RAW）；
// 失败则回退到非特权 ping socket（udp4，需要内核 net.ipv4.ping_group_range 覆盖运行用户组）。
type pingProber struct {
	ip      net.IP
	timeout time.Duration
	seq     atomic.Uint32
}

func newPingProber(ip string, timeout time.Duration) (Prober, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return nil, fmt.Errorf("ping 目标 %q 不是合法的 IPv4 地址", ip)
	}
	return &pingProber{ip: parsed, timeout: timeout}, nil
}

func (p *pingProber) Probe(ctx context.Context) Result {
	start := time.Now()
	conn, err := p.listen()
	if err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	defer conn.Close()

	seq := p.seq.Add(1)
	id := os.Getpid() & 0xffff
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: int(seq), Data: []byte("nexthop-probe")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}
	if _, err := conn.WriteTo(wire, &net.IPAddr{IP: p.ip}); err != nil {
		return Result{OK: false, RTT: time.Since(start), Error: err}
	}

	// 读取回复直到匹配 ID/Seq，或超时。
	deadline := time.Now().Add(p.timeout)
	buf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return Result{OK: false, RTT: time.Since(start), Error: ctx.Err()}
		}
		_ = conn.SetReadDeadline(deadline)
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			if isTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded) {
				return Result{OK: false, RTT: time.Since(start), Error: fmt.Errorf("ping 超时")}
			}
			return Result{OK: false, RTT: time.Since(start), Error: err}
		}
		if peer == nil || peer.String() != p.ip.String() {
			continue // 忽略其他来源的包
		}
		reply, err := icmp.ParseMessage(1, buf[:n])
		if err != nil {
			continue
		}
		if reply.Type == ipv4.ICMPTypeEchoReply {
			if echo, ok := reply.Body.(*icmp.Echo); ok && echo.ID == id && echo.Seq == int(seq) {
				return Result{OK: true, RTT: time.Since(start)}
			}
		}
	}
}

// listen 尝试创建 ICMP socket，优先特权模式。
func (p *pingProber) listen() (*icmp.PacketConn, error) {
	if conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return conn, nil
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("创建 ICMP socket 失败（需要 root/CAP_NET_RAW，或设置 net.ipv4.ping_group_range）: %w", err)
	}
	return conn, nil
}
