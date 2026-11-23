package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.Chat.MaxSteps != 12 {
		t.Fatalf("对话默认步数应是 12，得到 %d", c.Chat.MaxSteps)
	}
	if c.Chat.MaxHistoryMessages != 10 {
		t.Fatalf("历史默认条数应是 10，得到 %d", c.Chat.MaxHistoryMessages)
	}
	if c.Run.MaxSteps != 8 {
		t.Fatalf("单次诊断默认步数应仍是 8，得到 %d", c.Run.MaxSteps)
	}
	if c.Run.TimeoutSeconds != 60 || c.Model.TimeoutSeconds != 30 {
		t.Fatalf("超时默认值不对: run=%d model=%d", c.Run.TimeoutSeconds, c.Model.TimeoutSeconds)
	}
	if c.Model.Temperature != 0.2 {
		t.Fatalf("温度默认值应是 0.2，得到 %v", c.Model.Temperature)
	}
	if c.Model.MaxTokens != 4096 {
		t.Fatalf("单次最大输出 Token 默认值应是 4096，得到 %d", c.Model.MaxTokens)
	}
	if c.Repair.MaxMemoryMi != 4096 || c.Repair.MaxRatio != 4 {
		t.Fatalf("修复上限默认值不对: maxMemoryMi=%d maxRatio=%d", c.Repair.MaxMemoryMi, c.Repair.MaxRatio)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("默认值本身应通过校验: %v", err)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("chat:\n  maxSteps: 16\n  maxHistoryMessages: 5\n"), 0644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if c.Chat.MaxSteps != 16 || c.Chat.MaxHistoryMessages != 5 {
		t.Fatalf("文件里的值应生效，得到 chat=%+v", c.Chat)
	}
	// 文件里没写的字段保持默认值，不被 YAML 零值覆盖。
	if c.Run.MaxSteps != 8 || c.Model.Temperature != 0.2 {
		t.Fatalf("没写的字段应保持默认，得到 run=%+v model=%+v", c.Run, c.Model)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "不存在.yaml"))
	if err != nil {
		t.Fatalf("缺文件时应用默认值、不报错: %v", err)
	}
	if c.Chat.MaxSteps != 12 {
		t.Fatalf("缺文件时对话步数应是默认 12，得到 %d", c.Chat.MaxSteps)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("chat:\n  maxSteps: 16\n"), 0644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	t.Setenv("CHAT_MAX_STEPS", "20")
	t.Setenv("MODEL_MAX_TOKENS", "2048")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if c.Chat.MaxSteps != 20 {
		t.Fatalf("环境变量应优先于文件，得到 %d", c.Chat.MaxSteps)
	}
	if c.Model.MaxTokens != 2048 {
		t.Fatalf("MODEL_MAX_TOKENS 应优先于文件，得到 %d", c.Model.MaxTokens)
	}
}

// TestValidateRejectsBadValues 覆盖配置中心 POST 的校验：
// 非法值（maxSteps=0、temperature=2）必须被挡住，不保存、不生效。
func TestValidateRejectsBadValues(t *testing.T) {
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"chat.maxSteps=0", func(c *Config) { c.Chat.MaxSteps = 0 }},
		{"chat.maxSteps=100", func(c *Config) { c.Chat.MaxSteps = 100 }},
		{"chat.maxHistoryMessages=0", func(c *Config) { c.Chat.MaxHistoryMessages = 0 }},
		{"run.maxSteps=0", func(c *Config) { c.Run.MaxSteps = 0 }},
		{"run.timeoutSeconds=1", func(c *Config) { c.Run.TimeoutSeconds = 1 }},
		{"model.timeoutSeconds=0", func(c *Config) { c.Model.TimeoutSeconds = 0 }},
		{"model.temperature=2", func(c *Config) { c.Model.Temperature = 2 }},
		{"model.temperature=-0.1", func(c *Config) { c.Model.Temperature = -0.1 }},
		{"model.maxTokens=-1", func(c *Config) { c.Model.MaxTokens = -1 }},
		{"model.maxTokens=40000", func(c *Config) { c.Model.MaxTokens = 40000 }},
		{"repair.maxMemoryMi=64", func(c *Config) { c.Repair.MaxMemoryMi = 64 }},
		{"repair.maxRatio=0", func(c *Config) { c.Repair.MaxRatio = 0 }},
		{"repair.maxRatio=99", func(c *Config) { c.Repair.MaxRatio = 99 }},
	}
	for _, tc := range bad {
		c := Defaults()
		tc.mut(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s 应校验失败", tc.name)
		}
	}
}

