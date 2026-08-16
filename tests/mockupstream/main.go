// mockupstream 是集成测试用的模拟上游服务：
// 提供可独立开关的 tcp / http / udp 三个探测端点，
// 通过控制端口 POST /state 设置各服务的存活状态。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
)

type svc struct {
	name string
	addr string
	kind string // tcp | http | udp

	mu      sync.Mutex
	up      bool
	ln      net.Listener
	httpSrv *http.Server // http 类型：用于关闭时强制断开活跃连接
	pc      net.PacketConn
}

// set 打开或关闭该服务（listener 真实关闭/重建，保证探测行为真实变化）。
func (s *svc) set(up bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if up == s.up {
		return nil
	}
	if up {
		if err := s.open(); err != nil {
			return err
		}
	} else {
		s.close()
	}
	s.up = up
	return nil
}

func (s *svc) open() error {
	switch s.kind {
	case "tcp":
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			return fmt.Errorf("监听 %s %s: %w", s.name, s.addr, err)
		}
		s.ln = ln
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	case "http":
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			return fmt.Errorf("监听 %s %s: %w", s.name, s.addr, err)
		}
		s.ln = ln
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			io.WriteString(w, "ok")
		})}
		s.httpSrv = srv
		go srv.Serve(ln)
	case "udp":
		pc, err := net.ListenPacket("udp", s.addr)
		if err != nil {
			return fmt.Errorf("监听 %s %s: %w", s.name, s.addr, err)
		}
		s.pc = pc
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
	}
	return nil
}

func (s *svc) close() {
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
	// http.Server.Close 会强制关闭所有活跃连接（含 keep-alive 空闲连接），
	// 真实模拟"服务停止 = 所有连接断开"，保证复用连接的探测也能感知故障。
	if s.httpSrv != nil {
		s.httpSrv.Close()
		s.httpSrv = nil
	}
	if s.pc != nil {
		s.pc.Close()
		s.pc = nil
	}
}

func main() {
	tcpAddr := flag.String("tcp", "0.0.0.0:9001", "tcp 探测地址")
	httpAddr := flag.String("http", "0.0.0.0:9002", "http 探测地址")
	udpAddr := flag.String("udp", "0.0.0.0:9003", "udp 探测地址")
	ctlAddr := flag.String("ctl", "127.0.0.1:9100", "控制端口")
	flag.Parse()

	svcs := []*svc{
		{name: "tcp", addr: *tcpAddr, kind: "tcp"},
		{name: "http", addr: *httpAddr, kind: "http"},
		{name: "udp", addr: *udpAddr, kind: "udp"},
	}
	allUp := func() error {
		for _, s := range svcs {
			if err := s.set(true); err != nil {
				return err
			}
		}
		return nil
	}
	if err := allUp(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	log.Printf("mockupstream 已启动 tcp=%s http=%s udp=%s (全部 up)", *tcpAddr, *httpAddr, *udpAddr)

	// 控制端点：GET/POST /state，POST body 如 {"tcp":false,"http":true,"udp":true}
	http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]bool{
				"tcp": svcs[0].up, "http": svcs[1].up, "udp": svcs[2].up,
			})
			return
		}
		var flags map[string]bool
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&flags); err != nil {
			http.Error(w, "无效的 JSON body", 400)
			return
		}
		for _, s := range svcs {
			if v, ok := flags[s.name]; ok {
				if err := s.set(v); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "已应用"})
	})
	if err := http.ListenAndServe(*ctlAddr, nil); err != nil {
		log.Fatalf("控制服务退出: %v", err)
	}
}
