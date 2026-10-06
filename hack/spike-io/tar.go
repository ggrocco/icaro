package main

import (
	"archive/tar"
	"errors"
	"io"
)

type tarWriter struct{ tw *tar.Writer }

func newTarWriter(w io.Writer) *tarWriter { return &tarWriter{tw: tar.NewWriter(w)} }

func (t *tarWriter) add(name, data string) {
	_ = t.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = t.tw.Write([]byte(data))
}

func (t *tarWriter) close() { _ = t.tw.Close() }

func untarFirst(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("empty archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 1<<20))
		}
	}
}
