// Package config 是 KubeCause 的轻量配置中心（Slice 8）。
//
// 只做一件事：把原来散落在代码里的几个写死数字（对话步数、历史条数、
// 单次诊断步数/超时、模型超时/温度、修复上限）集中到一个 config.yaml，
// 启动时读文件、环境变量可覆盖、改完通过 POST /api/config 热加载，
// 不用重启服务。
//
// 刻意不做的事：
//   - 不存 MODEL_API_KEY。Key 只从环境变量读，绝不进配置文件、
//     绝不在 GET /api/config 里返回、绝不在前端显示。
//     原因：配置文件会被提交进仓库、被打包进 zip，一旦写进 Key
//     就等于公开泄露；热加载改 Key 也没有意义，改 Key 本来就该重启。
//   - 不做鉴权、配置版本、多环境切换。那是部署层面的事，
//     这个配置中心只服务单机演示和个人使用。
package config

import (
	"fmt"
	"os"
	"strconv"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config 是全部可调参数。JSON 字段名和前端 Settings 页一一对应，
// YAML 字段名是小驼峰，config.yaml 里有中文注释逐项说明。
type Config struct {
	Chat struct {
		// MaxSteps 是一轮对话里模型最多调工具的次数。
		// 它是预算，不是决策：调大它不干预模型怎么选工具，
		// 只是允许模型在一轮里查得更多；步数用完时后端返回
		// 已查到的半成品，而不是直接报错（见 internal/chat/real.go）。
		MaxSteps int `yaml:"maxSteps" json:"maxSteps"`
		// MaxHistoryMessages 是每次调用模型时带上的最近用户/助手消息数，
		// 只算 user 和 assistant，tool 消息不占名额。
		// 更早的消息数据库里全量保留，只是暂时不进模型上下文。
		MaxHistoryMessages int `yaml:"maxHistoryMessages" json:"maxHistoryMessages"`
	} `yaml:"chat" json:"chat"`
	Run struct {
		// MaxSteps 是单次诊断（/api/runs）最多调工具的次数，默认 8，
		// 和对话的 12 步是分开的两个预算，不要混成一个。
		MaxSteps int `yaml:"maxSteps" json:"maxSteps"`
		// TimeoutSeconds 是单次诊断的整体超时秒数。
		TimeoutSeconds int `yaml:"timeoutSeconds" json:"timeoutSeconds"`
	} `yaml:"run" json:"run"`
	Model struct {
		// TimeoutSeconds 是每次模型 API 调用的超时秒数。
		TimeoutSeconds int `yaml:"timeoutSeconds" json:"timeoutSeconds"`
		// Temperature 是模型温度，0-1。诊断场景默认 0.2，
		// 温度越低模型越按证据说话、越少自由发挥。
		Temperature float64 `yaml:"temperature" json:"temperature"`
	} `yaml:"model" json:"model"`
	Repair struct {
		// MaxMemoryMi 是修复建议允许的新值上限（MiB），默认 4096 即 4Gi。
		MaxMemoryMi int64 `yaml:"maxMemoryMi" json:"maxMemoryMi"`
		// MaxRatio 是修复新值相对旧值的最大倍数，默认 4，
		// 即新值不超过旧值的 4 倍。两个上限取更严的那一个。
		MaxRatio int64 `yaml:"maxRatio" json:"maxRatio"`
	} `yaml:"repair" json:"repair"`
}

// Defaults 返回默认配置：和 Slice 7 及以前的行为一致，
// 只有对话步数从 8 改成 12（Slice 8 的决定，见 README）。
func Defaults() Config {
	var c Config
	c.Chat.MaxSteps = 12
	c.Chat.MaxHistoryMessages = 10
	c.Run.MaxSteps = 8
	c.Run.TimeoutSeconds = 60
	c.Model.TimeoutSeconds = 30
	c.Model.Temperature = 0.2
	c.Repair.MaxMemoryMi = 4096
	c.Repair.MaxRatio = 4
	return c
}

// Validate 校验每个参数的范围。范围写死在这里是故意的：
// 配置中心允许调参，但不允许调出一个让服务直接失控的值
// （例如 maxSteps=0 会让对话一步工具都调不了，
// temperature=2 超出模型协议允许的范围）。
// 校验失败时调用方不保存、不生效，旧配置继续跑。
func (c Config) Validate() error {
	check := func(name string, v, min, max int) error {
		if v < min || v > max {
			return fmt.Errorf("%s 必须在 %d-%d 之间，得到 %d", name, min, max, v)
		}
		return nil
	}
	if err := check("chat.maxSteps", c.Chat.MaxSteps, 1, 50); err != nil {
		return err
	}
	if err := check("chat.maxHistoryMessages", c.Chat.MaxHistoryMessages, 1, 50); err != nil {
		return err
	}
	if err := check("run.maxSteps", c.Run.MaxSteps, 1, 50); err != nil {
		return err
	}
	if err := check("run.timeoutSeconds", c.Run.TimeoutSeconds, 5, 600); err != nil {
		return err
	}
	if err := check("model.timeoutSeconds", c.Model.TimeoutSeconds, 5, 300); err != nil {
		return err
	}
	if c.Model.Temperature < 0 || c.Model.Temperature > 1 {
		return fmt.Errorf("model.temperature 必须在 0-1 之间，得到 %v", c.Model.Temperature)
	}
	if c.Repair.MaxMemoryMi < 128 || c.Repair.MaxMemoryMi > 16384 {
		return fmt.Errorf("repair.maxMemoryMi 必须在 128-16384 之间，得到 %d", c.Repair.MaxMemoryMi)
	}
	if c.Repair.MaxRatio < 1 || c.Repair.MaxRatio > 10 {
		return fmt.Errorf("repair.maxRatio 必须在 1-10 之间，得到 %d", c.Repair.MaxRatio)
	}
	return nil
}

// envInt / envFloat 从环境变量读一个值，读不到或解析失败返回 ok=false，
// 调用方保留文件或默认值。环境变量优先于文件，方便部署时改，
// 而不用去改仓库里的 config.yaml。
func envInt(key string) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func envFloat(key string) (float64, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// applyEnv 把环境变量覆盖到配置上。环境变量优先于文件，
// 但只覆盖明确设置了的变量，没设置的字段保持文件或默认值。
func applyEnv(c *Config) {
	if v, ok := envInt("CHAT_MAX_STEPS"); ok {
		c.Chat.MaxSteps = v
	}
	if v, ok := envInt("CHAT_MAX_HISTORY_MESSAGES"); ok {
		c.Chat.MaxHistoryMessages = v
	}
	if v, ok := envInt("RUN_MAX_STEPS"); ok {
		c.Run.MaxSteps = v
	}
	if v, ok := envInt("RUN_TIMEOUT_SECONDS"); ok {
		c.Run.TimeoutSeconds = v
	}
	if v, ok := envInt("MODEL_TIMEOUT_SECONDS"); ok {
		c.Model.TimeoutSeconds = v
	}
	if v, ok := envFloat("MODEL_TEMPERATURE"); ok {
		c.Model.Temperature = v
	}
	if v, ok := envInt("REPAIR_MAX_MEMORY_MI"); ok {
		c.Repair.MaxMemoryMi = int64(v)
	}
	if v, ok := envInt("REPAIR_MAX_RATIO"); ok {
		c.Repair.MaxRatio = int64(v)
	}
}

// Load 从 path 读配置文件，文件不存在时用默认值，
// 再用环境变量覆盖，最后校验。校验失败返回错误，
// 调用方（main.go）应让启动失败，而不是带着非法配置跑起来。
func Load(path string) (Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 没有配置文件不是错误：用默认值跑，部署时再补文件。
			applyEnv(&c)
			return c, c.Validate()
		}
		return c, fmt.Errorf("读配置文件失败: %w", err)
	}
	// 先按默认值解码：文件里只写了部分字段时，没写的字段保持默认，
	// 而不是被 YAML 的零值（0）覆盖掉。
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("解析配置文件失败: %w", err)
	}
	applyEnv(&c)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// Save 把配置写回文件。先校验再写：非法值不落盘，
// 磁盘上的旧文件保持可用，服务继续用旧配置跑。
// 文件用 0644 权限写，里面不含任何密钥，提交进仓库也没关系。
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(&c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写配置文件失败: %w", err)
	}
	return nil
}

// Manager 持有当前生效的配置，支持热加载：
// POST /api/config 改完后直接换掉内存里的值，
// 正在跑的服务下一次读配置就拿到新值，不用重启。
// 文件路径记在 Manager 里，Save 时写回同一个文件。
type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

// NewManager 用已加载的配置创建一个 Manager。
func NewManager(cfg Config, path string) *Manager {
	return &Manager{cfg: cfg, path: path}
}

// Current 返回当前生效配置的一份拷贝。
func (m *Manager) Current() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Path 返回配置文件路径。
func (m *Manager) Path() string { return m.path }

// Update 校验新配置、写回文件、再换掉内存里的当前值。
// 三步任何一步失败都不换内存值，服务继续用旧配置跑。
// 注意：环境变量覆盖不进文件——文件里保存的是用户在前端改的值，
// 下次启动时环境变量仍会优先覆盖它，这是有意的：
// 部署时用环境变量定死的优先级最高。
func (m *Manager) Update(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := Save(m.path, c); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
	return nil
}
