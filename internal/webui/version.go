// Package webui serves the NetTact web console. The SPA is no longer embedded
// at build time: the exact web-console release tag is stamped into Version at
// compile time, and the Manager downloads that release from GitHub at runtime
// into a local directory, verifying checksums and swapping the handler in
// atomically. Until the SPA is installed (or when a download keeps failing) a
// minimal built-in placeholder page is served — the API and agents are never
// blocked on the frontend.
package webui

// Version is the exact web-console release tag this build downloads at runtime.
// Release builds stamp it via
//
//	-ldflags "-X github.com/nettact/server-lite/internal/webui.Version=v0.1.0"
//
// with the value from ci/deps.env WEB_CONSOLE_VERSION. "dev" (unstamped)
// disables downloading: the Manager serves NETTACT_WEBUI_LOCAL if set, else
// the placeholder page.
var Version = "dev"
