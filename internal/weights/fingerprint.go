// fingerprint.go: the §2.3 content fingerprint for hf: weights ids —
// sha256 over the sorted "<path>\t<sha256-of-file>\n" lines of every
// content file in the local snapshot(s). Weights, tokenizer, chat
// template and configs all count (a different chat template is a
// different model to a consumer); pure metadata (README, license, images)
// does not. The HF hub cache stores blobs named by their own sha256, so
// LFS-named blobs are read for free — only small git-blob-named files are
// actually read. A fingerprint is all-or-nothing: one missing overlay
// file or dangling symlink voids the whole thing, because a partial
// fingerprint would claim stock weights.
package weights

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"remote.network/node/weightsid"
)

// hashBuf bounds one file read; LFS blobs are never read, so this covers
// the configs/tokenizers/templates only.
const hashBuf = 1 << 20

// Warn reasons (the node logs them once per card, with the path).
const (
	ReasonMissing    = "missing"    // declared base snapshot or overlay file not on this machine
	ReasonDangling   = "dangling"   // symlink target gone (the unmounted-volume case)
	ReasonUnreadable = "unreadable" // open/read failed
	ReasonIrregular  = "not a regular file"
)

// Fingerprint computes the fingerprint for an hf: weights id over the
// snapshots found under cacheRoot. Empty when the id is not hf:-shaped,
// when the base snapshot is not on this machine (expected for
// containerized backends — no warning), or when any declared overlay file
// is missing/unreadable (warned, with the path). warn may be nil.
func Fingerprint(cacheRoot, id string, warn func(path, reason string)) string {
	spec, ok := ParseID(id)
	if !ok || cacheRoot == "" {
		return ""
	}
	fail := func(path, reason string) {
		if warn != nil {
			warn(path, reason)
		}
	}

	lines, ok := snapshotLines(cacheRoot, spec.Repo, spec.Rev, "", fail)
	if !ok {
		return ""
	}
	for _, ov := range spec.Overlays {
		// Overlay lines are namespaced "overlay:<org>/<name>/<path>" so they
		// cannot collide with a base path.
		ls, ok := snapshotLines(cacheRoot, ov.Repo, ov.Rev, ov.Path, fail)
		if !ok {
			return ""
		}
		lines = append(lines, ls...)
	}

	return weightsid.FingerprintLines(lines)
}

// snapshotLines walks one snapshot: for the base it hashes every content
// file (path relative to the snapshot root, "/"-separated); for an overlay
// (only != "") it hashes that single file, line-named
// "overlay:<org>/<name>/<path>" so it cannot collide with a base path.
// ok is false when the snapshot is not on this machine or a needed file
// failed — the caller publishes no fingerprint at all, never a partial one.
func snapshotLines(cacheRoot, repo, rev, only string, fail func(path, reason string)) ([]string, bool) {
	root := snapshotDir(cacheRoot, repo, rev)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		// A base snapshot that is simply not here is the expected
		// containerized-backend case: quiet empty. An overlay snapshot that
		// is not here means the declared weights cannot be verified: warn.
		if only != "" {
			fail(weightsid.OverlayPath(repo, only), ReasonMissing)
		}
		return nil, false
	}

	if only != "" {
		// One file out of the overlay snapshot.
		rel := filepath.FromSlash(only)
		path := filepath.Join(root, rel)
		hex, err := fileHash(path)
		if err != nil {
			fail(weightsid.OverlayPath(repo, only), reasonFor(err))
			return nil, false
		}
		return []string{weightsid.Line(weightsid.OverlayPath(repo, only), hex)}, true
	}

	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fail(relOr(path, root), reasonFor(err))
			return errFingerprint
		}
		if d.IsDir() {
			return nil
		}
		if weightsid.ExcludedName(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			fail(path, ReasonUnreadable)
			return errFingerprint
		}
		hex, err := fileHash(path)
		if err != nil {
			fail(filepath.ToSlash(rel), reasonFor(err))
			return errFingerprint
		}
		lines = append(lines, weightsid.Line(filepath.ToSlash(rel), hex))
		return nil
	})
	if err != nil {
		return nil, false
	}
	return lines, true
}

// errFingerprint aborts the walk on the first problem — all-or-nothing.
var errFingerprint = errors.New("fingerprint incomplete")

func reasonFor(err error) string {
	switch {
	case errors.Is(err, errFingerprint):
		return ReasonUnreadable
	case errors.Is(err, fs.ErrNotExist):
		return ReasonDangling
	default:
		return ReasonUnreadable
	}
}

func relOr(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// fileHash hashes one snapshot file. Symlinks resolve to their blob: a
// blob whose NAME is 64 hex IS its sha256 (the HF LFS convention) — the
// multi-GB file is never read. Small files carry 40-hex git-blob names,
// so they are read and hashed (1 MiB buffer).
func fileHash(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	blob := path
	if fi.Mode()&fs.ModeSymlink != 0 {
		blob, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", err // dangling symlink
		}
	} else if !fi.Mode().IsRegular() {
		return "", errors.New(ReasonIrregular)
	}
	if base := filepath.Base(blob); weightsid.IsHex64(base) {
		return base, nil // LFS: the blob name is the digest
	}
	f, err := os.Open(blob)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, hashBuf)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