// TestValidateAllowsZeroMaxTokens：maxTokens=0 是合法的“不传”——
// 0 表示不发 max_tokens 字段、走模型默认，不是非法值。
func TestValidateAllowsZeroMaxTokens(t *testing.T) {
	c := Defaults()
	c.Model.MaxTokens = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("maxTokens=0 应通过校验: %v", err)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := Defaults()
	c.Chat.MaxSteps = 16
	c.Model.Temperature = 0.5
	if err := Save(path, c); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("重新读配置失败: %v", err)
	}
	if got.Chat.MaxSteps != 16 || got.Model.Temperature != 0.5 {
		t.Fatalf("保存后再读应一致，得到 %+v", got)
	}
	// 非法值不落盘：磁盘文件保持旧内容。
	bad := Defaults()
	bad.Chat.MaxSteps = 0
	if err := Save(path, bad); err == nil {
		t.Fatalf("非法值保存应失败")
	}
	got, _ = Load(path)
	if got.Chat.MaxSteps != 16 {
		t.Fatalf("非法保存后文件应保持旧值 16，得到 %d", got.Chat.MaxSteps)
	}
}

func TestManagerHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	mgr := NewManager(Defaults(), path)
	if mgr.Current().Chat.MaxSteps != 12 {
		t.Fatalf("初始值应是 12")
	}
	next := Defaults()
	next.Chat.MaxSteps = 16
	if err := mgr.Update(next); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if mgr.Current().Chat.MaxSteps != 16 {
		t.Fatalf("热更新后当前值应是 16，得到 %d", mgr.Current().Chat.MaxSteps)
	}
	// 文件也写回去了，重启后读到的就是新值。
	got, err := Load(path)
	if err != nil || got.Chat.MaxSteps != 16 {
		t.Fatalf("热更新后文件应也是 16，得到 %+v / %v", got, err)
	}
	// 非法更新不换内存值、不写文件。
	bad := Defaults()
	bad.Model.Temperature = 2
	if err := mgr.Update(bad); err == nil {
		t.Fatalf("非法更新应失败")
	}
	if mgr.Current().Chat.MaxSteps != 16 || mgr.Current().Model.Temperature != 0.2 {
		t.Fatalf("非法更新后内存值应保持不变，得到 %+v", mgr.Current())
	}
}

func TestDefaultsSummary(t *testing.T) {
	c := Defaults()
	if !c.Chat.Summary.Enabled {
		t.Fatalf("摘要默认应开启")
	}
	if c.Chat.Summary.TriggerMessages != 10 {
		t.Fatalf("摘要触发阈值默认应是 10，得到 %d", c.Chat.Summary.TriggerMessages)
	}
}

func TestValidateRejectsBadSummaryTrigger(t *testing.T) {
	for _, v := range []int{0, 101} {
		c := Defaults()
		c.Chat.Summary.TriggerMessages = v
		if err := c.Validate(); err == nil {
			t.Fatalf("triggerMessages=%d 应被校验拒绝", v)
		}
	}
}

func TestLoadEnvSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("chat:\n  maxSteps: 16\n"), 0644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	t.Setenv("CHAT_SUMMARY_ENABLED", "false")
	t.Setenv("CHAT_SUMMARY_TRIGGER_MESSAGES", "25")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if c.Chat.Summary.Enabled {
		t.Fatalf("CHAT_SUMMARY_ENABLED=false 应关掉摘要")
	}
	if c.Chat.Summary.TriggerMessages != 25 {
		t.Fatalf("CHAT_SUMMARY_TRIGGER_MESSAGES 应是 25，得到 %d", c.Chat.Summary.TriggerMessages)
	}
}

func TestLoadEnvSummaryBadBoolIgnored(t *testing.T) {
	// 环境变量布尔值解析失败时保留文件或默认值，不报错、不关功能。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("chat:\n  maxSteps: 16\n"), 0644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	t.Setenv("CHAT_SUMMARY_ENABLED", "bogus")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if !c.Chat.Summary.Enabled {
		t.Fatalf("非法布尔值应被忽略、摘要保持开启")
	}
}

func TestLoadOldFileKeepsSummaryDefaults(t *testing.T) {
	// 老 config.yaml 里没有 summary 一节：按默认值解码，
	// 摘要保持开启、阈值 10，而不是被 YAML 零值关掉。
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("chat:\n  maxSteps: 16\n"), 0644); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if !c.Chat.Summary.Enabled || c.Chat.Summary.TriggerMessages != 10 {
		t.Fatalf("老文件应保持摘要默认开启 / 阈值 10，得到 %+v", c.Chat.Summary)
	}
}
