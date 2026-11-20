# 镜像拉取失败 ImagePullBackOff

症状：Pod 卡在 ImagePullBackOff 或 ErrImagePull，容器一直没起来。

判断：先查 Events 里的 `Failed to pull image`，再核对 Deployment 的 image 字段和 tag 是否写错，镜像仓库是否可达，imagePullSecrets 是否缺失。
