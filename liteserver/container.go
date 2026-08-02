package liteserver

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Container / network-mode detection, reported through server-info so the
// console can decide whether the listen-address control means anything here.
//
// Inside a container with its own network namespace the bind address is the
// runtime's choice, not the operator's: the console is reached through a
// published port, so binding 127.0.0.1 inside the container makes it
// unreachable from everywhere (docker-proxy included) with no UI left to undo
// it, and changing the port desyncs the container from its port mapping and its
// healthcheck. Host networking is the exception — the host's namespace is
// shared, so 127.0.0.1 vs 0.0.0.0 means exactly what it does on bare metal.

// Network modes reported to the console. "isolated" covers every non-host
// attachment (bridge, none, another container's namespace); the console only
// distinguishes "host" from "not host", and "unknown" from "we could tell".
const (
	netModeHost     = "host"
	netModeIsolated = "isolated"
	netModeUnknown  = "unknown"
)

// containerInfo is the resolved deployment shape. It is computed once at
// startup: neither a process's container-ness nor its network namespace changes
// while it runs.
type containerInfo struct {
	inContainer bool
	networkMode string // "" when not in a container
}

func detectContainer() containerInfo {
	if !inContainer() {
		return containerInfo{}
	}
	return containerInfo{inContainer: true, networkMode: detectNetworkMode()}
}

// inContainer recognises the runtimes NetTact is actually deployed under: the
// docker and podman marker files, a container-managed cgroup path (cgroup v1,
// and v2 where the manager still names itself), and Kubernetes' injected env.
func inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, needle := range []string{"docker", "containerd", "kubepods", "libpod", "lxc"} {
			if strings.Contains(s, needle) {
				return true
			}
		}
	}
	return false
}

// detectNetworkMode decides whether this container shares the host's network
// namespace, from the interfaces it can see.
//
// Host networking is proven by hardware. Only a NIC with a bus parent has a
// `device` symlink under /sys/class/net/<name>, and a namespaced container never
// gets one: everything plumbed into it is virtual. Seeing one means the host's
// own namespace.
//
// Isolation is proven by a plumbed-in link. A veth end or a macvlan child
// reports an iflink — its peer or parent, which lives in another namespace —
// different from its own ifindex. That is the shipped docker deployment.
//
// Everything else proves nothing, and the difference matters: "virtual" is not
// "isolated". WireGuard and dummy interfaces, bridges, and the per-namespace
// sit0/tunl0 fallback tunnels the kernel materialises in every new namespace
// once those modules are loaded are all self-linked (iflink == ifindex) while
// saying nothing about which namespace they are in. Treating self-linked as
// hardware would call a bridged container host-networked and hand its operator
// a control that binds loopback inside a container nothing can then reach — so
// they get no vote, and a namespace showing only such devices is reported
// unknown for the console to warn about.
//
// NETTACT_NETWORK_MODE overrides the probe for runtimes it reads wrongly (an
// exotic namespace shape, a sysfs that isn't mounted). It is deliberately not
// set by the shipped compose file: a value baked in there would go stale the
// moment someone edits the network mode next to it.
func detectNetworkMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NETTACT_NETWORK_MODE"))) {
	case "host":
		return netModeHost
	case "bridge", "nat", "none", "isolated", "container":
		return netModeIsolated
	}
	return networkModeFrom(sysClassNet)
}

// sysClassNet is the interface directory the probe reads; a variable only so the
// test can point it at a fabricated namespace.
var sysClassNet = "/sys/class/net"

func networkModeFrom(sysNet string) string {
	ents, err := os.ReadDir(sysNet)
	if err != nil {
		// No sysfs to read: say so rather than guess — the console warns instead
		// of silently hiding a control the operator may actually need.
		return netModeUnknown
	}
	sawIface, sawPlumbedIn := false, false
	for _, e := range ents {
		name := e.Name()
		if name == "lo" {
			continue
		}
		sawIface = true
		// Hardware settles it, wherever in the listing it turns up.
		if _, err := os.Stat(filepath.Join(sysNet, name, "device")); err == nil {
			return netModeHost
		}
		ifindex, err1 := readSysInt(filepath.Join(sysNet, name, "ifindex"))
		iflink, err2 := readSysInt(filepath.Join(sysNet, name, "iflink"))
		if err1 == nil && err2 == nil && ifindex != iflink {
			sawPlumbedIn = true
		}
	}
	switch {
	case sawPlumbedIn:
		// A veth end or macvlan child: another namespace holds the other side.
		return netModeIsolated
	case !sawIface:
		// Loopback alone (--network=none): reachable from nowhere, LAN included.
		return netModeIsolated
	default:
		// Only self-linked virtual devices — no hardware to call it host, no
		// plumbed-in link to call it isolated.
		return netModeUnknown
	}
}

func readSysInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
