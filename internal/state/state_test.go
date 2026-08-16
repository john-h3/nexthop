package state

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"nexthop/internal/config"
	"nexthop/internal/probe"
)

// fakeProber 按顺序循环返回预设结果。
type fakeProber struct {
	mu      sync.Mutex
	results []probe.Result
	calls   int
}

func (f *fakeProber) Probe(ctx context.Context) probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.results[f.calls%len(f.results)]
	f.calls++
	return res
}

func up() probe.Result  { return probe.Result{OK: true, RTT: time.Millisecond} }
func down() probe.Result {
	return probe.Result{OK: false, RTT: time.Millisecond, Error: errFake}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (e *fakeErr) Error() string { return "fake down" }

// fakeUpdater 记录所有路由切换调用（无论成败），
// 并用 current 模拟内核路由表的当前实际默认路由。
type fakeUpdater struct {
	mu      sync.Mutex
	current net.IP // 模拟当前实际默认路由网关
	calls   []net.IP
	err     error
}

func (f *fakeUpdater) ReplaceDefault(gw net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, gw)
	if f.err != nil {
		return f.err
	}
	f.current = gw
	return nil
}

func (f *fakeUpdater) CurrentDefault() (net.IP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *fakeUpdater) last() net.IP {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeUpdater) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func baseConfig() *config.Config {
	return &config.Config{
		ProbeInterval: time.Second,
		ProbeTimeout:  100 * time.Millisecond,
		EgressDevice:  "eth0",
		StableRounds:  2,
		FinalIP:       "10.0.0.254",
		Targets: []config.Target{
			{Name: "a", IP: "10.0.0.1", Weight: 100, Probe: config.ProbePing},
			{Name: "b", IP: "10.0.0.2", Weight: 50, Probe: config.ProbePing},
		},
	}
}

// newTestManager 构造带 fake prober 的 Manager。
func newTestManager(t *testing.T, cfg *config.Config, results map[string][]probe.Result) (*Manager, *fakeUpdater) {
	t.Helper()
	updater := &fakeUpdater{}
	m := &Manager{
		cfg:     cfg,
		probers: make(map[string]probe.Prober, len(cfg.Targets)),
		states:  make(map[string]*TargetState, len(cfg.Targets)),
		updater: updater,
		log:     quietLog(),
	}
	for i := range cfg.Targets {
		tgt := &cfg.Targets[i]
		m.probers[tgt.Name] = &fakeProber{results: results[tgt.Name]}
		m.states[tgt.Name] = &TargetState{Target: *tgt}
	}
	return m, updater
}

func TestNewTargetRequiresStableRounds(t *testing.T) {
	cfg := baseConfig()
	// 目标 a：前 stable_rounds 轮 up；目标 b：一直 down。
	resA := make([]probe.Result, 0)
	for i := 0; i < cfg.StableRounds; i++ {
		resA = append(resA, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resA,
		"b": {down(), down(), down()},
	})

	// 第 1 轮：a 尚未稳定 up，无存活目标 → 先切到 final 兜底。
	m.ProbeOnce(context.Background())
	if updater.count() != 1 {
		t.Fatalf("第 1 轮应切到 final 兜底，实际调用 %d 次", updater.count())
	}
	if got := updater.last().String(); got != "10.0.0.254" {
		t.Fatalf("第 1 轮期望 final 10.0.0.254，实际 %s", got)
	}

	// 第 2 轮：a 连续两轮 up 稳定 → 切到 a。
	m.ProbeOnce(context.Background())
	if updater.count() != 2 {
		t.Fatalf("第 2 轮应切换到 a，实际调用 %d 次", updater.count())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("期望切到 10.0.0.1，实际 %s", got)
	}
}

func TestWeightSelection(t *testing.T) {
	cfg := baseConfig()
	// 两个目标都 up（各自达到稳定）。
	res := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		res = append(res, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": res, "b": res,
	})

	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("权重高者（a:100）应被选中，实际 %s", got)
	}
}

func TestAllDownUseFinal(t *testing.T) {
	cfg := baseConfig()
	// 先让 a 稳定 up，然后 a、b 都 down。
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": append(resUp, down(), down(), down()),
		"b": {down()},
	})

	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("a 稳定后应切到 10.0.0.1，实际 %s", got)
	}

	// a 连续 down 两轮（防抖阈值）后应切到 final。
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("a 只 down 一轮不应切换，实际 %s", got)
	}
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.254" {
		t.Fatalf("a 连续 down 两轮后应切到 final 10.0.0.254，实际 %s", got)
	}
	if _, _, final := m.Status(); !final {
		t.Fatal("finalActive 应为 true")
	}
}

func TestRecoverFromFinal(t *testing.T) {
	cfg := baseConfig()
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	// a：先 up 稳定，然后 down，然后恢复 up 稳定。
	resA := append(append([]probe.Result{}, resUp...), down(), down(), up(), up())
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resA,
		"b": {down()},
	})

	// a 稳定 up
	m.ProbeOnce(context.Background())
	m.ProbeOnce(context.Background())
	// a down 两轮 → final
	m.ProbeOnce(context.Background())
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.254" {
		t.Fatalf("应处于 final，实际 %s", got)
	}
	// a 恢复 up：第一轮 up 不切，第二轮 up 才切回
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.254" {
		t.Fatalf("恢复第一轮不应切换，实际 %s", got)
	}
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("恢复稳定后应切回 10.0.0.1，实际 %s", got)
	}
	if _, _, final := m.Status(); final {
		t.Fatal("finalActive 应为 false")
	}
}

