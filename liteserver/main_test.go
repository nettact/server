package liteserver

import (
	"os"
	"testing"

	"github.com/nettact/server-core/updatecheck"
)

// TestMain switches update checking off for the whole package. Every Start
// schedules an immediate check, so without this the test suite would make a real
// request to the public release catalog for each server it brings up. Tests that
// exercise the checker point the same variable at an httptest catalog with
// t.Setenv, which overrides this for their duration.
func TestMain(m *testing.M) {
	if os.Getenv(updatecheck.EnvBaseURL) == "" {
		_ = os.Setenv(updatecheck.EnvBaseURL, "off")
	}
	os.Exit(m.Run())
}
