// Package web 提供前端构建产物的内嵌文件系统。
//
// 前后端同仓库：React 源码在 frontend/，构建产物输出到 web/dist/，
// 这里用 go:embed 把 dist 打进 Go 二进制，部署时还是一个进程，
// 不用另外起 Node 服务，也不用在服务器上装 Node。
//
// 注意：go:embed 要求构建时 web/dist 已经存在。交付 zip 里自带构建好的
// dist，解压后直接 go run 就能跑；如果本地删了 dist，先跑
// `cd frontend && npm run build` 再构建 Go。
package web

import "embed"

// Dist 是 Vite 构建产物（web/dist 目录）的内嵌副本。
//
//go:embed dist
var Dist embed.FS
