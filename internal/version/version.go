// Package version carries this server build's own release tag.
//
// It is deliberately separate from internal/webui.Version, which names the
// web-console release the build serves — two different things that ship on two
// different cadences. This one answers "is this server out of date", which the
// console's update check reports.
//
// Release builds stamp it:
//
//	-ldflags "-X github.com/nettact/server/internal/version.Version=v1.2.3"
//
// An unstamped build stays "dev", which the update check treats as older than
// every release.
package version

var Version = "dev"
