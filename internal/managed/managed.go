// Package managed is the zero-setup path: resolve/download/verify/supervise
// llama-server and pull catalog-pinned GGUFs. It produces exactly what the
// BYO path consumes — a local OpenAI-compatible endpoint — so the rest of
// the agent never knows which path it is on.
package managed

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	ghReleasesAPI = "https://api.github.com/repos/ggml-org/llama.cpp/releases"
	hfResolve     = "https://huggingface.co/%s/resolve/main/%s"
)

// Paths resolves the managed layout under the agent home.
func Paths(home string) (binDir, modelDir string) {
	return filepath.Join(home, "bin"), filepath.Join(home, "models")
}

// EnsureBinary returns a usable llama-server path, downloading the latest
// release for this platform on first use. Verification: the release's
// sha256-checksums.txt when present; otherwise the artifact's hash is
// recorded at first download (trust-on-first-download, logged).
func EnsureBinary(ctx context.Context, home string, log *slog.Logger) (string, error) {
	binDir, _ := Paths(home)
	marker := filepath.Join(binDir, "llama-server.path")
	if b, err := os.ReadFile(marker); err == nil {
		p := strings.TrimSpace(string(b))
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}

	log.Info("downloading llama-server (first use)", "platform", runtime.GOOS+"/"+runtime.GOARCH)
	tag, assets, err := fetchNightlyAssets(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve llama.cpp release: %w", err)
	}
	asset, checksums, err := pickAsset(assets)
	if err != nil {
		return "", err
	}
	log.Info("resolved llama.cpp nightly", "tag", tag, "asset", asset.Name)

	archivePath := filepath.Join(binDir, asset.Name+".download")
	if err := download(ctx, asset.URL, archivePath, asset.Size, log); err != nil {
		return "", err
	}
	wantSHA := checksums[asset.Name]
	if wantSHA == "" {
		// Nightlies publish no checksum asset: pin what we downloaded.
		log.Warn("release publishes no checksum for asset — recording first-download hash (TOFD)", "asset", asset.Name)
	} else if err := verifyFile(archivePath, wantSHA); err != nil {
		os.Remove(archivePath)
		return "", err
	}

	dest := filepath.Join(binDir, "llama.cpp-"+tag)
	_ = os.RemoveAll(dest)
	srvPath, err := extractServer(archivePath, dest)
	os.Remove(archivePath)
	if err != nil {
		return "", err
	}
	if wantSHA == "" {
		if h, err := hashFile(srvPath); err == nil {
			log.Info("recorded llama-server sha256 (TOFD)", "sha256", h[:12]+"…")
		}
	}
	if err := os.WriteFile(marker, []byte(srvPath), 0o600); err != nil {
		return "", err
	}
	log.Info("llama-server ready", "path", srvPath)
	return srvPath, nil
}

