package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reagent/config"
	"reagent/errdefs"
	"runtime"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

type DownloadProgress struct {
	FilePath      string
	Increment     uint64
	CurrentBytes  uint64
	TotalFileSize uint64
}

var DownloadLocks = make(map[string]*semaphore.Weighted)

func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func OverwriteFile(filePath string, value string) error {
	file, err := os.OpenFile(filePath, os.O_TRUNC|os.O_WRONLY, 0)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(file, "%s", value)
	if err != nil {
		return err
	}

	return err
}

func ReadFileInTgz(tarPath string, fileToFind string) (*tar.Reader, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	gzf, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}

	tarReader := tar.NewReader(gzf)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if path.Base(header.Name) == fileToFind {
				return tarReader, nil
			}
		}
	}

	return nil, errdefs.ErrNotFound
}

type WriteCounter struct {
	callback func(DownloadProgress)
	Size     uint64
	Total    uint64
	FilePath string
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Total += uint64(n)

	progress := DownloadProgress{
		Increment:     uint64(n),
		FilePath:      wc.FilePath,
		CurrentBytes:  wc.Total,
		TotalFileSize: wc.Size,
	}

	if wc.callback != nil {
		wc.callback(progress)
	}
	return n, nil
}

func GetTunnelBinaryPath(config *config.Config, binaryName string) string {
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	return filepath.Join(config.CommandLineArguments.AgentDir, binaryName)
}

// Downloads any data from a given URL to a given filePath. Progress is logged to CLI
func DownloadURL(filePath string, url string, callback func(DownloadProgress)) error {
	var currentLock *semaphore.Weighted
	if DownloadLocks[filePath] == nil {
		currentLock = semaphore.NewWeighted(1)
		DownloadLocks[filePath] = currentLock
	} else {
		currentLock = DownloadLocks[filePath]
	}

	log.Debug().Msgf("Trying to acquire download lock for %s", filePath)
	if !currentLock.TryAcquire(1) {
		return errdefs.InProgress(errors.New("download already in progress"))
	}

	defer func() {
		currentLock.Release(1)
		delete(DownloadLocks, filePath)
	}()

	client := http.Client{
		Transport: &http.Transport{
			// Honour HTTP(S)_PROXY/NO_PROXY for the OTA binary download — a
			// custom Transport otherwise ignores them, stranding agents behind
			// a corporate proxy on their first-installed version.
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			// One-shot client like remoteFileClient: close the connection with
			// the response instead of leaving it idle in an unused pool.
			DisableKeepAlives: true,
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	// A non-2xx body must never land on disk as the "binary": a 404 page has a
	// Content-Length like any other response, and with the .sha256 sidecar
	// equally missing the checksum step only warns, so it could get installed.
	// 206 is accepted alongside 200 in case a Range request is ever used.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download of %s failed: unexpected HTTP status %s", url, resp.Status)
	}

	size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
	if err != nil {
		return err
	}

	// The destination is created only once the response is accepted, so a
	// rejected download leaves nothing behind; a body that breaks off is
	// removed the same way rather than left as a truncated binary.
	out, err := os.Create(filePath)
	if err != nil {
		return err
	}

	defer func() { _ = out.Close() }()

	// copy the http body into the file
	counter := &WriteCounter{callback: callback, Size: uint64(size), FilePath: filePath}
	if _, err = io.Copy(out, io.TeeReader(resp.Body, counter)); err != nil {
		_ = out.Close() // Windows cannot remove an open file
		if removeErr := os.Remove(filePath); removeErr != nil {
			log.Warn().Err(removeErr).Msgf("failed to remove partial download %s", filePath)
		}
		return err
	}

	return nil
}
