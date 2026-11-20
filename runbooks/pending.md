# Pending / Pod 调度失败

症状：Pod 长时间处于 Pending，`kubectl describe pod` 的 Events 里出现 FailedScheduling。

判断：先看 FailedScheduling 的原因：节点资源不足（CPU / 内存不够）、节点选择器 nodeSelector 或亲和性没有节点满足、污点 taint 没有对应容忍、PersistentVolumeClaim 没绑定。`kubectl describe node` 可以看节点剩余资源。

处理：资源不足时调小 requests 或扩容节点；选择器写错时修正 Deployment；存储没绑定时检查 StorageClass 和 PVC 状态。