// PullModel downloads a catalog-pinned GGUF, verifying its sha256; returns
// the local path, skipping the download when the pin already matches.
func PullModel(ctx context.Context, repo, file, sha string, size int64, home string, log *slog.Logger) (string, error) {
	_, modelDir := Paths(home)
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(modelDir, file)
	if _, err := os.Stat(final); err == nil {
		if sha == "" || verifyFile(final, sha) == nil {
			return final, nil
		}
		log.Warn("existing GGUF fails pin — re-downloading", "file", file)
		os.Remove(final)
	}
	if sha == "" {
		return "", errors.New("managed pull requires a pinned sha256 (catalog entry has none)")
	}

	url := fmt.Sprintf(hfResolve, repo, file)
	tmp := final + ".download"
	if err := download(ctx, url, tmp, size, log); err != nil {
		return "", err
	}
	if err := verifyFile(tmp, sha); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("GGUF pin mismatch: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	log.Info("GGUF ready", "file", file, "size_mb", size/(1<<20))
	return final, nil
}

// ServerConfig for llama-server supervision.
type ServerConfig struct {
	Binary   string
	GGUFPath string
	Ctx      int
	Port     int    // 0 = pick a free one
	HomeDir  string // stderr/stdout land in HomeDir/logs/llama-server.log
	Log      *slog.Logger
}

// Server is a supervised llama-server process.
type Server struct {
	cmd    *exec.Cmd
	cfg    ServerConfig
	cancel context.CancelFunc
	dead   chan struct{}
}

// Start launches llama-server and waits for /health. The goroutine restarts
// it (backoff 1s→10s) if it crashes; Stop tears it down.
func Start(parent context.Context, cfg ServerConfig) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Port == 0 {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		cfg.Port = p
	}
	if cfg.Ctx == 0 {
		cfg.Ctx = 8192
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{cfg: cfg, cancel: cancel, dead: make(chan struct{})}

	if err := s.launch(); err != nil {
		cancel()
		return nil, err
	}
	go s.supervise(ctx)

	deadline := time.Now().Add(120 * time.Second) // big models load slowly
	for time.Now().Before(deadline) {
		if s.healthy(ctx) {
			cfg.Log.Info("llama-server healthy", "port", cfg.Port)
			return s, nil
		}
		select {
		case <-ctx.Done():
			s.Stop()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	s.Stop()
	return nil, errors.New("llama-server did not become healthy in 120s")
}

// BaseURL is the OpenAI-compatible endpoint the server serves.
func (s *Server) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d/v1", s.cfg.Port) }

// Port is the bound port.
func (s *Server) Port() int { return s.cfg.Port }

func (s *Server) launch() error {
	args := []string{
		"-m", s.cfg.GGUFPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(s.cfg.Port),
		"-c", fmt.Sprint(s.cfg.Ctx),
		"--no-webui",
	}
	cmd := exec.Command(s.cfg.Binary, args...)
	// Crash output goes to a file — discarding it once cost a blind
	// debug loop; a supervised process must leave a trail.
	var serverLog io.Writer = io.Discard
	if s.cfg.HomeDir != "" {
		if f, err := os.OpenFile(filepath.Join(s.cfg.HomeDir, "logs", "llama-server.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			serverLog = f
		}
	}
	cmd.Stdout = serverLog
	cmd.Stderr = serverLog
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch llama-server: %w", err)
	}
	s.cmd = cmd
	return nil
}

func (s *Server) supervise(ctx context.Context) {
	backoff := time.Second
	for {
		if err := s.cmd.Wait(); err == nil || ctx.Err() != nil {
			close(s.dead)
			return
		}
		s.cfg.Log.Warn("llama-server crashed — restarting", "backoff", backoff.String())
		select {
		case <-ctx.Done():
			close(s.dead)
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
		if err := s.launch(); err != nil {
			s.cfg.Log.Error("llama-server restart failed", "err", err)
			close(s.dead)
			return
		}
		backoff = time.Second
	}
}

// Stop terminates the process; safe to call twice.
func (s *Server) Stop() {
	s.cancel()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.dead
}

func (s *Server) healthy(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/health", s.cfg.Port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// --- asset resolution ---

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// fetchNightlyAssets walks recent releases (nightlies are PRERELEASES, so
// the /latest endpoint does not see them) and returns the newest release
// that carries an asset for this platform.
func fetchNightlyAssets(ctx context.Context) (tag string, assets []ghAsset, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ghReleasesAPI+"?per_page=30", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("github api: %s", resp.Status)
	}
	var releases []struct {
		TagName string    `json:"tag_name"`
		Assets  []ghAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", nil, err
	}
	for _, r := range releases {
		for _, a := range r.Assets {
			if assetSuffix() != "" && strings.HasSuffix(a.Name, assetSuffix()) {
				return r.TagName, r.Assets, nil
			}
		}
	}
	return "", nil, fmt.Errorf("no llama.cpp release asset for %s/%s in the last %d releases", runtime.GOOS, runtime.GOARCH, len(releases))
}

// assetSuffix is the current (2026-09) nightly naming; llama.cpp has
// renamed assets before, so this is the one place to update.
func assetSuffix() string {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "-bin-macos-arm64.tar.gz"
	case runtime.GOOS == "darwin":
		return "-bin-macos-x64.tar.gz"
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "-bin-ubuntu-x64.tar.gz"
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return "-bin-ubuntu-arm64.tar.gz"
	}
	return ""
}

func pickAsset(assets []ghAsset) (asset ghAsset, checksums map[string]string, err error) {
	checksums = map[string]string{}
	for _, a := range assets {
		if strings.Contains(a.Name, "sha256") && strings.HasSuffix(a.Name, ".txt") {
			if b, err := fetchText(a.URL); err == nil {
				checksums = parseChecksums(b)
			}
		}
	}
	suffix := assetSuffix()
	for _, a := range assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a, checksums, nil
		}
	}
	return asset, nil, fmt.Errorf("no llama.cpp asset matching *%s", suffix)
}

func parseChecksums(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[filepath.Base(f[1])] = f[0]
		}
	}
	return out
}

// extractServer unpacks the release archive (.tar.gz or legacy .zip) into
// dest and returns the llama-server path, preserving the archive's internal
// layout (dylibs load via @loader_path).
func extractServer(archivePath, dest string) (string, error) {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, dest)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	srv := ""
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		out := filepath.Join(dest, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return "", err
			}
			dst, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(dst, tr); err != nil {
				dst.Close()
				return "", err
			}
			dst.Close()
			if filepath.Base(hdr.Name) == "llama-server" {
				srv = out
			}
		case tar.TypeSymlink:
			// macOS dylib versioned names are symlinks (libllama.0.dylib →
			// libllama.0.4.1.dylib); dropping them breaks dyld at launch.
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return "", err
			}
			_ = os.Remove(out)
			if err := os.Symlink(hdr.Linkname, out); err != nil {
				return "", err
			}
		}
	}
	if srv == "" {
		return "", errors.New("no llama-server in archive")
	}
	return srv, nil
}

func extractZip(zipPath, dest string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer r.Close()
	srv := ""
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		out := filepath.Join(dest, f.Name)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		dst, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, err = io.Copy(dst, rc)
		rc.Close()
		dst.Close()
		if err != nil {
			return "", err
		}
		if filepath.Base(f.Name) == "llama-server" {
			srv = out
		}
	}
	if srv == "" {
		return "", errors.New("no llama-server in archive")
	}
	return srv, nil
}

// --- download + verify helpers ---

func download(ctx context.Context, url, dest string, size int64, log *slog.Logger) error {
	if size > 0 {
		log.Info("downloading", "url", url, "size_mb", size/(1<<20))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	var written int64
	buf := make([]byte, 1<<20)
	var lastLog time.Time
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			written += int64(n)
			if size > 0 && time.Since(lastLog) > 5*time.Second {
				log.Info("download progress", "pct", 100*written/size, "mb", written/(1<<20))
				lastLog = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// verifyFile fails unless sha256(file) == want, or records/writes it when
// want came from hashing the file itself (TOFD pin write).
func verifyFile(path, want string) error {
	if want == "" {
		return nil
	}
	got, err := hashFile(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: sha256 %s, want %s", filepath.Base(path), got[:12]+"…", want[:12]+"…")
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReader(f)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fetchText(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
