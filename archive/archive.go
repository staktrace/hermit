// Package archive extracts archives with a progress bar.
package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	bufra "github.com/avvmoto/buf-readerat"
	"github.com/blakesmith/ar"
	"github.com/gabriel-vasile/mimetype"
	"github.com/pkg/errors"
	"github.com/saracen/go7z"
	"github.com/sassoftware/go-rpmutils"
	"github.com/xi2/xz"
	"howett.net/plist"

	"github.com/cashapp/hermit/manifest"
	"github.com/cashapp/hermit/ui"
	"github.com/cashapp/hermit/util"
)

// Extract from "source" to package destination.
func Extract(b *ui.Task, source string, pkg *manifest.Package) (err error) {
	task := b.SubTask("unpack")
	if _, err := os.Stat(pkg.Dest); err == nil {
		return errors.Errorf("destination %s already exists", pkg.Dest)
	}
	task.Debugf("Extracting %s to %s", source, pkg.Dest)
	// Do we need to rename the result to the final pkg.Dest?
	// This is set to false if we are recursively extracting packages within one another
	renameResult := true
	ext := filepath.Ext(source)
	switch ext {
	case ".pkg":
		return extractMacPKG(task, source, pkg.Dest, pkg.Strip)

	case ".dmg":
		return installMacDMG(task, source, pkg)
	}

	parentDir := filepath.Dir(pkg.Dest)
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		return errors.WithStack(err)
	}

	tmpDest, err := ioutil.TempDir(parentDir, filepath.Base(pkg.Dest)+"-*")
	if err != nil {
		return errors.WithStack(err)
	}

	// Cleanup or finalise temporary directory.
	defer func() {
		if err != nil {
			task.Tracef("rm -rf %q", tmpDest)
			_ = os.RemoveAll(tmpDest)
			return
		}
		tmpRoot := filepath.Join(tmpDest, strings.TrimPrefix(pkg.Root, pkg.Dest))
		for old, new := range pkg.Rename {
			task.Tracef("  mv %q %q", old, new)
			err = errors.WithStack(os.Rename(filepath.Join(tmpRoot, old), filepath.Join(tmpRoot, new)))
			if err != nil {
				break
			}
		}
		// Make the unpacked destination files read-only.
		err = filepath.Walk(tmpDest, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			task.Tracef("chmod a-w %q", path)
			return errors.WithStack(os.Chmod(path, info.Mode()&^0222))
		})
		if err != nil {
			return
		}
		// Make the base directory writeable so we can rename it.
		task.Tracef("chmod 700 %q", tmpDest)
		if err = errors.WithStack(os.Chmod(tmpDest, 0700)); err != nil { // nolint: gosec
			return
		}
		task.Tracef("mv %q %q", tmpDest, pkg.Dest)
		if renameResult {
			err = errors.WithStack(os.Rename(tmpDest, pkg.Dest))
		}
	}()

	f, r, mime, err := openArchive(source)
	if err != nil {
		return err
	}
	defer f.Close() // nolint: gosec

	info, err := f.Stat()
	if err != nil {
		return errors.WithStack(err)
	}

	task.Size(int(info.Size()))
	defer task.Done()
	r = io.TeeReader(r, task.ProgressWriter())

	// Archive is a single executable.
	switch mime.String() {
	case "application/zip":
		return extractZip(task, f, info, tmpDest, pkg.Strip)

	case "application/x-7z-compressed":
		return extract7Zip(f, info.Size(), tmpDest, pkg.Strip)

	case "application/x-mach-binary", "application/x-elf",
		"application/x-executable", "application/x-sharedlib":
		return extractExecutable(r, tmpDest, path.Base(pkg.Source))

	case "application/x-tar":
		return extractPackageTarball(task, r, tmpDest, pkg.Strip)

	case "application/vnd.debian.binary-package":
		renameResult = false
		return extractDebianPackage(task, r, tmpDest, pkg)

	case "application/x-rpm":
		return extractRpmPackage(r, tmpDest, pkg)

	default:
		return errors.Errorf("don't know how to extract archive %s of type %s", source, mime)
	}

}

