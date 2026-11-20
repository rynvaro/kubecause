# 资源配额不足 / ResourceQuota

症状：Deployment 扩容或新建 Pod 时被拒绝，Events 里出现 exceeded quota，Pod 卡在 Pending 或创建失败。

判断：先看 namespace 的 ResourceQuota 和 LimitRange：`kubectl describe quota` 能看到已用和上限。再看是哪个资源超了：CPU、内存、Pod 数量、还是 Service / ConfigMap 个数。

处理：调小单 Pod 的 requests，或向管理员申请调高配额；不要绕过配额去别的 namespace 建资源。
