package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type Filesystem struct {
	activeTransfers     map[string]*ActiveFileTransfer // containerName --> transferID
	activeTransfersLock sync.Mutex
}

type ActiveFileTransfer struct {
	ID       string
	Current  uint64
	Total    uint64
	Canceled bool
	File     *os.File
}

type FileChunk struct {
	ID            string
	FileName      string
	FilePath      string
	Data          string
	ContainerName string
	Total         uint64
}

func New() Filesystem {
	return Filesystem{activeTransfers: make(map[string]*ActiveFileTransfer)}
}

func (fs *Filesystem) CancelFileTransfer(containerName string) {
	fs.activeTransfersLock.Lock()
	fileTransfer := fs.activeTransfers[containerName]
	fs.activeTransfersLock.Unlock()

	if fileTransfer == nil {
		return
	}

	fileTransfer.Canceled = true
	if fileTransfer.File != nil {
		_ = fileTransfer.File.Close()
	}
}

// CleanupFailedTransfer removes a failed transfer from the active transfers map.
// This should be called when a transfer error occurs to prevent stale state.
func (fs *Filesystem) CleanupFailedTransfer(containerName string) {
	fs.activeTransfersLock.Lock()
	transfer := fs.activeTransfers[containerName]
	if transfer != nil && transfer.File != nil {
		_ = transfer.File.Close()
	}
	delete(fs.activeTransfers, containerName)
	fs.activeTransfersLock.Unlock()
}

func (fs *Filesystem) GetActiveTransfer(containerName string) *ActiveFileTransfer {
	fs.activeTransfersLock.Lock()
	activeTransfer := fs.activeTransfers[containerName]
	fs.activeTransfersLock.Unlock()

	return activeTransfer
}

// Write decodes hex encoded data chunks and writes to a file.
//
// Matches implementation in file_transfer.ts (IronFlock Backend)
func (fs *Filesystem) Write(chunk FileChunk) error {
	filePath := chunk.FilePath + "/" + chunk.FileName

	if chunk.Data == "BEGIN" {
		fs.activeTransfersLock.Lock()

		// Close any previous transfer file for the same container
		if prev := fs.activeTransfers[chunk.ContainerName]; prev != nil && prev.File != nil {
			_ = prev.File.Close()
		}

		// O_TRUNC ensures old archive data is wiped atomically on open
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644) //nolint:gosec // G302: files a user transfers to the device for the device; same mode as before
		if err != nil {
			delete(fs.activeTransfers, chunk.ContainerName)
			fs.activeTransfersLock.Unlock()
			return err
		}

		fs.activeTransfers[chunk.ContainerName] = &ActiveFileTransfer{
			ID:    chunk.ID,
			Total: chunk.Total,
			File:  f,
		}
		fs.activeTransfersLock.Unlock()
		return nil
	}

	fs.activeTransfersLock.Lock()
	activeTransfer := fs.activeTransfers[chunk.ContainerName]
	fs.activeTransfersLock.Unlock()

	if activeTransfer == nil {
		return errors.New("We received a chunk without an active transfer")
	}

	if chunk.Data == "END" {
		// A stale END from a superseded transfer must not tear down the
		// transfer that currently owns the slot. When two publishes for the
		// same app overlap they share this containerName key; only the transfer
		// whose ID matches the active one may finalize and delete it. Without
		// this guard an earlier transfer's END deletes the live transfer's
		// entry, and its next chunk hits "no active transfer". BEGIN (last-wins,
		// closing the prior handle) and the data path already scope by ID.
		if activeTransfer.ID != chunk.ID {
			log.Debug().Msg("Ignoring END from a transfer that was reset")
			return nil
		}

		var err error
		if activeTransfer.File != nil {
			err = activeTransfer.File.Sync()
			if closeErr := activeTransfer.File.Close(); err == nil {
				err = closeErr
			}
		}

		fs.activeTransfersLock.Lock()
		delete(fs.activeTransfers, chunk.ContainerName)
		fs.activeTransfersLock.Unlock()

		return err
	}

	if activeTransfer.Canceled {
		return errors.New("canceled")
	}

	if activeTransfer.ID != chunk.ID {
		log.Debug().Msg("Received a chunk from transfer that was reset")
		return nil
	}

	if activeTransfer.File == nil {
		return errors.New("active transfer has no open file handle")
	}

	data, err := hex.DecodeString(chunk.Data)
	if err != nil {
		return err
	}

	n, err := activeTransfer.File.Write(data)
	if err != nil {
		return err
	}

	activeTransfer.Current += uint64(n)

	return nil
}

func decompressTgz(sourcePath string, targetPath string, targetFileName string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}

	defer func() { _ = file.Close() }()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}

	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)

	targetPath = filepath.Join(targetPath, targetFileName)
	writer, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()

	_, err = io.Copy(writer, tarReader) //nolint:gosec // G110: the archive is a user-initiated transfer to this device; disk use is bounded by the device storage guard
	return err
}
