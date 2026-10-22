package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ExtractTarGz(source, dest string) error {
	gzipFile, err := os.Open(source)
	if err != nil {
		return err
	}

	defer func() { _ = gzipFile.Close() }()

	gzipReader, err := gzip.NewReader(gzipFile)
	if err != nil {
		return err
	}

	defer func() { _ = gzipReader.Close() }()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// The agent runs as root: an entry like "../../etc/x" or "/etc/x" would
		// write outside dest. Only names local to dest are extracted.
		if !filepath.IsLocal(header.Name) {
			return fmt.Errorf("archive entry %q points outside the target directory", header.Name)
		}
		target := filepath.Join(dest, header.Name) //nolint:gosec // G305: entry names are checked with filepath.IsLocal above

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil { //nolint:gosec // G301: app sources, readable by the app's build
				return err
			}
		case tar.TypeReg:
			if err := extractFile(target, tarReader); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown type: %v in %s", header.Typeflag, header.Name)
		}
	}

	return nil
}

// extractFile writes one regular archive entry and closes it right away (a
// defer inside the extraction loop kept every file open until the end).
func extractFile(target string, r io.Reader) error {
	file, err := os.Create(target) //nolint:gosec // G304: target is checked to stay inside dest
	if err != nil {
		return err
	}
	// Archive size is bounded by the upload that produced it (G110).
	if _, err := io.Copy(file, r); err != nil { //nolint:gosec // G110: see above
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0755); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
