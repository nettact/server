// Package webui serves the NetTact web console from one of two sources.
//
// Server builds do not embed the SPA: the exact web-console release tag is
// stamped into Version at compile time and the Manager downloads that release
// from GitHub at runtime into a local directory, verifying checksums and
// swapping the handler in atomically. Until the SPA is installed (or when a
// download keeps failing) a minimal built-in placeholder page is served — the
// API and agents are never blocked on the frontend.
//
// Desktop builds take the other path (NewPackaged): the dist is supplied from
// files installed beside the executable or in the app bundle, and nothing is
// ever fetched at runtime.
package webui

// Version is the exact web-console release tag this build serves — downloaded
// at runtime by server builds, packaged externally by desktop builds. Releases
// stamp it via
//
//	-ldflags "-X github.com/nettact/server/internal/webui.Version=v0.1.0"
//
// with the tag each release pipeline resolves from web-console's latest stable
// GitHub Release. "dev" (unstamped) disables downloading: the Manager serves
// NETTACT_WEBUI_LOCAL if set, else ../web-console/dist if it holds a built
// dist, else the placeholder page.
var Version = "dev"
