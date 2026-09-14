package cli

import (
	"bytes"
	"embed"
	"net/http"
	"path"
	"time"
)

// Build with npm --prefix web run build. Committed output keeps go install self-contained.
// https://pkg.go.dev/embed
//
//go:embed webassets
var webAssets embed.FS

func serveWebAsset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	if name == "/" {
		name = "/index.html"
	}
	content, err := webAssets.ReadFile("webassets" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(content))
}
