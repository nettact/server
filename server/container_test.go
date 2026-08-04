package server

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// iface writes one fabricated /sys/class/net entry. A differing iflink marks an
// interface plumbed in from another namespace (veth, macvlan); hw adds the
// `device` link only a NIC with a bus parent has.
func iface(t *testing.T, root, name string, ifindex, iflink int, hw bool) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	write := func(file, val string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(val+"\n"), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", name, file, err)
		}
	}
	write("ifindex", strconv.Itoa(ifindex))
	write("iflink", strconv.Itoa(iflink))
	if hw {
		// Real sysfs has a symlink to the bus device; only its existence is read.
		if err := os.MkdirAll(filepath.Join(dir, "device"), 0o755); err != nil {
			t.Fatalf("mkdir %s/device: %v", name, err)
		}
	}
}

func TestNetworkModeFrom(t *testing.T) {
	t.Run("bridge container is isolated", func(t *testing.T) {
		// The shipped docker deployment: loopback plus one veth end. Loopback is
		// real in every namespace, so counting it would misreport every container
		// as host-networked.
		root := t.TempDir()
		iface(t, root, "lo", 1, 1, false)
		iface(t, root, "eth0", 12, 27, false)
		if got := networkModeFrom(root); got != netModeIsolated {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeIsolated)
		}
	})

	t.Run("self-linked virtual devices do not make a container host", func(t *testing.T) {
		// sit0/tunl0 appear in EVERY namespace once those modules are loaded, and
		// a WireGuard link is created inside plenty of containers. All are
		// self-linked; reading that as hardware would unlock the control in an
		// ordinary bridged container.
		root := t.TempDir()
		iface(t, root, "lo", 1, 1, false)
		iface(t, root, "eth0", 12, 27, false)
		iface(t, root, "sit0", 2, 2, false)
		iface(t, root, "tunl0", 3, 3, false)
		iface(t, root, "wg0", 4, 4, false)
		if got := networkModeFrom(root); got != netModeIsolated {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeIsolated)
		}
	})

	t.Run("host namespace is host", func(t *testing.T) {
		// Sharing the host's namespace exposes its real NICs — and the veths and
		// bridges of whatever containers it runs, which must not veto the verdict
		// no matter which order the directory lists them in.
		root := t.TempDir()
		iface(t, root, "lo", 1, 1, false)
		iface(t, root, "docker0", 3, 3, false)
		iface(t, root, "vethb1c2d3", 7, 6, false)
		iface(t, root, "eth0", 2, 2, true)
		if got := networkModeFrom(root); got != netModeHost {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeHost)
		}
	})

	t.Run("loopback only is isolated", func(t *testing.T) {
		root := t.TempDir()
		iface(t, root, "lo", 1, 1, false)
		if got := networkModeFrom(root); got != netModeIsolated {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeIsolated)
		}
	})

	t.Run("only self-linked virtual devices is unknown", func(t *testing.T) {
		// Rootless podman's slirp tap0: no hardware to prove host, no plumbed-in
		// link to prove isolation. Warn rather than guess either way.
		root := t.TempDir()
		iface(t, root, "lo", 1, 1, false)
		iface(t, root, "tap0", 2, 2, false)
		if got := networkModeFrom(root); got != netModeUnknown {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeUnknown)
		}
	})

	t.Run("unreadable sysfs is unknown", func(t *testing.T) {
		if got := networkModeFrom(filepath.Join(t.TempDir(), "absent")); got != netModeUnknown {
			t.Fatalf("networkModeFrom = %q, want %q", got, netModeUnknown)
		}
	})
}

func TestDetectNetworkModeEnvOverride(t *testing.T) {
	root := t.TempDir()
	iface(t, root, "lo", 1, 1, false)
	iface(t, root, "eth0", 12, 27, false) // probes as isolated
	old := sysClassNet
	sysClassNet = root
	t.Cleanup(func() { sysClassNet = old })

	t.Setenv("NETTACT_NETWORK_MODE", " Host ")
	if got := detectNetworkMode(); got != netModeHost {
		t.Fatalf("with override=host: %q, want %q", got, netModeHost)
	}
	t.Setenv("NETTACT_NETWORK_MODE", "garbage")
	if got := detectNetworkMode(); got != netModeIsolated {
		t.Fatalf("with unrecognised override, probe should decide: %q, want %q", got, netModeIsolated)
	}
}
