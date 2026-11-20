# KubeCause 前端（Slice 5）

React + TypeScript + Vite，不引入 UI 框架，手写 CSS，保持构建快、依赖少。

## 前后端同仓库

前端源码在 `frontend/`，构建产物输出到 `../web/dist/`，Go 用 `go:embed` 把
`web/dist` 打进二进制。部署时还是一个 Go 进程，不用另外起 Node 服务，
简历上也只有一个仓库链接。

## 本地开发

```bash
# 装依赖（需要 Node 18+，本机实测 Node 24）
cd frontend
npm install

# 开发模式：Vite 起在 :5173，/api 代理到本地 Go 服务（先起 Go 服务在 :8080）
npm run dev

# 构建：先 tsc 类型检查，再 vite build，产物到 ../web/dist
npm run build
```

## 直接跑（不用装 Node）

交付 zip 里已经包含构建好的 `web/dist/`，解压后直接：

```bash
KUBE_MODE=fake MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

打开 http://localhost:8080 就能看到 React 页。

注意：如果本地把 `web/dist/` 删了，`go build` 会因为 go:embed 找不到文件而报错，
这时先跑 `cd frontend && npm run build` 再构建 Go。
