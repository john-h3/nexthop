// Package probe 实现四种上游连通性探测器：ping / tcp / udp / http。
package probe

import (
	"context"
	"fmt"
	"time"

	"nexthop/internal/config"
)

// Result 是一次探测的结果。
type Result struct {
	OK    bool
	RTT   time.Duration
	Error error
}

// Prober 探测一个目标是否可达。
type Prober interface {
	Probe(ctx context.Context) Result
}

// New 根据目标配置构造探测器。
func New(t config.Target, timeout time.Duration) (Prober, error) {
	switch t.Probe {
	case config.ProbePing:
		return newPingProber(t.IP, timeout)
	case config.ProbeTCP:
		return newTCPProber(t.IP, t.Port, timeout)
	case config.ProbeUDP:
		return newUDPProber(t.IP, t.Port, timeout)
	case config.ProbeHTTP:
		return newHTTPProber(t.URL, timeout)
	default:
		return nil, fmt.Errorf("未知探测方式 %q", t.Probe)
	}
}
