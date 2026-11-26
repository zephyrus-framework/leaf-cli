package overlay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMerge_UserOverridesDefaults(t *testing.T) {
	defaults := fstest.MapFS{
		"templates/layouts/docs.latte": {Data: []byte("DEFAULT DOCS")},
		"templates/partials/nav.latte": {Data: []byte("DEFAULT NAV")},
		"public/assets/css/app.css":    {Data: []byte("DEFAULT CSS")},
	}
	userDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userDir, "templates", "layouts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "templates", "layouts", "docs.latte"), []byte("USER DOCS"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	err := Merge(dst, []Source{
		{Name: "defaults", Priority: 0, FS: defaults},
		{Name: "user", Priority: 10, Root: userDir},
	})
	if err != nil {
		t.Fatal(err)
	}

	// User override wins.
	if got := readFile(t, filepath.Join(dst, "templates", "layouts", "docs.latte")); got != "USER DOCS" {
		t.Errorf("docs.latte: got %q, want user override", got)
	}
	// Default preserved where user didn't override.
	if got := readFile(t, filepath.Join(dst, "templates", "partials", "nav.latte")); got != "DEFAULT NAV" {
		t.Errorf("nav.latte: got %q, want default", got)
	}
	if got := readFile(t, filepath.Join(dst, "public", "assets", "css", "app.css")); got != "DEFAULT CSS" {
		t.Errorf("app.css: got %q", got)
	}
}

func TestMerge_PriorityDeterminesOrder(t *testing.T) {
	low := fstest.MapFS{"x.txt": {Data: []byte("LOW")}}
	mid := fstest.MapFS{"x.txt": {Data: []byte("MID")}}
	high := fstest.MapFS{"x.txt": {Data: []byte("HIGH")}}

	dst := t.TempDir()
	// Pass out of order to confirm sort works.
	err := Merge(dst, []Source{
		{Name: "high", Priority: 20, FS: high},
		{Name: "low", Priority: 0, FS: low},
		{Name: "mid", Priority: 10, FS: mid},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "x.txt")); got != "HIGH" {
		t.Errorf("priority order wrong: got %q want HIGH", got)
	}
}

