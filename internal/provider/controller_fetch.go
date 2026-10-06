package provider

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	cacheDirectoryName = "pveguest"
	cacheDirectoryMode = 0o700
	cachePartPattern   = ".download-*"

	controllerDownloadTimeout = 10 * time.Minute

	// The controller reads the extracted member into memory.
	maximumArchiveMemberBytes = 1 << 30

	fetchGuest      = "guest"
	fetchController = "controller"
)

type hashLocks struct {
	mutex sync.Mutex
	locks map[string]*sync.Mutex
}

var downloadLocks = &hashLocks{locks: map[string]*sync.Mutex{}}

func (h *hashLocks) lock(hash string) func() {
	h.mutex.Lock()
	entry, found := h.locks[hash]
	if !found {
		entry = &sync.Mutex{}
		h.locks[hash] = entry
	}
	h.mutex.Unlock()
	entry.Lock()
	return entry.Unlock
}

// controllerPayload is the file that the controller sends to the guest.
type controllerPayload struct {
	Content []byte
	SHA256  string
}

// validateArchiveMember rejects an absolute path and a path with a ..
// component. The guest extracts the member with the same name.
func validateArchiveMember(member string) error {
	if member == "" || strings.ContainsRune(member, 0) {
		return errors.New("archive_member must be a non-empty path without NUL characters")
	}
	if strings.HasPrefix(member, "/") {
		return fmt.Errorf("archive_member %q must be a relative path", member)
	}
	if slices.Contains(strings.Split(member, "/"), "..") {
		return fmt.Errorf("archive_member %q must not contain a .. component", member)
	}
	return nil
}

func cacheDirectory() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Error("find the home directory for the download cache failed", "err", err)
			return "", fmt.Errorf("find the home directory for the download cache: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, cacheDirectoryName), nil
}

// fileHasHash reports false for a missing or unreadable file. The caller then
// downloads the object again.
func fileHasHash(filePath string, wantHash string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false
	}
	return hex.EncodeToString(hasher.Sum(nil)) == wantHash
}

func newControllerHTTPClient(caFile string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		slog.Error("load the system certificate pool failed", "err", err)
		return nil, fmt.Errorf("load the system certificate pool: %w", err)
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			slog.Error("read controller_ca_file failed", "err", err)
			return nil, fmt.Errorf("read controller_ca_file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("controller_ca_file %s has no PEM certificate", caFile)
		}
	}
	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: transport, Timeout: controllerDownloadTimeout}, nil
}

// cachedArchive returns the path of the downloaded object in the cache. A
// cached file with the expected hash skips the download.
func cachedArchive(ctx context.Context, caFile string, url string, wantHash string) (string, error) {
	directory, err := cacheDirectory()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, cacheDirectoryMode); err != nil {
		slog.ErrorContext(ctx, "create the download cache directory failed", "err", err)
		return "", fmt.Errorf("create the download cache directory: %w", err)
	}
	cachedPath := filepath.Join(directory, wantHash)
	// Check the cache after acquiring the hash lock; another request may have filled it.
	unlock := downloadLocks.lock(wantHash)
	defer unlock()
	if fileHasHash(cachedPath, wantHash) {
		return cachedPath, nil
	}

	client, err := newControllerHTTPClient(caFile)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build the request for %s: %w", url, err)
	}
	response, err := client.Do(request)
	if err != nil {
		slog.ErrorContext(ctx, "controller download failed", "url", url, "err", err)
		return "", fmt.Errorf("download %s on the controller: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s on the controller: HTTP status %d", url, response.StatusCode)
	}

	part, err := os.CreateTemp(directory, cachePartPattern)
	if err != nil {
		return "", fmt.Errorf("create a cache file: %w", err)
	}
	partPath := part.Name()
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(part, hasher), response.Body)
	closeErr := part.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(partPath)
		slog.ErrorContext(ctx, "write the download cache file failed", "url", url, "err", err)
		return "", fmt.Errorf("download %s on the controller: %w", url, err)
	}
	receivedHash := hex.EncodeToString(hasher.Sum(nil))
	if receivedHash != wantHash {
		_ = os.Remove(partPath)
		return "", fmt.Errorf(
			"the download of %s has sha256 %s, but the configuration expects %s",
			url, receivedHash, wantHash,
		)
	}
	if err := os.Rename(partPath, cachedPath); err != nil {
		_ = os.Remove(partPath)
		return "", fmt.Errorf("move the download into the cache: %w", err)
	}
	return cachedPath, nil
}

func archiveStepError(step string, err error) error {
	slog.Error("extract an archive member failed", "step", step, "err", err)
	return fmt.Errorf("%s: %w", step, err)
}

func extractArchiveMember(archivePath string, member string) ([]byte, error) {
	if err := validateArchiveMember(member); err != nil {
		return nil, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, archiveStepError("open the archive", err)
	}
	defer func() { _ = file.Close() }()
	decompressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, archiveStepError("read the gzip stream of the archive", err)
	}
	defer func() { _ = decompressed.Close() }()

	wanted := path.Clean(member)
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no member %q", member)
		}
		if err != nil {
			return nil, archiveStepError("read the tar stream of the archive", err)
		}
		if header.Typeflag != tar.TypeReg || path.Clean(header.Name) != wanted {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(reader, maximumArchiveMemberBytes+1))
		if err != nil {
			return nil, archiveStepError("read the archive member "+member, err)
		}
		if len(content) > maximumArchiveMemberBytes {
			return nil, fmt.Errorf("the archive member %q exceeds %d bytes", member, maximumArchiveMemberBytes)
		}
		return content, nil
	}
}

// fetchControllerPayload downloads the object on the controller and returns the
// object, or the archive member when member is not empty.
func fetchControllerPayload(
	ctx context.Context,
	caFile string,
	url string,
	wantHash string,
	member string,
) (controllerPayload, error) {
	archivePath, err := cachedArchive(ctx, caFile, url, wantHash)
	if err != nil {
		return controllerPayload{}, err
	}
	var content []byte
	if member == "" {
		content, err = os.ReadFile(archivePath)
	} else {
		content, err = extractArchiveMember(archivePath, member)
	}
	if err != nil {
		slog.ErrorContext(ctx, "read the controller payload failed", "url", url, "err", err)
		return controllerPayload{}, fmt.Errorf("read the payload of %s: %w", url, err)
	}
	return controllerPayload{Content: content, SHA256: hashHex(content)}, nil
}

func gzipContent(content []byte) ([]byte, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(content); err != nil {
		slog.Error("compress the payload failed", "err", err)
		return nil, fmt.Errorf("compress the payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		slog.Error("compress the payload failed", "err", err)
		return nil, fmt.Errorf("compress the payload: %w", err)
	}
	return compressed.Bytes(), nil
}
