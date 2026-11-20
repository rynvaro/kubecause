// Package repair 负责审批后的修复。
//
// Slice 1 只留接口和占位，不执行任何修复。
// 这里把 Slice 3 要做的事写清楚，是为了面试时能讲明白
// “为什么现在不做、以后怎么做”，而不是留一个空目录。
package repair

import (
	"context"
	"fmt"

	"kubecause/internal/store"
)

// Proposal 是修复提案，和 change_requests 表一一对应。
// 一份提案只改一个东西：指定 Deployment 的指定容器的内存 limit。
// 字段和值都要由修复模块校验，不允许模型直接拼一个任意 Patch。
type Proposal struct {
	RunID      string
	Namespace  string
	Deployment string
	Container  string
	OldValue   string // 例如 128Mi
	NewValue   string // 例如 256Mi
}

// CreateProposal 生成一份待审批提案，写入 change_requests 表。
// 提案一旦生成就不可变：改目标、改前后值都要生成新提案，
// 不能原地改旧提案，这样审批时看到的东西和执行时用的东西才一致。
func CreateProposal(s *store.Store, p *Proposal) (*store.ChangeRequest, error) {
	if p.Namespace == "" || p.Deployment == "" || p.Container == "" {
		return nil, fmt.Errorf("提案缺少目标信息")
	}
	if p.OldValue == "" || p.NewValue == "" || p.OldValue == p.NewValue {
		return nil, fmt.Errorf("提案的新旧值非法")
	}
	// Slice 1 只支持内存 limit 这一类变更，值必须是 Mi 结尾的数量，
	// 防止以后有人把提案扩展成任意字段的 Patch。
	if !isMemoryValue(p.OldValue) || !isMemoryValue(p.NewValue) {
		return nil, fmt.Errorf("内存值必须是 Mi 结尾，例如 128Mi")
	}
	return s.CreateChangeRequest(context.Background(), &store.ChangeRequest{
		RunID: p.RunID, Namespace: p.Namespace, Deployment: p.Deployment,
		Container: p.Container, OldValue: p.OldValue, NewValue: p.NewValue,
	})
}

func isMemoryValue(v string) bool {
	if len(v) < 3 || v[len(v)-2:] != "Mi" {
		return false
	}
	for _, c := range v[:len(v)-2] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Execute 是 Slice 3 的入口，Slice 1 明确拒绝执行。
//
// Slice 3 的正确做法（先写在这里当占位说明）：
//  1. 重新读取目标 Deployment，校验 UID、容器名和当前旧值是否和提案一致，
//     不一致就把提案标为 expired，不执行；
//  2. 用本次读取的 resourceVersion 做条件 Patch，只改
//     spec.template.spec.containers[].resources.limits.memory 这一个字段，
//     防止并发覆盖把别人的改动冲掉；
//  3. 写入超时或中断时先读回目标状态，确认值是否已满足，
//     归因不明确时保留“执行结果未知”，不谎报成功；
//  4. 验证阶段独立检查 rollout，看新 Pod 是否 Ready、重启次数是否继续增长。
// 诊断和修复要用不同的 K8s 身份，写权限只限定在演示 namespace 的
// 指定 Deployment 上。
func Execute(s *store.Store, changeID string) error {
	return fmt.Errorf("Slice 1 不执行修复：提案 %s 已记录，等待 Slice 3 接入审批后执行", changeID)
}
