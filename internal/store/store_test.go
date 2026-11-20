package store

import (
	"context"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGetRun(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if r.ID == "" || r.Status != StatusQueued {
		t.Fatalf("任务初始状态不对: %+v", r)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.Name != "demo-api" {
		t.Fatalf("读取任务失败: %v %+v", err, got)
	}
}

func TestClaimAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	claimed, err := s.ClaimNext(ctx)
	if err != nil || claimed == nil || claimed.Status != StatusRunning {
		t.Fatalf("领取任务失败: %v %+v", err, claimed)
	}
	// 第二次领取应为空，同一任务不能被领两次。
	if again, _ := s.ClaimNext(ctx); again != nil {
		t.Fatalf("同一任务被重复领取")
	}
	// 模拟进程崩溃：running 的任务在重启后应改回 queued，步骤保留。
	n, err := s.ResetRunningToQueued(ctx)
	if err != nil || n != 1 {
		t.Fatalf("重启恢复失败: n=%d err=%v", n, err)
	}
	claimed2, err := s.ClaimNext(ctx)
	if err != nil || claimed2 == nil || claimed2.ID != r.ID {
		t.Fatalf("恢复后无法重新领取: %v %+v", err, claimed2)
	}
}

func TestAppendStepAndListAfter(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	for i := 1; i <= 3; i++ {
		seq, _ := s.NextSeq(ctx, r.ID)
		if seq != i {
			t.Fatalf("序号不对: 想要 %d 得到 %d", i, seq)
		}
		if err := s.AppendStep(ctx, &RunStep{RunID: r.ID, Seq: seq, Kind: "tool_result", ToolName: "get_pod", Summary: "ok"}); err != nil {
			t.Fatalf("写步骤失败: %v", err)
		}
	}
	after, err := s.ListStepsAfter(ctx, r.ID, 1)
	if err != nil || len(after) != 2 || after[0].Seq != 2 {
		t.Fatalf("增量拉取不对: %v %+v", err, after)
	}
}

func TestDecideApprovalIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	c, err := s.CreateChangeRequest(ctx, &ChangeRequest{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi"})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	decided, already, err := s.DecideApproval(ctx, c.ID, ChangeApproved, "varo")
	if err != nil || already || decided.Status != ChangeApproved {
		t.Fatalf("第一次审批不对: %v already=%v %+v", err, already, decided)
	}
	// 重复提交（甚至换一个决定）必须返回当前结果，不改状态。
	decided2, already2, err := s.DecideApproval(ctx, c.ID, ChangeRejected, "varo")
	if err != nil || !already2 || decided2.Status != ChangeApproved {
		t.Fatalf("重复审批应幂等返回已批准: %v already=%v %+v", err, already2, decided2)
	}
}