func TestReloadPreservesState(t *testing.T) {
	cfg := baseConfig()
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resUp, "b": {down()},
	})
	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("预置路由未生效: %s", got)
	}

	// 热加载：保留 a、b，新增 c（权重最高）。
	newCfg := baseConfig()
	newCfg.Targets = append(newCfg.Targets, config.Target{Name: "c", IP: "10.0.0.3", Weight: 200, Probe: config.ProbePing})
	// 新配置需先通过 Validate（探测方式合法即可）。
	if err := newCfg.Validate(); err != nil {
		t.Fatalf("新配置校验失败: %v", err)
	}
	if err := m.Reload(newCfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Reload 后 a 的状态保留（up），c 初始 down → 期望仍是 a。
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("Reload 后应保持 a，实际 %s", got)
	}
	// a 保持 up，c 持续 up：两轮后 c（权重 200）应胜出。
	resC := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resC = append(resC, up())
	}
	m.probers["c"] = &fakeProber{results: resC}
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("c 第一轮 up 不应切换，实际 %s", got)
	}
	m.ProbeOnce(context.Background())
	if got := updater.last().String(); got != "10.0.0.3" {
		t.Fatalf("c 稳定 up 后应按权重切到 10.0.0.3，实际 %s", got)
	}
}

func TestUpdaterFailureRetries(t *testing.T) {
	cfg := baseConfig()
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resUp, "b": {down()},
	})
	updater.err = errFake // 路由更新持续失败

	// 第 1 轮：切 final 失败；第 2 轮：a 稳定后切 a 失败 → 每轮都重试。
	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}
	if updater.count() != cfg.StableRounds {
		t.Fatalf("失败后应每轮重试，实际调用 %d 次", updater.count())
	}

	// 更新成功：当前 currentNexthop 仍为空，应立即收敛一次。
	updater.err = nil
	m.ProbeOnce(context.Background())
	if updater.count() != cfg.StableRounds+1 {
		t.Fatalf("恢复后应立即收敛，实际调用 %d 次", updater.count())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("收敛应切到 10.0.0.1，实际 %s", got)
	}

	// 成功后：路由已生效，不再重复切换。
	m.ProbeOnce(context.Background())
	if updater.count() != cfg.StableRounds+1 {
		t.Fatalf("成功后不应多余切换，实际调用 %d 次", updater.count())
	}
}

func TestStatusSnapshot(t *testing.T) {
	cfg := baseConfig()
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	m, _ := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resUp, "b": {down()},
	})
	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}

	targets, active, final := m.Status()
	if len(targets) != 2 {
		t.Fatalf("Status targets = %d, want 2", len(targets))
	}
	byName := map[string]TargetStatus{}
	for _, ts := range targets {
		byName[ts.Name] = ts
	}
	if !byName["a"].Up {
		t.Error("a 应为 up")
	}
	if byName["b"].Up {
		t.Error("b 应为 down")
	}
	if active != "10.0.0.1" {
		t.Errorf("active = %s, want 10.0.0.1", active)
	}
	if final {
		t.Error("final 不应激活")
	}
}

// 外部修改/删除默认路由后，daemon 应在下一轮探测时恢复（路由自愈）。
func TestRouteSelfHeal(t *testing.T) {
	cfg := baseConfig()
	resUp := make([]probe.Result, 0, cfg.StableRounds)
	for i := 0; i < cfg.StableRounds; i++ {
		resUp = append(resUp, up())
	}
	m, updater := newTestManager(t, cfg, map[string][]probe.Result{
		"a": resUp, "b": {down()},
	})
	for i := 0; i < cfg.StableRounds; i++ {
		m.ProbeOnce(context.Background())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("预置路由未生效: %s", got)
	}

	// 外部修改路由（模拟 ip route replace default via 其他网关）。
	updater.mu.Lock()
	updater.current = net.ParseIP("192.168.1.1")
	updater.mu.Unlock()

	before := updater.count()
	m.ProbeOnce(context.Background())
	if updater.count() != before+1 {
		t.Fatalf("外部修改后应恢复路由，实际调用 %d -> %d 次", before, updater.count())
	}
	if got := updater.last().String(); got != "10.0.0.1" {
		t.Fatalf("应恢复路由到 10.0.0.1，实际 %s", got)
	}

	// 外部删除路由（模拟 ip route del default）。
	updater.mu.Lock()
	updater.current = nil
	updater.mu.Unlock()
	before = updater.count()
	m.ProbeOnce(context.Background())
	if updater.count() != before+1 {
		t.Fatalf("外部删除后应恢复路由，实际调用 %d -> %d 次", before, updater.count())
	}

	// 路由与期望一致时不应重复设置。
	before = updater.count()
	m.ProbeOnce(context.Background())
	if updater.count() != before {
		t.Fatalf("路由一致时不应重复设置，实际调用 %d -> %d 次", before, updater.count())
	}
}
