package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// distFS 嵌入前端构建产物（ack_notes/dist）。仓库只提交占位
// index.html，真实产物由 pnpm build 生成后再嵌入。
//
//go:embed all:ack_notes/dist
var distFS embed.FS

// webHandler 返回前端静态文件处理器：命中文件走文件系统，
// 其余路径回退 index.html（SPA 路由）。
func webHandler() http.Handler {
	sub, err := fs.Sub(distFS, "ack_notes/dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path := strings.TrimPrefix(r.URL.Path, "/"); path != "" {
			if _, err := fs.Stat(sub, path); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// SPA 回退：未知路径返回 index.html，由前端路由接管。
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
