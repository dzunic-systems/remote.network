// Package weights builds the node-side weights identity: what files the
// backend actually serves, as a WeightsID plus, when the snapshot is on
// this machine, a content fingerprint. Pure
// functions over paths and one GGUF digest cache — no backend knowledge,
// so serve.buildCard can call it from every precedence tier. A missing
// fact is an empty string, never a guess: an unresolvable revision or an
// unreadable file yields no identity rather than a wrong one.
package weights

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repoRev matches one "<org>/<name>@<rev40>" reference.
var repoRev = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)@([0-9a-fA-F]{40})$`)

// pathCharset is what an overlay path may contain (the router's intake
// regex agrees: [A-Za-z0-9._/-]+).
var overlayPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Spec is a parsed operator declaration: one base repo at a full commit,
// plus optional file-level overlays (house ablations ride stock weights).
type Spec struct {
	Repo     string // "<org>/<name>"
	Rev      string // 40-hex commit, lowercased
	Overlays []Overlay
}

// Overlay is one file taken from another repo at a pinned commit.
type Overlay struct {
	Repo string
	Rev  string
	Path string // within that repo, "/"-separated, no leading slash
}

// ParseSpec parses "<org>/<name>@<rev40>[+<org>/<name>@<rev40>:<path>]...".
// Revisions must be full 40-hex commits — a short or branch-name revision
// is an error, never silently accepted or guessed (uppercased hex is
// lowercased). The caller's error path names the flag; the message here
// says which rule failed.
func ParseSpec(s string) (Spec, error) {
	var spec Spec
	parts := strings.Split(s, "+")
	ref := strings.TrimSpace(parts[0])
	m := repoRev.FindStringSubmatch(ref)
	if m == nil {
		return spec, fmt.Errorf("want <org>/<name>@<full-40-hex-commit>, got %q", ref)
	}
	spec.Repo, spec.Rev = m[1]+"/"+m[2], strings.ToLower(m[3])
	for _, p := range parts[1:] {
		refPart, path, _ := strings.Cut(strings.TrimSpace(p), ":")
		ov := Overlay{Path: strings.Trim(path, "/")}
		m := repoRev.FindStringSubmatch(refPart)
		if m == nil {
			return spec, fmt.Errorf("overlay want <org>/<name>@<full-40-hex-commit>:<path>, got %q", p)
		}
		ov.Repo, ov.Rev = m[1]+"/"+m[2], strings.ToLower(m[3])
		if ov.Path == "" || !overlayPath.MatchString(ov.Path) || hasDotDot(ov.Path) {
			return spec, fmt.Errorf("overlay path %q: want a repo file path (letters, digits, . _ / -)", path)
		}
		spec.Overlays = append(spec.Overlays, ov)
	}
	return spec, nil
}

func hasDotDot(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// ID renders the canonical WeightsID: "hf:<org>/<name>@<rev>" plus one
// "+<org>/<name>@<rev>:<path>" per overlay.
func (s Spec) ID() string {
	var b strings.Builder
	b.WriteString("hf:")
	b.WriteString(s.Repo)
	b.WriteString("@")
	b.WriteString(s.Rev)
	for _, ov := range s.Overlays {
		b.WriteString("+")
		b.WriteString(ov.Repo)
		b.WriteString("@")
		b.WriteString(ov.Rev)
		b.WriteString(":")
		b.WriteString(ov.Path)
	}
	return b.String()
}

// ParseID reads a rendered "hf:" id back into its spec; other schemes (and
// malformed ids) are not-ok. Fingerprint uses it to find what to hash.
func ParseID(id string) (Spec, bool) {
	if !strings.HasPrefix(id, "hf:") {
		return Spec{}, false
	}
	spec, err := ParseSpec(strings.TrimPrefix(id, "hf:"))
	if err != nil {
		return Spec{}, false
	}
	return spec, true
}

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
		!overlayPath.MatchString(org) || !overlayPath.MatchString(name) ||
		hasDotDot(repo) {
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
	if len(d) != 64 || !isHex64(d) {
		return "", "", false
	}
	return "ollama:sha256:" + d, d, true
}

func isHex64(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
