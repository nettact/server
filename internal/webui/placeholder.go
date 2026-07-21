package webui

import (
	"net/http"
	"strings"
)

const placeholderHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="5">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NetTact</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f6f8;color:#1e293b}
main{max-width:34rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin-bottom:1rem}
p{line-height:1.7;color:#475569;margin:.5rem 0}
.spin{width:2rem;height:2rem;margin:0 auto 1.5rem;border:3px solid #cbd5e1;border-top-color:#334155;border-radius:50%;animation:s 1s linear infinite}
@keyframes s{to{transform:rotate(360deg)}}
code{background:#e2e8f0;padding:.1em .4em;border-radius:4px}
</style>
</head>
<body>
<main>
<div class="spin"></div>
<h1>NetTact 控制台界面正在下载</h1>
<p>如下载失败会自动重试。API 与监控服务运行正常，本页将自动刷新。</p>
<p>The NetTact console UI is downloading (retrying automatically on failure).
The API and monitoring services are running normally. This page refreshes automatically.</p>
<!--DEVHINT-->
</main>
</body>
</html>
`

const devHintHTML = `<p>dev build: set <code>NETTACT_WEBUI_LOCAL</code> to a built web-console dist.</p>`

// placeholderHandler serves the built-in waiting page for non-/api paths until
// the real SPA is installed. 503 + no-store keeps proxies and uptime checks
// honest; the meta refresh lands the browser in the app once the swap happens.
func placeholderHandler(devHint bool) http.Handler {
	page := strings.Replace(placeholderHTML, "<!--DEVHINT-->", "", 1)
	if devHint {
		page = strings.Replace(placeholderHTML, "<!--DEVHINT-->", devHintHTML, 1)
	}
	body := []byte(page)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
	})
}