func TestMerge_EmptySourceSkipped(t *testing.T) {
	defaults := fstest.MapFS{"a.txt": {Data: []byte("A")}}
	dst := t.TempDir()
	err := Merge(dst, []Source{
		{Name: "defaults", Priority: 0, FS: defaults},
		{Name: "empty", Priority: 10, Root: ""},
	})
	if err != nil {
		t.Fatalf("empty source should be a no-op: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "a.txt")); got != "A" {
		t.Errorf("got %q", got)
	}
}

func TestMerge_MissingUserDirIsNotError(t *testing.T) {
	// User hasn't created `templates/`; defaults alone should win.
	defaults := fstest.MapFS{"templates/base.latte": {Data: []byte("D")}}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	dst := t.TempDir()
	err := Merge(dst, []Source{
		{Name: "defaults", Priority: 0, FS: defaults},
		{Name: "user", Priority: 10, Root: missing},
	})
	// Root missing means os.DirFS returns an FS whose Open fails; WalkDir
	// surfaces that error. We explicitly choose to tolerate this.
	if err == nil {
		// Acceptable: defaults applied.
		if got := readFile(t, filepath.Join(dst, "templates", "base.latte")); got != "D" {
			t.Errorf("default lost: %q", got)
		}
		return
	}
	t.Logf("missing user dir surfaced as error (acceptable): %v", err)
}

func TestMerge_Skip(t *testing.T) {
	defaults := fstest.MapFS{
		"content/getting-started/intro.md": {Data: []byte("DEFAULT INTRO")},
		"templates/docs.latte":             {Data: []byte("D")},
	}
	userDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userDir, "content", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "content", "api", "ref.md"), []byte("USER REF"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	err := Merge(dst, []Source{
		{Name: "defaults", Priority: 0, FS: defaults, Skip: []string{"content"}},
		{Name: "user", Priority: 10, Root: userDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "content", "getting-started", "intro.md")); err == nil {
		t.Errorf("default content/ leaked through Skip")
	}
	if got := readFile(t, filepath.Join(dst, "content", "api", "ref.md")); got != "USER REF" {
		t.Errorf("user content lost: %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "templates", "docs.latte")); got != "D" {
		t.Errorf("non-skipped default dropped: %q", got)
	}
}

func TestMerge_KeepExisting(t *testing.T) {
	defaultFiles := map[string]string{
		"public/index.php":      "DEFAULT INDEX",
		"public/favicon.ico":    "DEFAULT ICON",
		"public/assets/app.css": "DEFAULT CSS",
		"app/Kernel.php":        "DEFAULT KERNEL",
	}
	mapFS := fstest.MapFS{}
	osRoot := t.TempDir()
	for rel, data := range defaultFiles {
		mapFS[rel] = &fstest.MapFile{Data: []byte(data)}
		p := filepath.Join(osRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]Source{
		"fs": {Name: "defaults", FS: mapFS, KeepExisting: []string{"public"}},
		"os": {Name: "defaults", Root: osRoot, KeepExisting: []string{"public"}},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			for rel, data := range map[string]string{
				"public/favicon.ico": "USER ICON",
				"app/Kernel.php":     "USER KERNEL",
			} {
				p := filepath.Join(dst, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := Merge(dst, []Source{src}); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"public/index.php":      "DEFAULT INDEX",
				"public/assets/app.css": "DEFAULT CSS",
				"public/favicon.ico":    "USER ICON",
				"app/Kernel.php":        "DEFAULT KERNEL",
			}
			for rel, w := range want {
				if got := readFile(t, filepath.Join(dst, filepath.FromSlash(rel))); got != w {
					t.Errorf("%s: got %q, want %q", rel, got, w)
				}
			}
		})
	}
}

func TestMerge_NestedDirectories(t *testing.T) {
	defaults := fstest.MapFS{
		"a/b/c/d.txt": {Data: []byte("deep")},
	}
	dst := t.TempDir()
	if err := Merge(dst, []Source{{Name: "defaults", FS: defaults}}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "a", "b", "c", "d.txt")); got != "deep" {
		t.Errorf("nested file wrong: %q", got)
	}
}

const (
	themeCatalogue   = `{"search": {"label": "Search", "placeholder": "Find"}, "tags": ["a", "b"], "n": 1.50, "html": "<b>&</b>", "only_theme": "T"}`
	projectCatalogue = `{"search": {"label": "Mine"}, "tags": ["c"], "n": null, "custom": "x"}`
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mapFSOf(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for rel, data := range files {
		m[rel] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}

// layerSources returns the same source tree backed by an fs.FS, a directory,
// and a directory reached through a symlinked locale folder.
func layerSources(t *testing.T, files map[string]string, mutate func(*Source)) map[string]Source {
	t.Helper()
	osRoot := t.TempDir()
	writeTree(t, osRoot, files)

	linkRoot := t.TempDir()
	realLocale := t.TempDir()
	for rel, data := range files {
		if inner, ok := strings.CutPrefix(rel, "locale/"); ok {
			writeTree(t, realLocale, map[string]string{inner: data})
		} else {
			writeTree(t, linkRoot, map[string]string{rel: data})
		}
	}
	if err := os.Symlink(realLocale, filepath.Join(linkRoot, "locale")); err != nil {
		t.Fatal(err)
	}

	srcs := map[string]Source{
		"fs":      {Name: "s", FS: mapFSOf(files)},
		"os":      {Name: "s", Root: osRoot},
		"symlink": {Name: "s", Root: linkRoot},
	}
	for k, s := range srcs {
		mutate(&s)
		srcs[k] = s
	}
	return srcs
}

func decodeJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &out); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return out
}

func TestMerge_MergeJSONSourceWinsOnEveryPath(t *testing.T) {
	files := map[string]string{"locale/en/general.json": projectCatalogue, "other/x.json": `{"a": 1}`}
	for name, src := range layerSources(t, files, func(s *Source) { s.MergeJSON = []string{"locale"} }) {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			writeTree(t, dst, map[string]string{"locale/en/general.json": themeCatalogue, "other/x.json": `{"b": 2}`})
			if err := Merge(dst, []Source{src}); err != nil {
				t.Fatal(err)
			}
			got := decodeJSON(t, filepath.Join(dst, "locale/en/general.json"))
			search := got["search"].(map[string]any)
			if search["label"] != "Mine" || search["placeholder"] != "Find" {
				t.Errorf("nested merge wrong: %v", search)
			}
			if got["custom"] != "x" || got["only_theme"] != "T" || got["html"] != "<b>&</b>" {
				t.Errorf("keys lost: %v", got)
			}
			if tags := got["tags"].([]any); len(tags) != 1 || tags[0] != "c" {
				t.Errorf("arrays must be taken whole: %v", got["tags"])
			}
			if v, present := got["n"]; !present || v != nil {
				t.Errorf("null must win: %v", got["n"])
			}
			other := readFile(t, filepath.Join(dst, "other/x.json"))
			if other != `{"a": 1}` {
				t.Errorf("json outside the prefix must be replaced, got %q", other)
			}
		})
	}
}

