package bbh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"gopkg.in/yaml.v3"
)

func TestDurationUnmarshalJSONAndYAML(t *testing.T) {
	var fromString Duration
	if err := json.Unmarshal([]byte(`"250ms"`), &fromString); err != nil {
		t.Fatal(err)
	}
	if fromString.Duration != 250*time.Millisecond {
		t.Fatalf("json string duration = %s", fromString.Duration)
	}
	var fromNumber Duration
	if err := json.Unmarshal([]byte(`1000`), &fromNumber); err != nil {
		t.Fatal(err)
	}
	if fromNumber.Duration != time.Microsecond {
		t.Fatalf("json numeric duration = %s", fromNumber.Duration)
	}
	var fromYAML struct {
		D Duration `yaml:"d"`
	}
	if err := yaml.Unmarshal([]byte("d: 2s\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.D.Duration != 2*time.Second {
		t.Fatalf("yaml string duration = %s", fromYAML.D.Duration)
	}
	if err := yaml.Unmarshal([]byte("d:\n  - bad\n"), &fromYAML); err == nil {
		t.Fatal("yaml sequence duration should fail")
	}
	if err := yaml.Unmarshal([]byte("d: 1000\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.D.Duration != time.Microsecond {
		t.Fatalf("yaml numeric duration = %s", fromYAML.D.Duration)
	}
	if err := yaml.Unmarshal([]byte("d: true\n"), &fromYAML); err == nil {
		t.Fatal("yaml bool duration should fail")
	}
	if err := json.Unmarshal([]byte(`"bad"`), &fromString); err == nil {
		t.Fatal("bad json duration should fail")
	}
	if err := json.Unmarshal([]byte(`{}`), &fromString); err == nil {
		t.Fatal("bad json duration type should fail")
	}
}

func TestConfigLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadConfigFile(dir, "missing.yaml"); err == nil {
		t.Fatal("missing config should fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(dir, "bad.json"); err == nil {
		t.Fatal("bad json should fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("x: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(dir, "bad.yaml"); err == nil {
		t.Fatal("bad yaml should fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.toml"), []byte("x=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(dir, "bad.toml"); err == nil {
		t.Fatal("unsupported config extension should fail")
	}
	fileRoot := filepath.Join(dir, "file-root")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discoverConfig(fileRoot); err == nil {
		t.Fatal("discover under file root should fail")
	}
	if _, err := resolveConfig(dir, []Option{WithConfigFilePath("missing.yaml")}); err == nil {
		t.Fatal("resolve config with missing explicit path should fail")
	}
	badDiscover := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDiscover, "config.yaml"), []byte("hnsw:\n  flush_interval: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveConfig(badDiscover, nil); err == nil {
		t.Fatal("discovered bad config should fail")
	}
}

func TestResolveConfigUsesConfigFields(t *testing.T) {
	cfg, err := resolveConfig(t.TempDir(), []Option{WithConfig(Config{
		SearchOverfetch: 7,
		Bleve:           BleveConfig{Analyzer: "keyword"},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchOverfetch != 7 {
		t.Fatalf("SearchOverfetch=%d, want 7", cfg.SearchOverfetch)
	}
	if cfg.Bleve.Analyzer != "keyword" {
		t.Fatalf("Bleve.Analyzer=%q, want keyword", cfg.Bleve.Analyzer)
	}
}

func TestResolveConfigWithConfigSkipsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("search_overfetch: 99\nbleve:\n  analyzer: keyword\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveConfig(dir, []Option{WithConfig(Config{
		SearchOverfetch: 7,
		Bleve:           BleveConfig{Analyzer: "standard"},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SearchOverfetch != 7 || cfg.Bleve.Analyzer != "standard" {
		t.Fatalf("WithConfig should skip file, got %+v", cfg)
	}
}

func TestResolveConfigDefaultsGojieba(t *testing.T) {
	cfg, err := resolveConfig(t.TempDir(), []Option{WithConfig(Config{
		Bleve: BleveConfig{Analyzer: "gojieba"},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bleve.Gojieba.Mode != "search" {
		t.Fatalf("Gojieba.Mode=%q, want search", cfg.Bleve.Gojieba.Mode)
	}
}

func TestResolveConfigResolvesGojiebaNamedDictPaths(t *testing.T) {
	dir := t.TempDir()
	cfg, err := resolveConfig(dir, []Option{WithConfig(Config{
		Bleve: BleveConfig{
			Analyzer: "gojieba",
			Gojieba: GojiebaConfig{
				UserDictPath:  "dict/user.dict.utf8",
				StopWordsPath: "dict/stop_words.utf8",
			},
		},
	})})
	if err != nil {
		t.Fatal(err)
	}
	wantUser := filepath.Join(dir, "dict/user.dict.utf8")
	wantStop := filepath.Join(dir, "dict/stop_words.utf8")
	if cfg.Bleve.Gojieba.UserDictPath != wantUser {
		t.Fatalf("UserDictPath=%q, want %q", cfg.Bleve.Gojieba.UserDictPath, wantUser)
	}
	if cfg.Bleve.Gojieba.StopWordsPath != wantStop {
		t.Fatalf("StopWordsPath=%q, want %q", cfg.Bleve.Gojieba.StopWordsPath, wantStop)
	}
}

func TestConfigAutoLoadsYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("bleve:\n  analyzer: keyword\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := openInternalIndex(t, dir)
	assertKeywordAnalyzer(t, idx)
}

func TestWithConfigSkipsWorkspaceConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("bleve:\n  analyzer: keyword\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := openInternalIndex(t, dir, WithConfig(Config{
		Bleve: BleveConfig{Analyzer: "standard"},
	}))
	assertStandardAnalyzer(t, idx)
}

func TestWithConfigFilePathLoadsJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ignored.yaml"), []byte("bleve:\n  analyzer: standard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bbh.json"), []byte(`{"bleve":{"analyzer":"keyword"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := openInternalIndex(t, dir, WithConfigFilePath("bbh.json"))
	assertKeywordAnalyzer(t, idx)
}

func assertKeywordAnalyzer(t *testing.T, idx retrieval.Index) {
	t.Helper()
	indexAnalyzerFixture(t, idx)
	assertSearchCount(t, idx, "alpha", 0)
	assertSearchCount(t, idx, "alpha beta", 1)
}

func assertStandardAnalyzer(t *testing.T, idx retrieval.Index) {
	t.Helper()
	indexAnalyzerFixture(t, idx)
	assertSearchCount(t, idx, "alpha", 1)
}

func indexAnalyzerFixture(t *testing.T, idx retrieval.Index) {
	t.Helper()
	err := idx.Upsert(context.Background(), "ns", []retrieval.Doc{{
		ID:        "doc",
		Content:   "alpha beta",
		Timestamp: time.Unix(1_700_000_000, 0).UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func assertSearchCount(t *testing.T, idx retrieval.Index, query string, want int) {
	t.Helper()
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: query,
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != want {
		t.Fatalf("query %q: got %d hits, want %d (%+v)", query, len(resp.Hits), want, resp.Hits)
	}
}
