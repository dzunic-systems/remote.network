// Package weightsid is the weights-identity grammar shared by the node and
// the router: how a card names the files it serves (WeightsID), how the
// content fingerprint is formed from per-file hashes, which files never
// count, and how an Ollama tag maps to its public registry manifest.
//
// The node computes identities from what is on its disk
// (node/internal/weights); the router recomputes the same identities from
// the public sources (Hugging Face, the Ollama registry) to verify them.
// Both must agree byte for byte, so the rules live here, once.
package weightsid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// repoRev matches one "<org>/<name>@<rev40>" reference.
var repoRev = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)@([0-9a-fA-F]{40})$`)

// pathCharset is what a repo path may contain (the router's intake regex
// agrees: [A-Za-z0-9._/-]+).
var pathCharset = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Spec is a parsed weights declaration: one base repo at a full commit,
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
		if ov.Path == "" || !SafePath(ov.Path) {
			return spec, fmt.Errorf("overlay path %q: want a repo file path (letters, digits, . _ / -)", path)
		}
		spec.Overlays = append(spec.Overlays, ov)
	}
	return spec, nil
}

// SafePath reports whether p is a plain repo-relative path: only
// letters, digits and . _ / -, and no ".." segment.
func SafePath(p string) bool {
	if !pathCharset.MatchString(p) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
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
// malformed ids) are not-ok.
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

// ExcludedName reports whether a file is pure metadata, excluded from the
// fingerprint (case-insensitive). Everything else counts — weights,
// tokenizer, chat template, configs: a different chat template is a
// different model to a consumer.
func ExcludedName(name string) bool {
	l := strings.ToLower(name)
	switch {
	case strings.HasPrefix(l, "readme"), strings.HasPrefix(l, "license"), strings.HasPrefix(l, "notice"):
		return true
	case l == ".gitattributes":
		return true
	}
	for _, ext := range []string{".md", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".pdf"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// Line is one fingerprint input line (without its trailing newline):
// "<path>\t<sha256 hex>". Base files use their snapshot-relative path;
// overlay files are written "overlay:<org>/<name>/<path>" so they cannot
// collide with a base path.
func Line(path, hexSum string) string { return path + "\t" + hexSum }

// OverlayPath is the fingerprint path of one overlay file.
func OverlayPath(repo, path string) string { return "overlay:" + repo + "/" + path }

// FingerprintLines is the content fingerprint: sha256 over the lines
// sorted bytewise, each followed by "\n". It sorts a copy.
func FingerprintLines(lines []string) string {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, l := range sorted {
		h.Write([]byte(l))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsHex64 reports whether s is exactly 64 lowercase hex characters.
func IsHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// OllamaRef maps a raw Ollama tag to its registry coordinates:
// "gemma4:e2b-it-qat" → (library, gemma4, e2b-it-qat);
// "qllama/multilingual-e5-large-instruct:latest" → (qllama, …, latest).
// A leading "registry.ollama.ai/" is stripped; any other host-looking
// prefix (a "." before the first "/", e.g. "hf.co/…") is not the public
// registry → not ok. A missing tag is "latest".
func OllamaRef(raw string) (ns, name, tag string, ok bool) {
	r := strings.ToLower(strings.TrimSpace(raw))
	r = strings.TrimPrefix(r, "registry.ollama.ai/")
	if r == "" {
		return "", "", "", false
	}
	if first, _, hasSlash := strings.Cut(r, "/"); hasSlash && strings.Contains(first, ".") {
		return "", "", "", false
	}
	path, t, hasTag := strings.Cut(r, ":")
	if !hasTag || t == "" {
		t = "latest"
	}
	ns, name = "library", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		ns, name = path[:i], path[i+1:]
	}
	if ns == "" || name == "" || strings.Contains(ns, "/") ||
		!SafePath(ns) || !SafePath(name) || !SafePath(strings.ReplaceAll(t, ":", "")) {
		return "", "", "", false
	}
	return ns, name, t, true
}

// OllamaKey is the reference key for an Ollama tag ("ollama:<ns>/<name>:<tag>").
func OllamaKey(ns, name, tag string) string { return "ollama:" + ns + "/" + name + ":" + tag }

// OllamaPoolID is the public pool id for a verified Ollama tag, as the user
// types the tag: "ollama:gemma4:e2b-it-qat", "ollama:qllama/<name>:<tag>".
func OllamaPoolID(ns, name, tag string) string {
	if ns == "library" {
		return "ollama:" + name + ":" + tag
	}
	return "ollama:" + ns + "/" + name + ":" + tag
}
