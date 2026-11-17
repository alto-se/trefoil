package build

import (
	"archive/zip"
	"io"
	"path"
	"slices"
	"time"
)

// zipModified is the time every entry carries, so the same files always zip
// to the same bytes.
var zipModified = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteZip writes the context as a zip with every file under dir/, so it
// unpacks to a folder that `docker build dir` builds. Entries are sorted and
// carry a fixed time so the same context always zips to the same bytes.
func (bc *Context) WriteZip(w io.Writer, dir string) error {
	zw := zip.NewWriter(w)
	if err := bc.AddToZip(zw, dir); err != nil {
		return err
	}
	return zw.Close()
}

// AddToZip adds the Dockerfile and files under dir/ to an open zip, sorted
// as in WriteZip.
func (bc *Context) AddToZip(zw *zip.Writer, dir string) error {
	files := map[string][]byte{"Dockerfile": []byte(bc.Dockerfile)}
	for name, data := range bc.Files {
		files[name] = data
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ZipFile(zw, path.Join(dir, name), files[name]); err != nil {
			return err
		}
	}
	return nil
}

// ZipFile adds one file to an open zip with the fixed time and mode.
func ZipFile(zw *zip.Writer, name string, data []byte) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipModified}
	h.SetMode(0o644)
	f, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}
