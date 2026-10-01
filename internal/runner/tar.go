package runner

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
)

type tarFile struct {
	Name string
	Mode int64
	Data []byte
}

// tarball builds an in-memory tar of flat files (no directories).
func tarball(files []tarFile) *bytes.Buffer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		_ = tw.WriteHeader(&tar.Header{Name: f.Name, Mode: mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(f.Data)
	}
	_ = tw.Close()
	return &buf
}

// untarFirst returns the content of the first regular file in a tar stream,
// refusing anything larger than limit.
func untarFirst(r io.Reader, limit int64) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("empty archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA still appears in old archives
			continue
		}
		if h.Size > limit {
			return nil, errTooLarge
		}
		return io.ReadAll(io.LimitReader(tr, limit+1))
	}
}

var errTooLarge = errors.New("file exceeds size limit")
