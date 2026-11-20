# CrashLoopBackOff / 容器反复崩溃重启

症状：Pod 状态为 CrashLoopBackOff，容器启动后很快退出，又被拉起，如此反复。`kubectl describe pod` 的 LastState 有终止记录，Events 里有 BackOff。

判断：先看上次终止的原因和退出码：OOMKilled 是内存问题，Error 退出码 1 常是应用启动失败或探针失败。再看日志最后几行，应用 panic、配置报错、端口被占都会让容器一启动就退出。

处理：不要只调大重启间隔，要找到退出原因。内存问题调高 limit（需人工审批），启动失败修配置或镜像，探针问题调探针参数。
