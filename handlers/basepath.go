package handlers

import (
	"os"
	"strings"
)

// BasePath is the URL prefix the app is mounted under (e.g. "/koffan") when
// served behind a reverse proxy in a subfolder. Empty means mounted at root.
// It is normalized to have a leading slash and no trailing slash.
var BasePath string

// NormalizeBasePath cleans a raw BASE_PATH value into the canonical form used
// throughout the app: empty (root) or "/segment" with no trailing slash.
func NormalizeBasePath(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// InitBasePath reads BASE_PATH from the environment and stores the normalized
// value in BasePath.
func InitBasePath() {
	BasePath = NormalizeBasePath(os.Getenv("BASE_PATH"))
}

// WithBase prefixes a root-absolute app path with BasePath.
func WithBase(p string) string {
	return BasePath + p
}

// CookiePath returns the Path used for session cookies so they are scoped to
// the app's mount point.
func CookiePath() string {
	if BasePath == "" {
		return "/"
	}
	return BasePath + "/"
}

// StripBase removes the BasePath prefix from a request path so handlers can
// compare against root-relative routes (e.g. "/login").
func StripBase(path string) string {
	if BasePath == "" {
		return path
	}
	if rel := strings.TrimPrefix(path, BasePath); rel != path {
		if rel == "" {
			return "/"
		}
		return rel
	}
	return path
}
