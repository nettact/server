package webui

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaceholderPagesUseCanonicalBrandMark(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{name: "download", handler: placeholderHandler(false)},
		{name: "download with dev hint", handler: placeholderHandler(true)},
		{name: "missing bundle", handler: missingBundleHandler()},
	}

	canonicalOrbit := []string{
		`M22 126C37 87 86 56 158 49`,
		`M158 49C171.3 55.3 175.29 67.48 172.028 81.767`,
		`M172.028 81.767C170.63 87.89 167.9 94.4 164 101C140 141 82 160 39 153C24 151 17 141 22 126`,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := renderPlaceholderPage(t, tt.handler)

			if strings.Contains(page, "<!--BRANDMARK-->") || strings.Contains(page, "<!--FAVICON-->") {
				t.Fatal("brand asset marker was not replaced")
			}
			if strings.Count(page, `viewBox="0 0 200 200"`) != 2 {
				t.Fatal("page should contain equal-axis light and dark canonical marks")
			}
			if strings.Contains(page, `preserveAspectRatio="none"`) {
				t.Fatal("brand mark must not be stretched")
			}
			for _, path := range canonicalOrbit {
				if strings.Count(page, path) != 2 {
					t.Fatalf("canonical orbit segment %q should occur once in each theme mark", path)
				}
			}
			if strings.Count(page, `<circle `) != 4 {
				t.Fatal("each theme mark should retain two circular route nodes")
			}
			if !strings.Contains(page, `@media (prefers-color-scheme:dark)`) ||
				!strings.Contains(page, `.brand-mark .brand-mark-dark{display:block}`) {
				t.Fatal("page should select the reverse mark in dark mode")
			}
			if !strings.Contains(page, `rel="icon" type="image/svg+xml"`) {
				t.Fatal("page should include the self-contained SVG favicon")
			}
		})
	}
}

func TestBrandFaviconIsCanonicalApplicationIcon(t *testing.T) {
	const prefix = `data:image/svg+xml;base64,`
	start := strings.Index(brandFaviconHTML, prefix)
	if start < 0 {
		t.Fatal("favicon is not an SVG data URL")
	}
	start += len(prefix)
	end := strings.IndexByte(brandFaviconHTML[start:], '"')
	if end < 0 {
		t.Fatal("favicon data URL has no closing quote")
	}

	decoded, err := base64.StdEncoding.DecodeString(brandFaviconHTML[start : start+end])
	if err != nil {
		t.Fatalf("decode favicon: %v", err)
	}
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(decoded, &root); err != nil {
		t.Fatalf("favicon is not valid SVG XML: %v", err)
	}
	if root.XMLName.Local != "svg" {
		t.Fatalf("favicon root = %q; want svg", root.XMLName.Local)
	}

	svg := string(decoded)
	for _, want := range []string{
		`viewBox="0 0 1024 1024"`,
		`rx="224" fill="#10192A"`,
		`transform="translate(86 78) scale(4.4)"`,
		`M22 126C37 87 86 56 158 49`,
		`<circle cx="172.028" cy="81.767" r="11"`,
	} {
		if !strings.Contains(svg, want) {
			t.Fatalf("favicon is missing canonical application-icon geometry %q", want)
		}
	}
}

func renderPlaceholderPage(t *testing.T, handler http.Handler) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	return recorder.Body.String()
}
