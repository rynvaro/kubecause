# ConfigMap / Secret 缺失

症状：Pod 启动失败或应用报错找不到配置，Events 里出现 FailedMount 或 CreateContainerConfigError，日志里报 config file not found 或 secret not found。

判断：先看 Deployment 挂载的 ConfigMap / Secret 名字在 namespace 里存不存在，再看 key 名有没有写错。Secret 还要确认类型对不对，tls 类型的 Secret 挂载方式和普通 Secret 不一样。

处理：补建缺失的 ConfigMap / Secret，或修正 Deployment 里的引用名和 key。
