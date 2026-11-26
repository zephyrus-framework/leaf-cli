// Package overlay merges embedded defaults with the user's project tree into
// a single directory that the PHP runtime treats as ROOT_DIR.
//
// Order of precedence (last write wins):
//  1. Embedded defaults (templates, assets, fallback config)
//  2. User project files (content/, templates/, public/, config.yml)
//
// Files replace each other whole, except under a Source's MergeJSON prefixes,
// where JSON objects are merged key by key.
//
// The merged tree lives under a tempdir for the duration of the build and is
// cleaned up afterwards.
package overlay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Source describes one layer of the overlay stack. A Source with higher
// Priority wins. Zero-value FS means "use the filesystem at Root."
type Source struct {
	Name     string   // "defaults", "user", etc. Used in error messages.
	Priority int      // higher wins
	FS       fs.FS    // if nil, OS filesystem under Root is used
	Root     string   // absolute path when FS is nil
	Skip     []string // top-level path prefixes this source must not contribute
	// Top-level path prefixes where a file already present in dst is kept.
	KeepExisting []string
	// Top-level path prefixes where a .json file already present in dst is
	// merged with this source's file (objects recursively, other values taken
	// whole from the winner) instead of replaced. The source wins, except
	// under KeepExisting, where dst wins.
	MergeJSON []string
}

// Merge walks each source in priority order (ascending) and writes files into
// dst, overwriting earlier layers with later ones. The dst directory must
// exist and should be empty.
func Merge(dst string, sources []Source) error {
	ordered := make([]Source, len(sources))
	copy(ordered, sources)
	// Insertion sort by priority ascending; small N, keeps order stable.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j-1].Priority > ordered[j].Priority; j-- {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}
	for _, src := range ordered {
		if err := copyLayer(src, dst); err != nil {
			return fmt.Errorf("overlay source %q: %w", src.Name, err)
		}
	}
	return nil
}

func copyLayer(src Source, dst string) error {
	// OS-backed path: follow symlinks so dev-mode vendor checkouts work.
	if src.FS == nil {
		if src.Root == "" {
			return nil
		}
		return copyOSDir(src.Root, dst, src)
	}
	// Abstract fs.FS (e.g. go:embed): no symlinks possible, walk directly.
	fsys := src.FS
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if underPrefix(path, src.Skip) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return place(path, target, src, func() (io.ReadCloser, error) { return fsys.Open(path) })
	})
}

// copyOSDir walks the filesystem tree at root, following symlinks (including
// directory symlinks, common in local Composer workspace setups), and writes
// a real-file copy under dst. Missing roots are not an error (the user may
// not have a templates/ dir yet, for example).
func copyOSDir(root, dst string, src Source) error {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return walkCopy(root, dst, "", src, make(map[string]bool))
}

func walkCopy(srcRoot, dstRoot, rel string, src Source, seen map[string]bool) error {
	if rel != "" && underPrefix(rel, src.Skip) {
		return nil
	}

	srcPath := filepath.Join(srcRoot, rel)
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}

	// Follow symlinks, with cycle protection via resolved-path set.
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(srcPath)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", srcPath, err)
		}
		if seen[resolved] {
			return nil // cycle, stop
		}
		seen[resolved] = true
		info, err = os.Stat(resolved)
		if err != nil {
			return err
		}
		srcPath = resolved
	}

	dstPath := filepath.Join(dstRoot, rel)

	if info.IsDir() {
		if rel != "" {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(srcPath)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := filepath.Join(rel, e.Name())
			// Recurse from the original srcRoot so relative paths stay consistent;
			// but the data comes from the resolved srcPath via an intermediate.
			if err := walkCopyAt(srcPath, e.Name(), dstRoot, childRel, src, seen); err != nil {
				return err
			}
		}
		return nil
	}

	if !info.Mode().IsRegular() {
		return nil
	}
	return place(rel, dstPath, src, func() (io.ReadCloser, error) { return os.Open(srcPath) })
}

// walkCopyAt handles a child entry whose source lives under a (possibly
// resolved-via-symlink) directory different from the original root. relFromDst
// is kept so the destination layout mirrors the logical source tree.
func walkCopyAt(parentSrc, childName, dstRoot, relFromDst string, src Source, seen map[string]bool) error {
	if underPrefix(relFromDst, src.Skip) {
		return nil
	}
	childSrc := filepath.Join(parentSrc, childName)
	info, err := os.Lstat(childSrc)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(childSrc)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", childSrc, err)
		}
		if seen[resolved] {
			return nil
		}
		seen[resolved] = true
		info, err = os.Stat(resolved)
		if err != nil {
			return err
		}
		childSrc = resolved
	}
	dstPath := filepath.Join(dstRoot, relFromDst)
	if info.IsDir() {
		if err := os.MkdirAll(dstPath, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(childSrc)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := filepath.Join(relFromDst, e.Name())
			if err := walkCopyAt(childSrc, e.Name(), dstRoot, childRel, src, seen); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return place(relFromDst, dstPath, src, func() (io.ReadCloser, error) { return os.Open(childSrc) })
}

func keepsExisting(rel, dstPath string, keeps []string) bool {
	if !underPrefix(filepath.ToSlash(rel), keeps) {
		return false
	}
	_, err := os.Lstat(dstPath)
	return err == nil
}

func underPrefix(path string, skips []string) bool {
	for _, s := range skips {
		if path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}

// place writes one source file to dstPath: kept, merged or copied, per the
// source's KeepExisting and MergeJSON prefixes.
func place(rel, dstPath string, src Source, open func() (io.ReadCloser, error)) error {
	keep := keepsExisting(rel, dstPath, src.KeepExisting)
	if underPrefix(filepath.ToSlash(rel), src.MergeJSON) && strings.HasSuffix(rel, ".json") {
		merged, ok, err := mergeJSONFile(dstPath, open, keep)
		if err != nil {
			return err
		}
		if ok {
			return writeBytes(dstPath, merged)
		}
	}
	if keep {
		return nil
	}
	return writeFile(dstPath, open)
}

// mergeJSONFile returns the merged document, or ok=false when dst is absent or
// either side is not a JSON object, so the plain file rule applies.
func mergeJSONFile(dstPath string, open func() (io.ReadCloser, error), dstWins bool) (merged []byte, ok bool, err error) {
	existing, err := os.ReadFile(dstPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	in, err := open()
	if err != nil {
		return nil, false, err
	}
	defer in.Close()
	incoming, err := io.ReadAll(in)
	if err != nil {
		return nil, false, err
	}
	base, okBase := decodeObject(existing)
	over, okOver := decodeObject(incoming)
	if !okBase || !okOver {
		return nil, false, nil
	}
	if dstWins {
		base, over = over, base
	}
	mergeObjects(base, over)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")
	if err := enc.Encode(base); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func decodeObject(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing content
	}
	return obj, true
}

// mergeObjects folds over into base: nested objects recurse, any other value
// from over replaces the one in base.
func mergeObjects(base, over map[string]any) {
	for k, v := range over {
		if bo, ok := base[k].(map[string]any); ok {
			if oo, ok := v.(map[string]any); ok {
				mergeObjects(bo, oo)
				continue
			}
		}
		base[k] = v
	}
}

func writeFile(dstPath string, open func() (io.ReadCloser, error)) error {
	in, err := open()
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func writeBytes(dstPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dstPath, data, 0o644)
}
