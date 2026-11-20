# Service 无 Endpoints / 服务访问不到

症状：Service 存在，但 `kubectl get endpoints` 为空，访问 Service 没有后端响应；或者 Pod 是 Running，但流量进不来。

判断：先核对 Service 的 selector 和 Pod 的 labels 是否对得上，标签差一个字符就匹配不到。再看 Pod 的 Readiness 探针是否通过：Readiness 失败的 Pod 不会进 Endpoints。最后看 Service 的端口和 Pod 的容器端口是否一致。

处理：修正 selector 或 Pod 标签，修好 Readiness 探针，核对端口映射。