type hdiEntry struct {
	DevEntry   string `plist:"dev-entry"`
	MountPoint string `plist:"mount-point"`
}

type hdi struct {
	SystemEntities []*hdiEntry `plist:"system-entities"`
}

func installMacDMG(b *ui.Task, source string, pkg *manifest.Package) error {
	dest := pkg.Dest + "~"
	err := os.MkdirAll(dest, 0700)
	if err != nil {
		return errors.WithStack(err)
	}
	defer os.RemoveAll(dest)
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.WithStack(err)
	}
	output, err := util.Capture(b, "hdiutil", "attach", "-plist", source)
	if err != nil {
		return errors.Wrap(err, "could not mount DMG")
	}
	list := &hdi{}
	_, err = plist.Unmarshal(output, list)
	if err != nil {
		return errors.WithStack(err)
	}
	var entry *hdiEntry
	for _, ent := range list.SystemEntities {
		if ent.MountPoint != "" && ent.DevEntry != "" {
			entry = ent
			break
		}
	}
	if entry == nil {
		return errors.New("couldn't determine volume information from hdiutil attach, volume may still be mounted :(")
	}
	defer util.Run(b, "hdiutil", "detach", entry.DevEntry) // nolint: errcheck
	switch {
	case len(pkg.Apps) != 0:
		for _, app := range pkg.Apps {
			base := filepath.Base(app)
			// Use rsync because reliably syncing all filesystem attributes is non-trivial.
			appDest := filepath.Join(dest, base)
			err = util.Run(b, "rsync", "-av",
				filepath.Join(entry.MountPoint, app)+"/",
				appDest+"/")
			if err != nil {
				return errors.WithStack(err)
			}
			err = os.Symlink(appDest, filepath.Join(home, "Applications", base))
			if err != nil {
				return errors.WithStack(err)
			}
		}
		return errors.WithStack(os.Rename(dest, pkg.Dest))

	default:
		return errors.New("manifest for does not provide a dmg{} block")
	}
}

func extractExecutable(r io.Reader, dest, executableName string) error {
	destExe := filepath.Join(dest, executableName)
	ext := filepath.Ext(destExe)
	switch ext {
	case ".gz", ".bz2", ".xz":
		destExe = strings.TrimSuffix(destExe, ext)
	}

	w, err := os.OpenFile(destExe, os.O_CREATE|os.O_WRONLY, 0700) // nolint: gosec
	if err != nil {
		return errors.WithStack(err)
	}
	defer w.Close() // nolint: gosec
	_, err = io.Copy(w, r)
	return errors.WithStack(err)
}

