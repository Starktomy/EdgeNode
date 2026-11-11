package nodes

import (
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// normalizeRootPath accepts a URL path, never an operating-system path.
func normalizeRootPath(requestPath string, decode bool, stripPrefix string, exceptHidden bool) (string, error) {
	if decode {
		decoded, err := url.PathUnescape(requestPath)
		if err != nil {
			return "", fs.ErrInvalid
		}
		requestPath = decoded
	}
	if strings.ContainsAny(requestPath, "\\\x00") {
		return "", fs.ErrInvalid
	}
	if stripPrefix != "" {
		if !strings.HasPrefix(stripPrefix, "/") {
			stripPrefix = "/" + stripPrefix
		}
		requestPath = strings.TrimPrefix(requestPath, stripPrefix)
	}
	for _, component := range strings.Split(requestPath, "/") {
		if component == ".." || (exceptHidden && strings.HasPrefix(component, ".") && component != "") {
			return "", fs.ErrPermission
		}
	}
	// Strip only the URL's leading slash. A second leading slash is not a local path.
	relative := strings.TrimPrefix(requestPath, "/")
	if strings.HasPrefix(relative, "/") {
		return "", fs.ErrInvalid
	}
	relative = path.Clean(relative)
	if !fs.ValidPath(relative) || !filepath.IsLocal(filepath.FromSlash(relative)) {
		return "", fs.ErrPermission
	}
	return relative, nil
}

// openRootFile keeps both index discovery and the final open inside the same root.
// os.Root also confines symlinks and protects the open against path replacement races.
func openRootFile(rootDir string, relative string, indexes []string, exceptHidden bool) (*os.File, string, error) {
	if !fs.ValidPath(relative) || !filepath.IsLocal(filepath.FromSlash(relative)) {
		return nil, "", fs.ErrPermission
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()

	file, err := root.Open(filepath.FromSlash(relative))
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if info.Mode().IsRegular() {
		return file, relative, nil
	}
	_ = file.Close()
	if !info.IsDir() {
		return nil, "", fs.ErrPermission
	}

	for _, index := range indexes {
		indexPath, err := normalizeRootPath(index, false, "", exceptHidden)
		if err != nil || indexPath == "." || strings.HasPrefix(index, "/") {
			continue
		}
		var candidates = []string{path.Join(relative, indexPath)}
		if strings.Contains(indexPath, "*") {
			candidates, err = fs.Glob(root.FS(), candidates[0])
			if err != nil {
				continue
			}
		}
		for _, candidate := range candidates {
			if exceptHidden {
				if _, err := normalizeRootPath(candidate, false, "", true); err != nil {
					continue
				}
			}
			indexFile, err := root.Open(filepath.FromSlash(candidate))
			if err != nil {
				continue
			}
			info, err := indexFile.Stat()
			if err == nil && info.Mode().IsRegular() {
				return indexFile, candidate, nil
			}
			_ = indexFile.Close()
		}
	}
	return nil, "", fs.ErrNotExist
}
