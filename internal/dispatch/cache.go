// Disk-backed response cache (architecture §5 step 2, task T04). Path
// <UserCacheDir>/wise-yolo/v1/<backend>/<sha256>.json entries, keyed over the
// normalised command and the backend/versions, store verdict fields plus
// debug notes, timestamps, and the key itself — never the raw command text.
package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wiseyolo/internal/backend"
	"wiseyolo/internal/core"
)

// ttl is the entry lifetime; entries older than this are purged during the
// startup scan of a new cache (architecture §5 step 2, task T04).
const ttl = 30 * 24 * time.Hour

// cacheDirEnv is a test-only override of the cache root (so that cache tests
// never touch the developer's real cache directory). It is not a documented
// user setting; set WISE_YOLO_CACHE=false (or --no-cache) to turn the cache
// off instead.
const cacheDirEnv = "WISE_YOLO_CACHE_DIR"

// cacheEnv is the documented default-state setting (architecture §6):
// response cache on/off (check only), on by default; --cache/--no-cache
// override both.
const cacheEnv = "WISE_YOLO_CACHE"

// Cache is the disk-backed CacheHook. A Cache whose root could not be
// resolved behaves like NoCache: lookups miss, stores are dropped, and the
// check pipeline proceeds normally — a cache failure must never block
// screening. Concurrency is out of scope (one process per check).
type Cache struct {
	// root is <UserCacheDir>/wise-yolo/v1; empty disables the cache
	// entirely (no reads, no writes, eager creation of nothing).
	root string
}

// NewCache builds the cache, resolves its root, and runs the TTL purge scan
// once. Missing cache directories are not errors and are never created
// eagerly; they appear on the first Store.
func NewCache() *Cache {
	c := new(Cache)
	if dir, err := os.UserCacheDir(); err == nil {
		c.root = filepath.Join(dir, "wise-yolo", "v1")
	} else {
		// Unresolvable user cache dir: disable rather than fail the check.
		return c
	}
	if override := backend.Env(cacheDirEnv, ""); override != "" {
		// Test-only override, see cacheDirEnv.
		c.root = override
	}
	purge(c.root)
	return c
}

// Lookup reports the cached verdict for a normalised command, if a fresh,
// parseable entry exists under the derived key.
func (c *Cache) Lookup(cmd, backendName, requestedModel, policyVersion, thresholdsVersion string) (core.Verdict, bool) {
	if c.root == "" {
		return core.Verdict{}, false
	}
	data, err := os.ReadFile(c.path(cmd, backendName, requestedModel, policyVersion, thresholdsVersion))
	if err != nil {
		// Unreadable or absent: an ordinary miss.
		return core.Verdict{}, false
	}
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		// Corrupt or partial file: ignore; the next Store overwrites it.
		return core.Verdict{}, false
	}
	if time.Since(e.Created) > ttl {
		return core.Verdict{}, false
	}
	v := core.Verdict{
		Effect:     core.Effect(e.Effect),
		Confidence: e.Confidence,
		Categories: e.Categories,
		Reason:     e.Reason,
	}
	if !judged(v) {
		// Never serve an unknown effect from the cache.
		return core.Verdict{}, false
	}
	return v, true
}

// Store records the verdict under the derived key using an atomic
// write (temp file + rename); failures are silently dropped because caching
// is an optimisation, never a correctness requirement.
func (c *Cache) Store(cmd, backendName, requestedModel, policyVersion, thresholdsVersion string, v core.Verdict) {
	if c.root == "" || !judged(v) {
		return
	}
	e := entry{
		Effect:     string(v.Effect),
		Confidence: v.Confidence,
		Categories: ensureSlice(v.Categories),
		Reason:     v.Reason,
		Model:      requestedModel,
		Created:    time.Now().UTC(),
	}
	e.Key = cacheKey(cmd, backendName, requestedModel, policyVersion, thresholdsVersion)
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	dir := filepath.Join(c.root, backendName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(name, filepath.Join(c.root, backendName,
		cacheKey(cmd, backendName, requestedModel, policyVersion, thresholdsVersion)+".json")); err != nil {
		return
	}
}

// path builds the entry path: root/<backend>/<sha256>.json.
func (c *Cache) path(cmd, backendName, requestedModel, policyVersion, thresholdsVersion string) string {
	h := cacheKey(cmd, backendName, requestedModel, policyVersion, thresholdsVersion)
	return filepath.Join(c.root, backendName, h+".json")
}

// cacheKey is the hex sha256 over a stable "v1|" header plus the normalised
// command and the backend identity and versions (architecture §5 step 2):
// sha256("v1|" + normalised + "|" + backend + "|" + requested_model + "|" +
// policy_version + "|" + thresholds_version).
func cacheKey(normalised, backendName, requestedModel, policyVersion, thresholdsVersion string) string {
	h := sha256.Sum256([]byte("v1|" + normalised + "|" + backendName + "|" +
		requestedModel + "|" + policyVersion + "|" + thresholdsVersion))
	return hex.EncodeToString(h[:])
}

// purge deletes entries older than the TTL. It walks only directories that
// already exist (a missing cache directory is not an error and nothing is
// paved merely to scan), holding no locks: one process per check invocation.
func purge(root string) {
	backends, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, be := range backends {
		if !be.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, be.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > ttl {
				_ = os.Remove(filepath.Join(root, be.Name(), f.Name()))
			}
		}
	}
}

// DefaultCacheEnabled reports whether plain `check` runs with the cache on by
// default, honouring WISE_YOLO_CACHE (architecture §6); the --cache/--no-cache
// flags override both this and the env default.
func DefaultCacheEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(backend.Env(cacheEnv, ""))) {
	case "", "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// entry is the on-disk JSON. It never includes the raw command text; the key
// column is a hex sha256, safe to persist, stored purely for diagnoses.
// Notes reserves the debug slot for backend raw-answer annotations: the T03
// CacheHook transports verdicts only, so no raw answer is available here and
// the field stays empty until a hook revision carries notes through.
type entry struct {
	Effect     string    `json:"effect"`
	Confidence float64   `json:"confidence"`
	Categories []string  `json:"categories"`
	Reason     string    `json:"reason"`
	Notes      string    `json:"notes,omitempty"`
	Model      string    `json:"model"`
	Created    time.Time `json:"created"`
	Key        string    `json:"key"`
}
