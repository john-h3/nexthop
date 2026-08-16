package router

import (
	"net"
	"testing"
)

func TestNewInvalidDevice(t *testing.T) {
	if _, err := New("no-such-device-xyz"); err == nil {
		t.Fatal("不存在的网卡应报错")
	}
}

func TestIsDefaultV4(t *testing.T) {
	cases := []struct {
		name string
		rt   netlinkRouteLike
		want bool
	}{
		{"nil dst", netlinkRouteLike{dst: nil}, true},
		{"zero zero", netlinkRouteLike{dst: &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(0, 32)}}, true},
		{"非默认", netlinkRouteLike{dst: &net.IPNet{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(24, 32)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rt.isDefault(); got != tc.want {
				t.Fatalf("isDefault = %v, want %v", got, tc.want)
			}
		})
	}
}

// netlinkRouteLike 只暴露测试需要的字段，避免测试依赖真实 netlink.Route。
type netlinkRouteLike struct {
	dst *net.IPNet
}

func (r netlinkRouteLike) isDefault() bool {
	if r.dst == nil {
		return true
	}
	ip4 := r.dst.IP.To4()
	if ip4 == nil || !ip4.IsUnspecified() || r.dst.Mask == nil {
		return false
	}
	ones, bits := r.dst.Mask.Size()
	return ones == 0 && bits == 32
}
