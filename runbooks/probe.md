# 探针失败 / Liveness / Readiness

症状：Liveness 探针失败会导致容器被重启，Readiness 失败会让 Pod 脱离 Service。

判断：先看探针的路径、端口和超时，再看应用启动耗时是否超过 initialDelaySeconds，日志里是否有启动失败。
