package handlers

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"sort"

	"github.com/gofiber/fiber/v2"
)

// AssetHash is an 8-char hex SHA256 digest over all files in the embedded
// static FS. Computed once at startup by ComputeAssetHash and used as a
// cache-buster in asset URLs (e.g. app.js?v=<AssetHash>).
var AssetHash string

// ServiceWorkerBytes holds sw.js with __CACHE_VERSION__ and __ASSET_HASH__
// placeholders replaced by AssetHash. Built once at startup by
// BuildServiceWorker and served by ServeServiceWorker.
var ServiceWorkerBytes []byte

// ComputeAssetHash computes a deterministic 8-char hex digest from the
// content of every file in fsys. Paths are sorted so the hash is stable
// across runs of the same build.
func ComputeAssetHash(fsys fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		h.Write(data)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:8], nil
}

// BuildServiceWorker reads sw.js from fsys and replaces the placeholders
// __CACHE_VERSION__, __ASSET_HASH__ and __BASE_PATH__ with their values.
func BuildServiceWorker(fsys fs.FS, hash string) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, "sw.js")
	if err != nil {
		return nil, err
	}
	out := bytes.ReplaceAll(raw, []byte("__CACHE_VERSION__"), []byte(hash))
	out = bytes.ReplaceAll(out, []byte("__ASSET_HASH__"), []byte(hash))
	out = bytes.ReplaceAll(out, []byte("__BASE_PATH__"), []byte(BasePath))
	return out, nil
}

// ManifestBytes holds manifest.json with root-absolute URLs rewritten to
// include BasePath. Built once at startup by BuildManifest.
var ManifestBytes []byte

// BuildManifest reads manifest.json from fsys and prefixes its root-absolute
// URLs (start_url, icon sources) with BasePath, and adds a matching scope so
// the installed PWA is scoped to the app's mount point.
func BuildManifest(fsys fs.FS) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, "manifest.json")
	if err != nil {
		return nil, err
	}
	if BasePath == "" {
		return raw, nil
	}
	out := bytes.ReplaceAll(raw, []byte(`"/static/`), []byte(`"`+BasePath+`/static/`))
	out = bytes.ReplaceAll(out, []byte(`"start_url": "/"`), []byte(`"start_url": "`+BasePath+`/",
  "scope": "`+BasePath+`/"`))
	return out, nil
}

// ServeServiceWorker's manifest sibling: serves the pre-built ManifestBytes.
func ServeManifest(c *fiber.Ctx) error {
	c.Set("Content-Type", "application/manifest+json")
	c.Set("Cache-Control", "no-cache")
	return c.Send(ManifestBytes)
}

// ServeServiceWorker serves the pre-built ServiceWorkerBytes. SW must not be
// hard-cached by the browser because it controls all other caches; use
// no-cache so browsers revalidate on every navigation. The Service-Worker-Allowed
// header widens the max scope to "/" even though the script is served from
// /static/, so the worker can control navigations (and serve the app shell
// offline) rather than only /static/ subresources.
func ServeServiceWorker(c *fiber.Ctx) error {
	c.Set("Content-Type", "application/javascript; charset=utf-8")
	c.Set("Cache-Control", "no-cache")
	c.Set("Service-Worker-Allowed", CookiePath())
	return c.Send(ServiceWorkerBytes)
}
