// Package router 通过 netlink 系统调用管理 IPv4 默认路由。
// 不使用任何命令行工具；需要 CAP_NET_ADMIN 权限。
package router

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// Router 管理默认路由。
type Router struct {
	egress string
}

// New 创建 Router 并校验出口网卡存在。
func New(egress string) (*Router, error) {
	r := &Router{egress: egress}
	if _, err := r.linkIndex(); err != nil {
		return nil, err
	}
	return r, nil
}

// linkIndex 解析出口网卡的 index。
func (r *Router) linkIndex() (int, error) {
	link, err := netlink.LinkByName(r.egress)
	if err != nil {
		return 0, fmt.Errorf("出口网卡 %q 不存在: %w", r.egress, err)
	}
	return link.Attrs().Index, nil
}

var defaultV4Dst = &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(0, 32)}

// ReplaceDefault 把 IPv4 默认路由指向 gw（经 egress 网卡）。
// 先删除所有现存默认路由，再添加新的，避免内核 ECMP 把流量分散到多个上游。
func (r *Router) ReplaceDefault(gw net.IP) error {
	idx, err := r.linkIndex()
	if err != nil {
		return err
	}
	if gw == nil || gw.To4() == nil {
		return fmt.Errorf("网关 %v 不是合法的 IPv4 地址", gw)
	}

	if err := r.RemoveDefault(); err != nil {
		return err
	}
	route := &netlink.Route{
		Dst:       defaultV4Dst,
		Gw:        gw.To4(),
		LinkIndex: idx,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("添加默认路由 via %s dev %s: %w", gw, r.egress, err)
	}
	return nil
}

// RemoveDefault 删除所有 IPv4 默认路由。
func (r *Router) RemoveDefault() error {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("读取路由表: %w", err)
	}
	for _, rt := range routes {
		if isDefaultV4(&rt) {
			if err := netlink.RouteDel(&rt); err != nil {
				return fmt.Errorf("删除默认路由: %w", err)
			}
		}
	}
	return nil
}

// CurrentDefault 返回当前 IPv4 默认路由的网关；没有则返回 nil。
func (r *Router) CurrentDefault() (net.IP, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("读取路由表: %w", err)
	}
	for _, rt := range routes {
		if isDefaultV4(&rt) && rt.Gw != nil {
			return rt.Gw, nil
		}
	}
	return nil, nil
}

// isDefaultV4 判断路由是否为 IPv4 默认路由（Dst 为 nil 或 0.0.0.0/0）。
// 注意：net.IPv4(0,0,0,0) 可能是 16 字节 IPv4-mapped 形式，必须用 To4() 规范化。
func isDefaultV4(rt *netlink.Route) bool {
	if rt.Dst == nil {
		// nil Dst 表示默认路由（内核表示法）。
		return true
	}
	ip4 := rt.Dst.IP.To4()
	if ip4 == nil || !ip4.IsUnspecified() || rt.Dst.Mask == nil {
		return false
	}
	ones, bits := rt.Dst.Mask.Size()
	return ones == 0 && bits == 32
}
