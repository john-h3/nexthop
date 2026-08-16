// Package control 提供 daemon 与 CLI 之间的本地 IPC：
// Unix domain socket + HTTP 协议，避免暴露任何网络端口。
package control

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"nexthop/internal/config"
	"nexthop/internal/state"
)

// Handler 是 daemon 提供给 control server 的能力。
// 由 cmd/nexthopd 实现。
type Handler interface {
	// Status 返回目标状态、当前生效网关与 final 是否激活。
	Status() (targets []state.TargetStatus, active string, finalActive bool)
	// Config 返回当前生效配置。
	Config() *config.Config
	// ConfigPath 返回配置文件路径。
	ConfigPath() string
	// ApplyConfig 校验并持久化新配置，然后热加载。
	ApplyConfig(data []byte) error
	// ReloadFromFile 从配置文件重新加载。
	ReloadFromFile() error
}

// StatusResponse 是 GET /api/status 的响应。
type StatusResponse struct {
	Targets       []state.TargetStatus `json:"targets"`
	ActiveNexthop string               `json:"active_nexthop"`
	FinalActive   bool                 `json:"final_active"`
}

// Server 是 unix socket HTTP 服务。
type Server struct {
	sockPath string
	handler  Handler
	log      *slog.Logger
	httpSrv  *http.Server
}

// NewServer 构造 control server。
func NewServer(sockPath string, h Handler, log *slog.Logger) *Server {
	s := &Server{sockPath: sockPath, handler: h, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/reload", s.handleReload)
	s.httpSrv = &http.Server{Handler: mux}
	return s
}

// Listen 创建 unix socket 监听器（清理旧 socket 文件）。
func (s *Server) Listen() (net.Listener, error) {
	if err := os.Remove(s.sockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("清理旧 socket %s: %w", s.sockPath, err)
	}
	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", s.sockPath, err)
	}
	return ln, nil
}

// Serve 在 ln 上提供 HTTP 服务（阻塞直到关闭）。
func (s *Server) Serve(ln net.Listener) error {
	err := s.httpSrv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown 优雅关闭并清理 socket 文件。
func (s *Server) Shutdown() {
	ctx, cancel := timeoutCtx(2 * time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
	_ = os.Remove(s.sockPath)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targets, active, final := s.handler.Status()
	writeJSON(w, StatusResponse{Targets: targets, ActiveNexthop: active, FinalActive: final})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := marshalYAML(s.handler.Config())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(data)
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "读取请求体失败", http.StatusBadRequest)
			return
		}
		if err := s.handler.ApplyConfig(body); err != nil {
			s.log.Warn("应用新配置失败", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log.Info("配置已通过 CLI 更新")
		writeJSON(w, map[string]string{"ok": "配置已保存并热加载"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.handler.ReloadFromFile(); err != nil {
		s.log.Warn("重载配置失败", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"ok": "配置已重新加载"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
