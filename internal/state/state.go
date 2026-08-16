// Package state 管理探测循环、目标状态（含防抖）、权重决策与路由切换。
package state

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"nexthop/internal/config"
	"nexthop/internal/probe"
)

// RouteUpdater 负责管理默认路由。
// 由 router 包实现；测试时注入 mock。
type RouteUpdater interface {
	// ReplaceDefault 把默认路由指向 gw。
	ReplaceDefault(gw net.IP) error
	// CurrentDefault 返回当前默认路由的网关；无默认路由返回 nil。
	CurrentDefault() (net.IP, error)
}

// TargetState 是单个目标的实时状态。
type TargetState struct {
	Target   config.Target
	Up       bool // 防抖后的稳定状态
	LastOK   bool
	LastRTT  time.Duration
	LastErr  string
	LastSeen time.Time

	// 防抖内部状态：连续探测到 pendingUp 方向的轮数。
	pending   int
	pendingUp bool
}

// Manager 驱动探测循环并维护整体状态。
type Manager struct {
	mu      sync.RWMutex
	cfg     *config.Config
	probers map[string]probe.Prober
	states  map[string]*TargetState
	updater RouteUpdater
	log     *slog.Logger

	currentNexthop string // 当前已生效的路由网关
	finalActive    bool
}

// New 构造 Manager，并立即构建探测器和状态表。
func New(cfg *config.Config, updater RouteUpdater, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		cfg:     cfg,
		probers: make(map[string]probe.Prober, len(cfg.Targets)),
		states:  make(map[string]*TargetState, len(cfg.Targets)),
		updater: updater,
		log:     log,
	}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		p, err := probe.New(*t, cfg.ProbeTimeout)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", t.Name, err)
		}
		m.probers[t.Name] = p
		m.states[t.Name] = &TargetState{Target: *t}
	}
	return m, nil
}

// Run 启动探测循环：立即执行一轮，此后按 probe_interval 周期性执行。
// 探测间隔在热加载后可动态变化，每轮结束后重新读取。
func (m *Manager) Run(ctx context.Context) {
	m.ProbeOnce(ctx)
	for {
		interval := m.interval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			m.ProbeOnce(ctx)
		}
	}
}

func (m *Manager) interval() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.ProbeInterval
}

// ProbeOnce 执行一轮探测、更新状态并视需要切换路由。
func (m *Manager) ProbeOnce(ctx context.Context) {
	m.mu.RLock()
	targets := make([]config.Target, len(m.cfg.Targets))
	copy(targets, m.cfg.Targets)
	probers := make(map[string]probe.Prober, len(m.probers))
	for k, v := range m.probers {
		probers[k] = v
	}
	timeout := m.cfg.ProbeTimeout
	m.mu.RUnlock()

	// 并行探测所有目标。用 slice 按索引收集结果（map 并发写不安全）。
	results := make([]probe.Result, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		idx := i
		t := &targets[idx]
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results[idx] = probers[t.Name].Probe(pctx)
		}()
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyResults(targets, results)
}

// applyResults 更新防抖状态并执行路由决策。调用方必须持有写锁。
func (m *Manager) applyResults(targets []config.Target, results []probe.Result) {
	now := time.Now()
	changed := false

	for i, t := range targets {
		st, ok := m.states[t.Name]
		if !ok {
			continue // 目标已被并发 Reload 移除，跳过
		}
		res := results[i]
		st.LastOK = res.OK
		st.LastRTT = res.RTT
		st.LastSeen = now
		if res.Error != nil {
			st.LastErr = res.Error.Error()
		} else {
			st.LastErr = ""
		}

		if res.OK != st.Up {
			// 变化候选：累计连续同方向探测轮数。
			if st.pending == 0 || st.pendingUp != res.OK {
				st.pending = 1
				st.pendingUp = res.OK
			} else {
				st.pending++
			}
			if st.pending >= m.cfg.StableRounds {
				st.Up = res.OK
				st.pending = 0
				changed = true
				m.log.Info("目标状态翻转",
					"target", t.Name, "up", st.Up,
					"rtt", st.LastRTT, "err", st.LastErr)
			}
		} else {
			st.pending = 0
		}
	}

	// 状态变化，或实际路由与期望不一致（含外部手动修改/删除）时收敛路由。
	if changed || !m.routeMatchesDesiredLocked() {
		m.reconcileRouteLocked()
	}
}

