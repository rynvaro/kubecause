# OOMKilled / 容器内存不足

症状：Pod 反复重启，`kubectl describe pod` 的 LastState 显示 Reason 为 OOMKilled，退出码 137。

判断：先确认 Deployment 的 `resources.limits.memory` 是否过低，再看应用是否有内存泄漏。日志里出现 `out of memory` 可以佐证，但不能只凭日志下结论，要和终止原因、Events 一起看。

处理：临时缓解是调高内存 limit（需人工审批）。根因仍需查应用内存曲线，确认调高后重启次数是否停止增长。