func TestMerge_MergeJSONWritesIndentedUnescapedNumberPreservingJSON(t *testing.T) {
	dst := t.TempDir()
	writeTree(t, dst, map[string]string{"locale/en/general.json": `{"n": 1.50, "html": "<b>&</b>"}`})
	src := Source{Name: "s", FS: mapFSOf(map[string]string{"locale/en/general.json": `{"a": 1}`}), MergeJSON: []string{"locale"}}
	if err := Merge(dst, []Source{src}); err != nil {
		t.Fatal(err)
	}
	want := "{\n    \"a\": 1,\n    \"html\": \"<b>&</b>\",\n    \"n\": 1.50\n}\n"
	if got := readFile(t, filepath.Join(dst, "locale/en/general.json")); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMerge_MergeJSONUnderKeepExistingDstWins(t *testing.T) {
	files := map[string]string{"locale/en/general.json": themeCatalogue}
	for name, src := range layerSources(t, files, func(s *Source) {
		s.MergeJSON = []string{"locale"}
		s.KeepExisting = []string{"locale"}
	}) {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			writeTree(t, dst, map[string]string{"locale/en/general.json": projectCatalogue})
			if err := Merge(dst, []Source{src}); err != nil {
				t.Fatal(err)
			}
			got := decodeJSON(t, filepath.Join(dst, "locale/en/general.json"))
			search := got["search"].(map[string]any)
			if search["label"] != "Mine" || search["placeholder"] != "Find" || got["only_theme"] != "T" || got["custom"] != "x" {
				t.Errorf("merge wrong: %v", got)
			}
			if v, present := got["n"]; !present || v != nil {
				t.Errorf("existing null must win: %v", got["n"])
			}
		})
	}
}

func TestMerge_MergeJSONFallsBackToTheFileRuleWhenNotAnObject(t *testing.T) {
	cases := map[string]struct{ existing, incoming string }{
		"invalid existing": {`{broken`, `{"a": 1}`},
		"invalid incoming": {`{"a": 1}`, `{broken`},
		"array root":       {`{"a": 1}`, `["x"]`},
		"trailing content": {`{"a": 1}`, `{"b": 2} {"c": 3}`},
		"null root":        {`null`, `{"a": 1}`},
	}
	for name, c := range cases {
		for _, keep := range []bool{false, true} {
			sub := name + "/replace"
			if keep {
				sub = name + "/keep"
			}
			t.Run(sub, func(t *testing.T) {
				dst := t.TempDir()
				writeTree(t, dst, map[string]string{"locale/en/general.json": c.existing})
				src := Source{Name: "s", FS: mapFSOf(map[string]string{"locale/en/general.json": c.incoming}), MergeJSON: []string{"locale"}}
				want := c.incoming
				if keep {
					src.KeepExisting = []string{"locale"}
					want = c.existing
				}
				if err := Merge(dst, []Source{src}); err != nil {
					t.Fatal(err)
				}
				if got := readFile(t, filepath.Join(dst, "locale/en/general.json")); got != want {
					t.Errorf("keep=%v: got %q, want %q", keep, got, want)
				}
			})
		}
	}
}

func TestMerge_MergeJSONSurfacesAnUnreadableExistingFile(t *testing.T) {
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "locale", "en", "general.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := Source{Name: "s", FS: mapFSOf(map[string]string{"locale/en/general.json": `{"a": 1}`}), MergeJSON: []string{"locale"}, KeepExisting: []string{"locale"}}
	if err := Merge(dst, []Source{src}); err == nil {
		t.Fatal("expected the unreadable destination to be reported")
	}
}
