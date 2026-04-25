package serve

import (
	"encoding/hex"
	"hash/fnv"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// yamlMIME is the MIME type per RFC 9512. Q-S3 confirmed.
const yamlMIME = "application/yaml"

// mimeOverrides maps extensions the stdlib mime package does not always
// map the way we want for kustomize consumers.
var mimeOverrides = map[string]string{
	".yaml": yamlMIME,
	".yml":  yamlMIME,
}

// DetectContentType returns the content type for a given filename.
// Falls back to application/octet-stream only when no extension match.
func DetectContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ct, ok := mimeOverrides[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ComputeETag returns a weak FNV-1a 64-bit ETag. Weak because a future
// transform (yamlToJson) may produce a different representation of the
// same underlying file, and weak ETags communicate that to caches.
func ComputeETag(body []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(body)
	sum := h.Sum(nil)
	return `W/"` + hex.EncodeToString(sum) + `"`
}

// WriteAsset renders body with y-cluster-serve's standard headers:
// ETag, Cache-Control forcing revalidation, and the content type
// detected from `filename`. Honors If-None-Match -> 304 and supports
// HEAD by discarding the body while preserving Content-Length.
func WriteAsset(w http.ResponseWriter, r *http.Request, filename string, body []byte) {
	WriteAssetAs(w, r, body, DetectContentType(filename))
}

// WriteAssetAs is the same as WriteAsset but takes an explicit
// content type, for callers that transform the body (e.g. yamlToJson)
// and cannot rely on filename-based detection.
func WriteAssetAs(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	etag := ComputeETag(body)
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache, must-revalidate")
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))

	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// matchesETag implements the RFC 7232 If-None-Match check for the single
// asset case. Accepts "*", and handles a comma-separated list. Weak tags
// compare only by opaque-tag equality here.
func matchesETag(header, have string) bool {
	if header == "" {
		return false
	}
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == have {
			return true
		}
	}
	return false
}

// MethodNotAllowed writes 405 with an Allow header. Used for routes that
// exist but don't support the requested method.
func MethodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
}
