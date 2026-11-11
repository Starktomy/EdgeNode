package utils

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const maxNodeArchiveEntries = 20000
const maxNodeExtractedBytes uint64 = 1 << 30

type Unzip struct {
	zipFile     string
	targetDir   string
	stripPrefix string
}

func NewUnzip(zipFile, targetDir, stripPrefix string) *Unzip {
	return &Unzip{zipFile: zipFile, targetDir: targetDir, stripPrefix: stripPrefix}
}

// Run validates the entire archive before replacing any file. os.Root confines
// creation and rename, including when a local symlink changes during extraction.
func (u *Unzip) Run() error {
	if u.zipFile == "" || u.targetDir == "" {
		return errors.New("archive and destination are required")
	}
	archive, err := zip.OpenReader(u.zipFile)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) > maxNodeArchiveEntries {
		return errors.New("too many archive entries")
	}
	type entry struct {
		file *zip.File
		name string
	}
	var entries []entry
	var total uint64
	seen := map[string]bool{}
	prefix := strings.TrimSuffix(u.stripPrefix, "/")
	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "" || !fs.ValidPath(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:") || (!file.Mode().IsRegular() && !file.Mode().IsDir()) {
			return fmt.Errorf("unsafe archive entry %q", file.Name)
		}
		if prefix != "" {
			if name == prefix && file.FileInfo().IsDir() {
				continue
			}
			if !strings.HasPrefix(name, prefix+"/") {
				return errors.New("archive entry is outside the expected package")
			}
			name = strings.TrimPrefix(name, prefix+"/")
		}
		if seen[name] {
			return errors.New("duplicate archive entry")
		}
		seen[name] = true
		if file.UncompressedSize64 > 256<<20 || total > maxNodeExtractedBytes-file.UncompressedSize64 {
			return errors.New("archive exceeds extraction budget")
		}
		total += file.UncompressedSize64
		entries = append(entries, entry{file, name})
	}
	if err := os.MkdirAll(u.targetDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(u.targetDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive destination must be a real directory")
	}
	root, err := os.OpenRoot(u.targetDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range entries {
		dir := path.Dir(entry.name)
		if entry.file.FileInfo().IsDir() {
			dir = entry.name
		}
		current := ""
		if dir != "." {
			for _, part := range strings.Split(dir, "/") {
				current = path.Join(current, part)
				if err := root.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
					return err
				}
				info, err := root.Lstat(current)
				if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return errors.New("archive parent must be a real directory")
				}
			}
		}
		if entry.file.FileInfo().IsDir() {
			continue
		}
		if err := replaceNodeArchiveFile(root, entry.file, entry.name); err != nil {
			return err
		}
	}
	return nil
}

func replaceNodeArchiveFile(root *os.Root, file *zip.File, name string) error {
	if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return errors.New("archive target must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(name), ".edge-unzip-"+hex.EncodeToString(nonce[:]))
	mode := os.FileMode(0600)
	if file.Mode().Perm()&0111 != 0 {
		mode = 0700
	}
	writer, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { writer.Close(); root.Remove(temporary) }()
	count, err := io.Copy(writer, io.LimitReader(reader, int64(file.UncompressedSize64)+1))
	if err != nil {
		return err
	}
	if count != int64(file.UncompressedSize64) {
		return errors.New("archive entry length differs from its header")
	}
	if err := writer.Sync(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
