package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseValid(t *testing.T) {
	data := []byte(`
probe_interval: 1s
probe_timeout: 100ms
egress_device: eth0
stable_rounds: 2
final_ip: 10.0.0.254
targets:
  - name: a
    ip: 10.0.0.1
    weight: 100
    probe: ping
  - name: b
    ip: 10.0.0.2
    weight: 80
    probe: tcp
    port: 443
  - name: c
    ip: 1.1.1.1
    weight: 90
    probe: udp
    port: 53
  - name: d
    url: http://192.168.1.1/healthz
    weight: 70
    probe: http
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ProbeInterval != time.Second {
		t.Errorf("ProbeInterval = %v, want 1s", cfg.ProbeInterval)
	}
	if cfg.ProbeTimeout != 100*time.Millisecond {
		t.Errorf("ProbeTimeout = %v, want 100ms", cfg.ProbeTimeout)
	}
	if len(cfg.Targets) != 4 {
		t.Fatalf("len(Targets) = %d, want 4", len(cfg.Targets))
	}
}

func TestParseDefaults(t *testing.T) {
	data := []byte(`
egress_device: eth0
final_ip: 10.0.0.254
targets:
  - name: a
    ip: 10.0.0.1
    weight: 100
    probe: ping
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ProbeInterval != 5*time.Second {
		t.Errorf("默认 ProbeInterval = %v, want 5s", cfg.ProbeInterval)
	}
	if cfg.StableRounds != 2 {
		t.Errorf("默认 StableRounds = %d, want 2", cfg.StableRounds)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"interval 0", &Config{ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}}}, "probe_interval"},
		{"timeout >= interval", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Second, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}}}, "probe_timeout"},
		{"空 egress", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}}}, "egress_device"},
		{"final 非法", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "not-an-ip", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}}}, "final_ip"},
		{"final 是 v6", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "::1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}}}, "final_ip"},
		{"无 target", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1"}, "targets"},
		{"名字重复", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbePing}, {Name: "a", IP: "1.1.1.2", Weight: 1, Probe: ProbePing}}}, "重复"},
		{"负权重", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: -1, Probe: ProbePing}}}, "weight"},
		{"tcp 缺 port", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: ProbeTCP}}}, "port"},
		{"http 缺 url", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", Weight: 1, Probe: ProbeHTTP}}}, "url"},
		{"未知方法", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "1.1.1.1", Weight: 1, Probe: "smtp"}}}, "未知探测方式"},
		{"ip 是 v6", &Config{ProbeInterval: time.Second, ProbeTimeout: time.Millisecond, EgressDevice: "eth0", FinalIP: "1.1.1.1", Targets: []Target{{Name: "a", IP: "::1", Weight: 1, Probe: ProbePing}}}, "IPv4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate 未报错，期望包含 %q", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("错误 %q 不包含期望 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSortedTargetsStable(t *testing.T) {
	cfg := &Config{Targets: []Target{
		{Name: "a", Weight: 10},
		{Name: "b", Weight: 30},
		{Name: "c", Weight: 30},
		{Name: "d", Weight: 20},
	}}
	got := cfg.SortedTargets()
	want := []string{"b", "c", "d", "a"}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("SortedTargets[%d] = %s, want %s", i, got[i].Name, n)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		ProbeInterval: 3 * time.Second,
		ProbeTimeout:  500 * time.Millisecond,
		EgressDevice:  "eth0",
		StableRounds:  3,
		FinalIP:       "10.0.0.254",
		Targets: []Target{
			{Name: "a", IP: "10.0.0.1", Weight: 100, Probe: ProbePing},
			{Name: "b", IP: "10.0.0.2", Weight: 80, Probe: ProbeTCP, Port: 443},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProbeInterval != 3*time.Second || got.StableRounds != 3 || len(got.Targets) != 2 {
		t.Errorf("RoundTrip 不一致: %+v", got)
	}
	if got.Targets[1].Port != 443 {
		t.Errorf("Port 未保留: %+v", got.Targets[1])
	}
	// 目录里不应残留临时文件
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("目录里有 %d 个文件，期望 1（残留临时文件）", len(entries))
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && (s == sub || len(s) >= len(sub) && (s[:len(sub)] == sub || contains(s[1:], sub)))
}
