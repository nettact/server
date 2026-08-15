package webui

import (
	"encoding/base64"
	"net/http"
	"strings"
)

const brandMarkHTML = `<div class="brand-mark" role="img" aria-label="NetTact">
<svg class="brand-mark-light" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200" aria-hidden="true" focusable="false">
  <g fill="none" stroke-linecap="round" stroke-linejoin="round">
    <path d="M22 126C37 87 86 56 158 49" stroke="#0284C7" stroke-width="7"/>
    <path d="M158 49C171.3 55.3 175.29 67.48 172.028 81.767" stroke="#0284C7" stroke-width="7"/>
    <path d="M48 140V52M48 52L132 140M132 140V52M105 52H159" stroke="#FFFFFF" stroke-width="26"/>
    <path d="M48 140V52" stroke="#10192A" stroke-width="22"/>
    <path d="M48 52L132 140" stroke="#0284C7" stroke-width="22"/>
    <path d="M132 140V52M105 52H159" stroke="#10192A" stroke-width="22"/>
    <path d="M172.028 81.767C170.63 87.89 167.9 94.4 164 101C140 141 82 160 39 153C24 151 17 141 22 126" stroke="#0284C7" stroke-width="7"/>
    <circle cx="22" cy="126" r="11" fill="#FFFFFF" stroke="#10192A" stroke-width="5"/>
    <circle cx="172.028" cy="81.767" r="11" fill="#0284C7" stroke="#10192A" stroke-width="5"/>
  </g>
</svg>
<svg class="brand-mark-dark" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200" aria-hidden="true" focusable="false">
  <g fill="none" stroke-linecap="round" stroke-linejoin="round">
    <path d="M22 126C37 87 86 56 158 49" stroke="#38BDF8" stroke-width="7"/>
    <path d="M158 49C171.3 55.3 175.29 67.48 172.028 81.767" stroke="#38BDF8" stroke-width="7"/>
    <path d="M48 140V52M48 52L132 140M132 140V52M105 52H159" stroke="#10192A" stroke-width="26"/>
    <path d="M48 140V52" stroke="#F8FAFC" stroke-width="22"/>
    <path d="M48 52L132 140" stroke="#38BDF8" stroke-width="22"/>
    <path d="M132 140V52M105 52H159" stroke="#F8FAFC" stroke-width="22"/>
    <path d="M172.028 81.767C170.63 87.89 167.9 94.4 164 101C140 141 82 160 39 153C24 151 17 141 22 126" stroke="#38BDF8" stroke-width="7"/>
    <circle cx="22" cy="126" r="11" fill="#10192A" stroke="#F8FAFC" stroke-width="5"/>
    <circle cx="172.028" cy="81.767" r="11" fill="#38BDF8" stroke="#F8FAFC" stroke-width="5"/>
  </g>
</svg>
</div>`

const brandFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
  <rect x="32" y="32" width="960" height="960" rx="224" fill="#10192A"/>
  <g transform="translate(86 78) scale(4.4)" fill="none" stroke-linecap="round" stroke-linejoin="round">
    <path d="M22 126C37 87 86 56 158 49" stroke="#38BDF8" stroke-width="7"/>
    <path d="M158 49C171.3 55.3 175.29 67.48 172.028 81.767" stroke="#38BDF8" stroke-width="7"/>
    <path d="M48 140V52M48 52L132 140M132 140V52M105 52H159" stroke="#10192A" stroke-width="26"/>
    <path d="M48 140V52" stroke="#F8FAFC" stroke-width="22"/>
    <path d="M48 52L132 140" stroke="#38BDF8" stroke-width="22"/>
    <path d="M132 140V52M105 52H159" stroke="#F8FAFC" stroke-width="22"/>
    <path d="M172.028 81.767C170.63 87.89 167.9 94.4 164 101C140 141 82 160 39 153C24 151 17 141 22 126" stroke="#38BDF8" stroke-width="7"/>
    <circle cx="22" cy="126" r="11" fill="#10192A" stroke="#F8FAFC" stroke-width="5"/>
    <circle cx="172.028" cy="81.767" r="11" fill="#38BDF8" stroke="#F8FAFC" stroke-width="5"/>
  </g>
