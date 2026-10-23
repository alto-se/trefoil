package build

import (
	"archive/zip"
	"io"
	"path"
	"slices"
	"time"
)

// WriteZip writes the context as a zip with every file under dir/, so it
// unpacks to a folder that `docker build dir` builds. Entries are sorted and
// carry a fixed time so the same context always zips to the same bytes.
func (bc *Context) WriteZip(w io.Writer, dir string) error {
	files := map[string][]byte{"Dockerfile": []byte(bc.Dockerfile)}
	for name, data := range bc.Files {
		files[name] = data
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	zw := zip.NewWriter(w)
	modified := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		h := &zip.FileHeader{Name: path.Join(dir, name), Method: zip.Deflate, Modified: modified}
		h.SetMode(0o644)
		f, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := f.Write(files[name]); err != nil {
			return err
		}
	}
	return zw.Close()
}
