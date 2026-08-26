package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// NewHandler combines an API handler with an optional directory of built web
// assets. API paths always stay with the API; other GET and HEAD requests use
// static files, prerendered .html pages, and finally the SPA index fallback.
func NewHandler(api http.Handler, webRoot string) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("API handler is required")
	}
	webRoot = strings.TrimSpace(webRoot)
	if webRoot == "" {
		return api, nil
	}

	absoluteRoot, err := filepath.Abs(webRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve WEB_ROOT %q: %w", webRoot, err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("open WEB_ROOT %q: %w", absoluteRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("WEB_ROOT %q is not a directory", absoluteRoot)
	}

	root := os.DirFS(absoluteRoot)
	index, err := fs.Stat(root, "index.html")
	if err != nil {
		return nil, fmt.Errorf("WEB_ROOT %q must contain index.html: %w", absoluteRoot, err)
	}
	if !index.Mode().IsRegular() {
		return nil, fmt.Errorf("WEB_ROOT %q index.html is not a regular file", absoluteRoot)
	}

	return &handler{
		api:   api,
		root:  root,
		files: http.FileServer(http.FS(root)),
	}, nil
}

type handler struct {
	api   http.Handler
	root  fs.FS
	files http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		h.api.ServeHTTP(w, r)
		return
	}

	setWebSecurityHeaders(w.Header())
	target, found := h.resolveTarget(r.URL.Path)
	if !found {
		http.NotFound(w, r)
		return
	}
	setCacheHeaders(w.Header(), target)

	request := new(http.Request)
	*request = *r
	requestURL := *r.URL
	request.URL = &requestURL
	if target == "index.html" {
		request.URL.Path = "/"
	} else {
		request.URL.Path = "/" + target
	}
	request.URL.RawPath = ""
	h.files.ServeHTTP(w, request)
}

func (h *handler) resolveTarget(requestPath string) (string, bool) {
	if strings.Contains(requestPath, `\`) {
		return "", false
	}
	cleaned := path.Clean("/" + requestPath)
	relative := strings.TrimPrefix(cleaned, "/")
	if relative == "" || relative == "." {
		return "index.html", true
	}
	for _, segment := range strings.Split(relative, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", false
		}
	}

	if isRegularFile(h.root, relative) {
		return relative, true
	}
	if isRegularFile(h.root, path.Join(relative, "index.html")) {
		return path.Join(relative, "index.html"), true
	}
	if path.Ext(relative) == "" && isRegularFile(h.root, relative+".html") {
		return relative + ".html", true
	}
	if path.Ext(relative) == "" && !isAssetPath(relative) {
		return "index.html", true
	}
	return "", false
}

func isRegularFile(root fs.FS, name string) bool {
	info, err := fs.Stat(root, name)
	return err == nil && info.Mode().IsRegular()
}

func isAPIPath(requestPath string) bool {
	return requestPath == "/api" || strings.HasPrefix(requestPath, "/api/")
}

func isAssetPath(relativePath string) bool {
	return relativePath == "assets" || strings.HasPrefix(relativePath, "assets/")
}

func setWebSecurityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func setCacheHeaders(header http.Header, target string) {
	switch {
	case target == "index.html" || strings.HasSuffix(target, ".html"):
		header.Set("Cache-Control", "no-cache")
	case strings.HasPrefix(target, "assets/"):
		header.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		header.Set("Cache-Control", "public, max-age=300")
	}
}
