// Package config 负责 nexthop 服务配置的加载、校验与原子持久化。
//
// 配置文件为 YAML 格式，支持 Go duration 写法（如 1ms / 5s / 1m）。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ProbeMethod 是连通性探测方式。
type ProbeMethod string

const (
	ProbePing ProbeMethod = "ping"
	ProbeTCP  ProbeMethod = "tcp"
	ProbeUDP  ProbeMethod = "udp"
	ProbeHTTP ProbeMethod = "http"
)

// Config 是服务的完整配置。
type Config struct {
	// ProbeInterval 每轮探测的间隔（支持 1ms/1s/1m 写法）。
	ProbeInterval time.Duration `yaml:"probe_interval"`
	// ProbeTimeout 单次探测的超时时间，必须小于 ProbeInterval。
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	// EgressDevice 出口网卡，default route 绑定的 dev。
	EgressDevice string `yaml:"egress_device"`
	// StableRounds 防抖：目标状态需连续 N 轮探测保持才翻转。
	StableRounds int `yaml:"stable_rounds"`
	// FinalIP 全部上游失效时的兜底下一跳。
	FinalIP string `yaml:"final_ip"`
	// Targets 候选上游列表。
	Targets []Target `yaml:"targets"`
}

// Target 是一个下一跳候选。
type Target struct {
	Name   string      `yaml:"name"`
	IP     string      `yaml:"ip"`
	Weight int         `yaml:"weight"`
	Probe  ProbeMethod `yaml:"probe"`
	Port   int         `yaml:"port,omitempty"` // tcp/udp 使用
	URL    string      `yaml:"url,omitempty"`  // http 使用
}

// Defaults 返回带默认值的配置骨架。
func Defaults() *Config {
	return &Config{
		ProbeInterval: 5 * time.Second,
		ProbeTimeout:  1 * time.Second,
		StableRounds:  2,
	}
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	return Parse(data)
}

// Parse 解析并校验配置内容。
func Parse(data []byte) (*Config, error) {
	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验配置的完整性。
func (c *Config) Validate() error {
	var errs []error

	if c.ProbeInterval <= 0 {
		errs = append(errs, errors.New("probe_interval 必须为正数"))
	}
	if c.ProbeTimeout <= 0 {
		errs = append(errs, errors.New("probe_timeout 必须为正数"))
	}
	if c.ProbeInterval > 0 && c.ProbeTimeout >= c.ProbeInterval {
		errs = append(errs, errors.New("probe_timeout 必须小于 probe_interval（避免探测轮次重叠）"))
	}
	if c.StableRounds < 1 {
		errs = append(errs, errors.New("stable_rounds 必须 >= 1"))
	}
	if strings.TrimSpace(c.EgressDevice) == "" {
		errs = append(errs, errors.New("egress_device 不能为空"))
	}
	if c.FinalIP == "" {
		errs = append(errs, errors.New("final_ip 不能为空"))
	} else if ip := net.ParseIP(c.FinalIP); ip == nil || ip.To4() == nil {
		errs = append(errs, fmt.Errorf("final_ip %q 不是合法的 IPv4 地址", c.FinalIP))
	}
	if len(c.Targets) == 0 {
		errs = append(errs, errors.New("targets 不能为空"))
	}

	names := make(map[string]bool, len(c.Targets))
	for i := range c.Targets {
		t := &c.Targets[i]
		if err := t.validate(); err != nil {
			errs = append(errs, fmt.Errorf("targets[%d] (%s): %w", i, t.Name, err))
		}
		if t.Name != "" {
			if names[t.Name] {
				errs = append(errs, fmt.Errorf("target 名字 %q 重复", t.Name))
			}
			names[t.Name] = true
		}
	}

	return errors.Join(errs...)
}

func (t *Target) validate() error {
	var errs []error

	if strings.TrimSpace(t.Name) == "" {
		errs = append(errs, errors.New("name 不能为空"))
	}
	if t.Weight < 0 {
		errs = append(errs, fmt.Errorf("weight %d 不能为负数", t.Weight))
	}

	switch t.Probe {
	case ProbePing:
		if t.IP == "" {
			errs = append(errs, errors.New("ping 探测需要 ip"))
		} else if ip := net.ParseIP(t.IP); ip == nil || ip.To4() == nil {
			errs = append(errs, fmt.Errorf("ip %q 不是合法的 IPv4 地址", t.IP))
		}
	case ProbeTCP, ProbeUDP:
		if t.IP == "" {
			errs = append(errs, errors.New(string(t.Probe)+" 探测需要 ip"))
		} else if ip := net.ParseIP(t.IP); ip == nil || ip.To4() == nil {
			errs = append(errs, fmt.Errorf("ip %q 不是合法的 IPv4 地址", t.IP))
		}
		if t.Port < 1 || t.Port > 65535 {
			errs = append(errs, fmt.Errorf("port %d 不合法（1-65535）", t.Port))
		}
	case ProbeHTTP:
		if t.URL == "" {
			errs = append(errs, errors.New("http 探测需要 url"))
		}
	default:
		errs = append(errs, fmt.Errorf("未知探测方式 %q（支持 ping/tcp/udp/http）", t.Probe))
	}

	return errors.Join(errs...)
}

// Save 原子写回配置文件（临时文件 + rename），避免半写状态。
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nexthop-config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("设置权限: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("替换配置文件: %w", err)
	}
	return nil
}

// SortedTargets 返回按权重降序（同权重保持配置顺序）的副本，供决策使用。
func (c *Config) SortedTargets() []Target {
	out := make([]Target, len(c.Targets))
	copy(out, c.Targets)
	// 稳定排序：相等权重保持原顺序。
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Weight > out[j].Weight
	})
	return out
}
