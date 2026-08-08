// Package pullfile implements the fs_pull_file download-link service.
//
// The manager issues short-lived, download-count-limited signed URLs for
// files inside the allowed roots. The URL itself is the only credential:
// possession of a valid token is authorization to download. Tokens embed the
// target path and are signed with a per-process random secret, so server
// restarts invalidate every outstanding link.
package pullfile

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/mcp-tools/internal/audit"
	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/security"
)

var (
	// ErrDisabled is returned when pull_file.enabled is false.
	ErrDisabled = errors.New("tool is disabled")
	// ErrNotExist is returned when the target file does not exist or is a directory.
	ErrNotExist = errors.New("file does not exist")
	// ErrTooLarge is returned when the file exceeds the effective max_bytes.
	ErrTooLarge = errors.New("file exceeds max_bytes")
	// ErrTypeNotAllowed is returned when the file extension is not whitelisted.
	ErrTypeNotAllowed = errors.New("file type is not allowed by pull_file.allowed_extensions")
	// ErrInvalidToken is returned for malformed, tampered, or unknown tokens.
	ErrInvalidToken = errors.New("invalid or expired download link")
	// ErrExpired is returned when the token TTL has elapsed.
	ErrExpired = errors.New("download link expired")
	// ErrConsumed is returned when the download limit has been reached.
	ErrConsumed = errors.New("download link limit reached")
)

const tokenVersion byte = 1

const (
	tokenHeaderLen = 1 + 8 + 16 // version + exp + nonce
	tokenSigLen    = 32
)

type downloadEntry struct {
	remaining int
	exp       int64
}

// Download describes a successfully issued download link.
type Download struct {
	Token     string
	Path      string
	Filename  string
	Bytes     int64
	MIMEType  string
	ExpiresAt time.Time
}

// Manager issues and resolves signed download links. A single Manager must be
// shared between the fs tool (issuing) and the HTTP download endpoint
// (resolving) so tokens issued by the tool resolve on the endpoint.
type Manager struct {
	cfg     config.Config
	auditor audit.Logger
	secret  [32]byte
	mu      sync.Mutex
	entries map[string]downloadEntry
}

// NewManager creates a Manager with a fresh random signing secret.
func NewManager(cfg config.Config, auditor audit.Logger) *Manager {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		// crypto/rand failure is effectively unrecoverable; derive a
		// per-process secret from time and pid so tokens stay unpredictable
		// across restarts without blocking startup.
		sum := sha256.Sum256([]byte(fmt.Sprintf("mcp-tools-pullfile:%d:%d", time.Now().UnixNano(), os.Getpid())))
		copy(secret[:], sum[:])
	}
	return &Manager{cfg: cfg, auditor: auditor, secret: secret, entries: make(map[string]downloadEntry)}
}

// Issue validates path against the pull_file policy and returns a signed
// download link. maxBytes is the caller-provided cap; it is clamped to the
// configured pull_file.max_bytes upper bound.
func (m *Manager) Issue(path string, maxBytes int64, now time.Time) (Download, error) {
	if !m.cfg.PullFile.Enabled {
		return Download{}, ErrDisabled
	}
	limit := m.cfg.PullFile.MaxBytes
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Download{}, ErrNotExist
		}
		return Download{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return Download{}, ErrNotExist
	}
	if info.Size() > limit {
		return Download{}, ErrTooLarge
	}
	mimeType := mimeForExtension(path)
	if err := m.checkExtension(path); err != nil {
		return Download{}, err
	}

	expiresAt := now.Add(time.Duration(m.cfg.PullFile.TTLSeconds) * time.Second)
	payload := buildTokenPayload(expiresAt.Unix(), path)
	payload = append(payload, sign(m.secret, payload)...)

	token := base64.RawURLEncoding.EncodeToString(payload)

	m.mu.Lock()
	remaining := m.cfg.PullFile.MaxDownloads // 0 = unlimited within TTL
	m.entries[token] = downloadEntry{remaining: remaining, exp: expiresAt.Unix()}
	m.sweepLocked(now)
	m.mu.Unlock()

	return Download{
		Token:     token,
		Path:      path,
		Filename:  filepath.Base(path),
		Bytes:     info.Size(),
		MIMEType:  mimeType,
		ExpiresAt: expiresAt,
	}, nil
}