// routeMatchesDesiredLocked 检查实际默认路由是否与期望一致。
// 调用方必须持有写锁。
func (m *Manager) routeMatchesDesiredLocked() bool {
	desired := m.desiredNexthop()
	actual, err := m.updater.CurrentDefault()
	if err != nil {
		m.log.Error("读取实际默认路由失败", "err", err)
		return false // 读不到视为不一致，触发 reconcile（含重试）
	}
	if actual == nil {
		return false // 无默认路由，需要设置
	}
	return actual.String() == desired
}

// desiredNexthop 计算期望的网关：存活目标中权重最高者；全部失效则用 final_ip。
// 调用方必须持有锁。
func (m *Manager) desiredNexthop() string {
	for _, t := range m.cfg.SortedTargets() {
		if st, ok := m.states[t.Name]; ok && st.Up {
			return t.IP
		}
	}
	return m.cfg.FinalIP
}

// reconcileRouteLocked 确保实际路由与期望一致。调用方必须持有写锁。
func (m *Manager) reconcileRouteLocked() {
	desired := m.desiredNexthop()
	gw := net.ParseIP(desired)
	if gw == nil || gw.To4() == nil {
		m.log.Error("期望网关非法，跳过路由更新", "gw", desired)
		return
	}
	finalActive := desired == m.cfg.FinalIP
	if err := m.updater.ReplaceDefault(gw); err != nil {
		m.log.Error("切换默认路由失败（下轮重试）", "gw", desired, "err", err)
		return
	}
	m.currentNexthop = desired
	m.finalActive = finalActive
	m.log.Info("默认路由已切换", "gw", desired, "final", finalActive)
}

// Reload 应用新配置：重建探测器，保留仍存在目标的稳定状态，
// 新增目标初始为 down（需连续 stable_rounds 轮 up 才参与选路）。
// 新配置必须先通过 Validate。
func (m *Manager) Reload(cfg *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	newProbers := make(map[string]probe.Prober, len(cfg.Targets))
	newStates := make(map[string]*TargetState, len(cfg.Targets))
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		p, err := probe.New(*t, cfg.ProbeTimeout)
		if err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		newProbers[t.Name] = p
		if old, ok := m.states[t.Name]; ok {
			old.Target = *t // 配置可能变化（权重等）
			newStates[t.Name] = old
		} else {
			newStates[t.Name] = &TargetState{Target: *t}
		}
	}

	m.cfg = cfg
	m.probers = newProbers
	m.states = newStates
	m.log.Info("配置已热加载", "targets", len(cfg.Targets))
	// 目标集合变化后，若实际路由与期望不一致则立即收敛。
	if !m.routeMatchesDesiredLocked() {
		m.reconcileRouteLocked()
	}
	return nil
}

// TargetStatus 是导出给 control/CLI 的状态快照条目。
type TargetStatus struct {
	Name     string        `json:"name"`
	IP       string        `json:"ip"`
	Weight   int           `json:"weight"`
	Probe    string        `json:"probe"`
	Up       bool          `json:"up"`
	LastRTT  time.Duration `json:"last_rtt_ns"`
	LastErr  string        `json:"last_error,omitempty"`
	LastSeen time.Time     `json:"last_seen,omitempty"`
}

// Status 返回当前状态快照。
func (m *Manager) Status() (targets []TargetStatus, active string, finalActive bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, st := range m.states {
		targets = append(targets, TargetStatus{
			Name:     st.Target.Name,
			IP:       st.Target.IP,
			Weight:   st.Target.Weight,
			Probe:    string(st.Target.Probe),
			Up:       st.Up,
			LastRTT:  st.LastRTT,
			LastErr:  st.LastErr,
			LastSeen: st.LastSeen,
		})
	}
	return targets, m.currentNexthop, m.finalActive
}

// Config 返回当前生效配置（只读副本）。
func (m *Manager) Config() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}