// Open a potentially compressed archive.
//
// It will return the MIME type of the underlying file, and a buffered io.Reader for that file.
func openArchive(source string) (f *os.File, r io.Reader, mime *mimetype.MIME, err error) {
	mime, err = mimetype.DetectFile(source)
	if err != nil {
		return nil, nil, mime, errors.WithStack(err)
	}
	f, err = os.Open(source)
	if err != nil {
		return nil, nil, mime, errors.WithStack(err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	r = f
	switch mime.String() {
	case "application/gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, mime, errors.WithStack(err)
		}
		r = zr

	case "application/x-bzip2":
		r = bzip2.NewReader(r)

	case "application/x-xz":
		xr, err := xz.NewReader(r, 0)
		if err != nil {
			return nil, nil, mime, errors.WithStack(err)
		}
		r = xr

	default:
		// Assume it's uncompressed?
		return f, r, mime, nil
	}

	// Now detect the underlying file type.
	buf := make([]byte, 4096)
	n, err := r.Read(buf)
	if err != nil && (!errors.Is(err, io.EOF) || n == 0) {
		return nil, nil, mime, errors.WithStack(err)
	}
	buf = buf[:n]
	mime = mimetype.Detect(buf)
	return f, io.MultiReader(bytes.NewReader(buf), r), mime, nil
}

const extractMacPkgChangesXML = `
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <array>
    <dict>
      <key>choiceAttribute</key>
      <string>customLocation</string>
      <key>attributeSetting</key>
      <string>${dest}</string>
      <key>choiceIdentifier</key>
      <string>default</string>
    </dict>
  </array>
</plist>
`

func extractMacPKG(b *ui.Task, path, dest string, strip int) error {
	if strip != 0 {
		return errors.Errorf("\"strip = %d\" is not supported for Mac installer .pkg files", strip)
	}
	err := os.MkdirAll(dest, 0700)
	if err != nil {
		return errors.WithStack(err)
	}
	task := b.SubProgress("install", 2)
	defer task.Done()
	changesf, err := ioutil.TempFile("", "hermit-*.xml")
	if err != nil {
		return errors.WithStack(err)
	}
	defer changesf.Close() // nolint: gosec
	defer os.Remove(changesf.Name())
	fmt.Fprint(changesf, os.Expand(extractMacPkgChangesXML, func(s string) string { return dest }))
	_ = changesf.Close()
	task.Add(1)
	return util.Run(b, "installer", "-verbose",
		"-pkg", path,
		"-target", "CurrentUserHomeDirectory",
		"-applyChoiceChangesXML", changesf.Name())
}

func extractZip(b *ui.Task, f *os.File, info os.FileInfo, dest string, strip int) error {
	zr, err := zip.NewReader(bufra.NewBufReaderAt(f, int(info.Size())), info.Size())
	if err != nil {
		return errors.WithStack(err)
	}
	task := b.SubProgress("unpack", len(zr.File))
	defer task.Done()
	for _, zf := range zr.File {
		b.Tracef("  %s", zf.Name)
		task.Add(1)
		destFile := makeDestPath(dest, zf.Name, strip)
		if destFile == "" {
			continue
		}
		err = extractZipFile(zf, destFile)
		if err != nil {
			return errors.Wrap(err, destFile)
		}
	}
	return nil
}

func extractZipFile(zf *zip.File, destFile string) error {
	zfr, err := zf.Open()
	if err != nil {
		return errors.WithStack(err)
	}
	defer zfr.Close()
	if zf.Mode().IsDir() {
		return errors.WithStack(os.MkdirAll(destFile, 0700))
	}
	w, err := os.OpenFile(destFile, os.O_CREATE|os.O_WRONLY, zf.Mode()&^0077)
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = io.Copy(w, zfr) // nolint: gosec
	if err != nil {
		return errors.WithStack(err)
	}
	err = w.Close()
	if err != nil {
		return errors.WithStack(err)
	}
	_ = os.Chtimes(destFile, zf.Modified, zf.Modified) // Best effort.
	return nil
}

func extractPackageTarball(b *ui.Task, r io.Reader, dest string, strip int) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return errors.WithStack(err)
		}
		mode := hdr.FileInfo().Mode() &^ 0077
		destFile := makeDestPath(dest, hdr.Name, strip)
		if destFile == "" {
			continue
		}
		b.Tracef("  %s -> %s", hdr.Name, destFile)
		err = os.MkdirAll(filepath.Dir(destFile), 0700)
		if err != nil {
			return errors.WithStack(err)
		}
		switch {
		case mode.IsDir():
			err = os.MkdirAll(destFile, 0700)
			if err != nil {
				return errors.Wrapf(err, "%s: failed to create directory", destFile)
			}

		case mode&os.ModeSymlink != 0:
			err = syscall.Symlink(hdr.Linkname, destFile)
			if err != nil {
				return errors.Wrapf(err, "%s: failed to create symlink to %s", destFile, hdr.Linkname)
			}

		case hdr.Typeflag&(tar.TypeLink|tar.TypeGNULongLink) != 0 && hdr.Linkname != "":
			// Convert hard links into symlinks so we don't have to track inodes later on during relocation.
			src := filepath.Join(dest, hdr.Linkname) // nolint: gosec
			rp, err := filepath.Rel(filepath.Dir(destFile), src)
			if err != nil {
				return errors.WithStack(err)
			}
			err = os.Symlink(rp, destFile)
			if err != nil {
				return errors.WithStack(err)
			}

		default:
			err := os.MkdirAll(filepath.Dir(destFile), 0700)
			if err != nil {
				return errors.WithStack(err)
			}
			w, err := os.OpenFile(destFile, os.O_CREATE|os.O_WRONLY, mode)
			if err != nil {
				return errors.WithStack(err)
			}
			_, err = io.Copy(w, tr) // nolint: gosec
			_ = w.Close()
			if err != nil {
				return errors.WithStack(err)
			}
			_ = os.Chtimes(destFile, hdr.AccessTime, hdr.ModTime) // Best effort.
		}
	}
	return nil
}

