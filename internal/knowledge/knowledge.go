// Package knowledge 提供排障手册检索。
//
// Slice 1 用内存里的 Markdown 片段做关键词检索，不引入向量数据库。
// 原因：首批只有 OOM、镜像拉取失败、探针失败三类，语料很小，
// 关键词加别名已经够准，而且结果确定、可解释，面试时好讲。
// Slice 2 可以把同样的 Markdown 建成 SQLite FTS5 索引，
// 接口 Search 不变，Agent 和工具都不用改。
package knowledge

import (
	"strings"
)

// Snippet 是一条带来源的检索结果。
type Snippet struct {
	Title   string `json:"title"`
	Source  string `json:"source"`
	Content string `json:"content"`
}

// 手册条目：关键词和别名用于匹配，内容是给模型看的判断要点。
type entry struct {
	title    string
	source   string
	content  string
	keywords []string
}

var entries = []entry{
	{
		title:   "OOMKilled / 容器内存不足",
		source:  "runbooks/oom.md",
		content: "容器因内存超限被内核杀掉，退出码 137，Reason 为 OOMKilled，LastState.Terminated 有记录。先确认 Deployment 的 resources.limits.memory 是否过低，再看应用是否有内存泄漏。临时缓解是调高 limit，根因仍需查应用内存曲线。",
		keywords: []string{"oomkilled", "oom", "out of memory", "137", "内存", "重启", "restart"},
	},
	{
		title:   "镜像拉取失败 ImagePullBackOff",
		source:  "runbooks/imagepull.md",
		content: "Pod 卡在 ImagePullBackOff，通常是镜像名或 tag 写错、镜像仓库不可达、或 imagePullSecrets 缺失。先查 Events 里的 Failed to pull image，再核对 Deployment 的 image 字段和 Secret。",
		keywords: []string{"imagepullbackoff", "imagepull", "镜像", "pull", "ErrImagePull", "Failed to pull"},
	},
	{
		title:   "探针失败 / Liveness / Readiness",
		source:  "runbooks/probe.md",
		content: "Liveness 探针失败会导致容器被重启，Readiness 失败会让 Pod 脱离 Service。先看探针的路径、端口和超时，再看应用启动耗时是否超过 initialDelaySeconds，日志里是否有启动失败。",
		keywords: []string{"probe", "liveness", "readiness", "探针", "健康检查", "unhealthy"},
	},
}

// Search 按关键词和别名返回最多 3 条相关片段。
// 匹配不区分大小写，query 为空返回空结果，不返回全部手册。
func Search(query string) []Snippet {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var out []Snippet
	for _, e := range entries {
		for _, kw := range e.keywords {
			if strings.Contains(q, strings.ToLower(kw)) || strings.Contains(strings.ToLower(kw), q) {
				out = append(out, Snippet{Title: e.title, Source: e.source, Content: e.content})
				break
			}
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}
