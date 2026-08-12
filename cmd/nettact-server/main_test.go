package main

import "testing"

func TestResolveListenAddrSource(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv(envServerAddr, "  127.0.0.1:19000  ")
		addr, fromFlag, fromEnv := resolveListenAddrSource(":12450", true)
		if addr != ":12450" || !fromFlag || fromEnv {
			t.Fatalf("resolved = %q, flag=%v, env=%v", addr, fromFlag, fromEnv)
		}
	})

	t.Run("environment wins without explicit flag", func(t *testing.T) {
		t.Setenv(envServerAddr, "  127.0.0.1:19000  ")
		addr, fromFlag, fromEnv := resolveListenAddrSource(":12450", false)
		if addr != "127.0.0.1:19000" || fromFlag || !fromEnv {
			t.Fatalf("resolved = %q, flag=%v, env=%v", addr, fromFlag, fromEnv)
		}
	})

	t.Run("empty environment is unset", func(t *testing.T) {
		t.Setenv(envServerAddr, "  ")
		addr, fromFlag, fromEnv := resolveListenAddrSource(":19001", false)
		if addr != ":19001" || fromFlag || fromEnv {
			t.Fatalf("resolved = %q, flag=%v, env=%v", addr, fromFlag, fromEnv)
		}
	})
}