</svg>`

var brandFaviconHTML = `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml;base64,` +
	base64.StdEncoding.EncodeToString([]byte(brandFaviconSVG)) + `">`

const placeholderHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="5">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NetTact</title>
<!--FAVICON-->
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f6f8;color:#1e293b}
main{max-width:34rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin-bottom:1rem}
p{line-height:1.7;color:#475569;margin:.5rem 0}
.brand-mark{width:5rem;height:5rem;margin:0 auto 1.25rem}
.brand-mark svg{display:block;width:100%;height:100%}
.brand-mark .brand-mark-dark{display:none}
.spin{width:2rem;height:2rem;margin:0 auto 1.5rem;border:3px solid #cbd5e1;border-top-color:#334155;border-radius:50%;animation:s 1s linear infinite}
@keyframes s{to{transform:rotate(360deg)}}
code{background:#e2e8f0;padding:.1em .4em;border-radius:4px}
@media (prefers-color-scheme:dark){
body{background:#10192a;color:#f8fafc}
p{color:#cbd5e1}
code{background:#1e293b;color:#f8fafc}
.spin{border-color:#334155;border-top-color:#38bdf8}
.brand-mark .brand-mark-light{display:none}
.brand-mark .brand-mark-dark{display:block}
}
</style>
</head>
<body>
<main>
<!--BRANDMARK-->
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

// missingBundleHTML is the embedded-build counterpart of placeholderHTML. A
// packaged build carries the console inside the executable and never downloads,
// so an absent dist is a build-assembly mistake, not a transient state: no
// spinner, no meta refresh (nothing will arrive), and the fix is named outright.
const missingBundleHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NetTact</title>
<!--FAVICON-->
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f6f8;color:#1e293b}
main{max-width:34rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin-bottom:1rem}
p{line-height:1.7;color:#475569;margin:.5rem 0}
.brand-mark{width:5rem;height:5rem;margin:0 auto 1.25rem}
.brand-mark svg{display:block;width:100%;height:100%}
.brand-mark .brand-mark-dark{display:none}
code{background:#e2e8f0;padding:.1em .4em;border-radius:4px}
@media (prefers-color-scheme:dark){
body{background:#10192a;color:#f8fafc}
p{color:#cbd5e1}
code{background:#1e293b;color:#f8fafc}
.brand-mark .brand-mark-light{display:none}
.brand-mark .brand-mark-dark{display:block}
}
</style>
</head>
<body>
<main>
<!--BRANDMARK-->
<h1>此版本未内置控制台界面</h1>
<p>这是一个未完整打包的开发构建。API 与监控服务运行正常。</p>
<p>This build was assembled without the web console. The API and monitoring
services are running normally.</p>
<p>Run <code>go run ./ci/fetchwebui</code> in the desktop repo before packaging,
or point <code>NETTACT_WEBUI_LOCAL</code> at a built web-console dist and restart.</p>
</main>
</body>
</html>
`

// placeholderHandler serves the built-in waiting page for non-/api paths until
// the real SPA is installed. 503 + no-store keeps proxies and uptime checks
// honest; the meta refresh lands the browser in the app once the swap happens.
func placeholderHandler(devHint bool) http.Handler {
	page := strings.Replace(placeholderHTML, "<!--DEVHINT-->", "", 1)
	if devHint {
		page = strings.Replace(placeholderHTML, "<!--DEVHINT-->", devHintHTML, 1)
	}
	return staticPage(brandedPage(page))
}

// missingBundleHandler serves the embedded-build "no console in this binary"
// page. Same 503 contract as placeholderHandler so uptime checks and proxies
// still see an unhealthy frontend.
func missingBundleHandler() http.Handler {
	return staticPage(brandedPage(missingBundleHTML))
}

func brandedPage(page string) string {
	page = strings.Replace(page, "<!--FAVICON-->", brandFaviconHTML, 1)
	return strings.Replace(page, "<!--BRANDMARK-->", brandMarkHTML, 1)
}

func staticPage(page string) http.Handler {
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
