package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestWebServesBundledUIAndAssetsWithoutCheckout(t *testing.T) {
	gateway, err := newWebGateway(context.Background(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	gateway.port = "8787"
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:8787"+path, nil))
		return response
	}
	page := get("/")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<div id="root">`) {
		t.Fatalf("bundled UI: status=%d body=%s", page.Code, page.Body.String())
	}
	assets := regexp.MustCompile(`(?:src|href)="(/assets/[^\"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(assets) < 2 {
		t.Fatalf("missing JS/CSS references: %s", page.Body.String())
	}
	for _, asset := range assets {
		response := get(asset[1])
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("asset %q: status=%d", asset[1], response.Code)
		}
	}
	for _, path := range []string{"/assets/missing.js", "/assets/../web.go", "/api/v1/missing"} {
		if response := get(path); response.Code != http.StatusNotFound {
			t.Errorf("unknown path %q: status=%d", path, response.Code)
		}
	}
}