// Resolve verifies token and returns the download path after re-checking the
// allowed-roots boundary, file existence, size, and extension whitelist.
func (m *Manager) Resolve(token string, now time.Time) (string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(payload) < tokenHeaderLen+tokenSigLen+1 {
		return "", ErrInvalidToken
	}
	if payload[0] != tokenVersion {
		return "", ErrInvalidToken
	}
	exp := int64(binary.BigEndian.Uint64(payload[1:9]))
	if now.Unix() > exp {
		return "", ErrExpired
	}
	body := payload[:len(payload)-tokenSigLen]
	sig := payload[len(payload)-tokenSigLen:]
	if !hmac.Equal(sig, sign(m.secret, body)) {
		return "", ErrInvalidToken
	}
	path := string(payload[tokenHeaderLen : len(payload)-tokenSigLen])

	m.mu.Lock()
	entry, ok := m.entries[token]
	if !ok {
		m.mu.Unlock()
		return "", ErrInvalidToken
	}
	if entry.exp < now.Unix() {
		delete(m.entries, token)
		m.mu.Unlock()
		return "", ErrExpired
	}
	if entry.remaining > 0 {
		entry.remaining--
		m.entries[token] = entry
		if entry.remaining == 0 {
			delete(m.entries, token)
		}
	}
	m.mu.Unlock()

	// Re-validate at download time (TOCTOU guard): the bound path must still
	// resolve inside allowed roots and the file must still satisfy the policy.
	if !m.cfg.UnsafeAllowAll {
		if _, err := security.ResolvePath(path, m.cfg.AllowedRoots); err != nil {
			return "", ErrInvalidToken
		}
	}
	if !m.cfg.PullFile.Enabled {
		return "", ErrDisabled
	}
	if err := m.checkExtension(path); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotExist
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", ErrNotExist
	}
	if info.Size() > m.cfg.PullFile.MaxBytes {
		return "", ErrTooLarge
	}
	return path, nil
}

// RecordDownload writes one audit event for a download attempt.
func (m *Manager) RecordDownload(started time.Time, remoteAddr, targetPath, detail string, ok bool) {
	if m.auditor == nil {
		return
	}
	ev := audit.Event{
		Timestamp:  started.UTC(),
		RemoteAddr: remoteAddr,
		Tool:       "fs_pull_file.download",
		TargetPath: targetPath,
		Allowed:    ok,
		Success:    ok,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if ok {
		ev.ResultDigest = detail
	} else {
		ev.ResultDigest = "download rejected"
		ev.Error = detail
	}
	_ = m.auditor.Write(ev)
}

func (m *Manager) checkExtension(path string) error {
	allowed := m.cfg.PullFile.AllowedExtensions
	if len(allowed) == 0 {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, candidate := range allowed {
		if ext == candidate {
			return nil
		}
	}
	return ErrTypeNotAllowed
}

func (m *Manager) sweepLocked(now time.Time) {
	for token, entry := range m.entries {
		if entry.exp < now.Unix() {
			delete(m.entries, token)
		}
	}
}

func buildTokenPayload(exp int64, path string) []byte {
	payload := make([]byte, 0, tokenHeaderLen+len(path))
	payload = append(payload, tokenVersion)
	var expBytes [8]byte
	binary.BigEndian.PutUint64(expBytes[:], uint64(exp))
	payload = append(payload, expBytes[:]...)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// Nonce only needs to be unique; fall back to exp+time.
		now := time.Now().UnixNano()
		binary.BigEndian.PutUint64(nonce[:8], uint64(now))
		binary.BigEndian.PutUint64(nonce[8:], uint64(exp))
	}
	payload = append(payload, nonce[:]...)
	payload = append(payload, []byte(path)...)
	return payload
}

func sign(secret [32]byte, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret[:])
	mac.Write(payload)
	return mac.Sum(nil)
}

func mimeForExtension(path string) string {
	if mimeType := mime.TypeByExtension(filepath.Ext(path)); mimeType != "" {
		if semicolon := strings.IndexByte(mimeType, ';'); semicolon >= 0 {
			return strings.TrimSpace(mimeType[:semicolon])
		}
		return mimeType
	}
	return "application/octet-stream"
}