func extractDebianPackage(b *ui.Task, r io.Reader, dest string, pkg *manifest.Package) error {
	reader := ar.NewReader(r)
	for {
		header, err := reader.Next()
		if err != nil {
			return errors.WithStack(err)
		}
		if strings.HasPrefix(header.Name, "data.tar") {
			bytes := make([]byte, header.Size)
			_, err := reader.Read(bytes)
			if err != nil {
				return errors.WithStack(err)
			}
			filename := filepath.Join(dest, header.Name)
			err = ioutil.WriteFile(filename, bytes, 0600)
			if err != nil {
				return errors.WithStack(err)
			}
			return Extract(b, filename, pkg)
		}
	}
}

func extract7Zip(r io.ReaderAt, size int64, dest string, strip int) error {
	sz, err := go7z.NewReader(r, size)
	if err != nil {
		return errors.WithStack(err)
	}

	for {
		hdr, err := sz.Next()
		if errors.Is(err, io.EOF) {
			break // End of archive
		}
		if err != nil {
			return errors.WithStack(err)
		}

		// If empty stream (no contents) and isn't specifically an empty file...
		// then it's a directory.
		if hdr.IsEmptyStream && !hdr.IsEmptyFile {
			continue
		}
		destFile := makeDestPath(dest, hdr.Name, strip)
		if destFile == "" {
			continue
		}
		err = ensureDirExists(destFile)
		if err != nil {
			return errors.WithStack(err)
		}

		// Create file
		f, err := os.OpenFile(destFile, os.O_CREATE|os.O_RDWR, 0755) // nolint: gosec
		if err != nil {
			return errors.WithStack(err)
		}

		if _, err := io.Copy(f, sz); err != nil {
			_ = f.Close()
			return errors.WithStack(err)
		}
		if err = f.Close(); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func extractRpmPackage(r io.Reader, dest string, pkg *manifest.Package) error {
	rpm, err := rpmutils.ReadRpm(r)
	if err != nil {
		return errors.WithStack(err)
	}
	pr, err := rpm.PayloadReader()
	if err != nil {
		return errors.WithStack(err)
	}
	for {
		header, err := pr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.WithStack(err)
		}
		if header.Filesize() > 0 {
			bts := make([]byte, header.Filesize())
			_, err = pr.Read(bts)
			if err != nil {
				return errors.WithStack(err)
			}
			filename := makeDestPath(dest, header.Filename(), pkg.Strip)
			if filename == "" {
				continue
			}
			err = ensureDirExists(filename)
			if err != nil {
				return errors.WithStack(err)
			}
			err = ioutil.WriteFile(filename, bts, os.FileMode(header.Mode()))
			if err != nil {
				return errors.WithStack(err)
			}
		}
	}
	return nil
}

func ensureDirExists(file string) error {
	dir := filepath.Dir(file)
	return os.MkdirAll(dir, os.ModePerm)
}

// Strip leading path component.
func makeDestPath(dest, path string, strip int) string {
	parts := strings.Split(path, "/")
	if len(parts) <= strip {
		return ""
	}
	destFile := strings.Join(parts[strip:], "/")
	destFile = filepath.Join(dest, destFile)
	return destFile
}
