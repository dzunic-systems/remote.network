// Package weights builds the node-side weights identity: what files the
// backend actually serves, as a WeightsID plus, when the snapshot is on
// this machine, a content fingerprint. Pure
// functions over paths and one GGUF digest cache — no backend knowledge,
// so serve.buildCard can call it from every precedence tier. A missing
// fact is an empty string, never a guess: an unresolvable revision or an
// unreadable file yields no identity rather than a wrong one.
package weights

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"remote.network/node/weightsid"
)

// The identity grammar lives in the exported weightsid package (the
// router verifies identities with the same rules); these aliases keep the
// node-side call sites unchanged.
type (
	Spec    = weightsid.Spec
	Overlay = weightsid.Overlay
)

// ParseSpec parses an operator --weights declaration (weightsid.ParseSpec).
func ParseSpec(s string) (Spec, error) { return weightsid.ParseSpec(s) }

// ParseID reads a rendered "hf:" id back into its spec (weightsid.ParseID).
func ParseID(id string) (Spec, bool) { return weightsid.ParseID(id) }

// HFCacheRoot resolves the Hugging Face cache root: the --hf-cache flag
// first, then the env vars huggingface_hub itself honours, then the
// default under the user's home. Never empty when a home exists.
func HFCacheRoot(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "huggingface", "hub")
	}
	return ""
}

// CachePath is the GGUF digest memo's location under the agent home
// (~/.remote/weights-cache.json); "" (no home — hand-built test runtimes)
// keeps the hasher memory-only.
func CachePath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, "weights-cache.json")
}

// snapshotDir is where one repo@rev lives in an HF hub cache.
func snapshotDir(cacheRoot, repo, rev string) string {
	return filepath.Join(cacheRoot, "models--"+strings.Replace(repo, "/", "--", 1), "snapshots", rev)
}

// snapshotPath captures "<anything>/models--<org>--<name>/snapshots/<rev40>"
// in a path — the container's /root/.cache/... prefix is irrelevant; only
// the shape is.
var snapshotPath = regexp.MustCompile(`models--([A-Za-z0-9._-]+)--([A-Za-z0-9._-]+)[/\\]snapshots[/\\]([0-9a-f]{40})`)

// HFIDFromPath derives "hf:<org>/<name>@<rev>" from a backend-reported
// path that names a cache snapshot (vLLM's root, llama.cpp's model_path).
// Anything else — a bare repo name, a local dir — is not-ok. Never guesses
// a revision: the path must carry the full 40-hex one.
func HFIDFromPath(path string) string {
	m := snapshotPath.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return "hf:" + m[1] + "/" + m[2] + "@" + m[3]
}

// HFIDFromRepo resolves a bare "<org>/<name>" to its current main-branch
// commit via the LOCAL hub cache's refs/main. Not-ok when the repo is not
// on this machine (a backend in a container, a local dir
// that merely looks repo-shaped): no id, rather than a wrong one.
func HFIDFromRepo(cacheRoot, repo string) string {
	org, name, ok := strings.Cut(repo, "/")
	if !ok || org == "" || name == "" || strings.ContainsAny(repo, "\\") ||
		!weightsid.SafePath(org) || !weightsid.SafePath(name) || strings.Contains(name, "/") {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(cacheRoot, "models--"+org+"--"+name, "refs", "main"))
	if err != nil {
		return ""
	}
	rev := strings.ToLower(strings.TrimSpace(string(b)))
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(rev) {
		return ""
	}
	return "hf:" + org + "/" + name + "@" + rev
}

// Ollama normalizes an /api/tags digest into (WeightsID, fingerprint):
// a bare hex string, or one with a "sha256:" prefix, lowercased. Not-ok
// unless it is exactly 64 hex — Ollama reports other hash families for
// non-GGUF models and they identify nothing we can pool on.
func Ollama(digest string) (id, fp string, ok bool) {
	d := strings.ToLower(strings.TrimSpace(digest))
	d = strings.TrimPrefix(d, "sha256:")
	if !weightsid.IsHex64(d) {
		return "", "", false
	}
	return "ollama:sha256:" + d, d, true
}
