package bbh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultSearchOverfetch = 20
	defaultBleveAnalyzer   = "standard"
	defaultHNSWFlushEvery  = 60 * time.Second
)

// Config is the serializable BBH configuration. It can be supplied directly
// via WithConfig, loaded from an explicit WithConfigFilePath, or discovered as
// config.yaml/config.yml/config.json in the workspace root.
type Config struct {
	SearchOverfetch int         `json:"search_overfetch,omitempty" yaml:"search_overfetch,omitempty"`
	Bleve           BleveConfig `json:"bleve,omitempty" yaml:"bleve,omitempty"`
	HNSW            HNSWConfig  `json:"hnsw,omitempty" yaml:"hnsw,omitempty"`
}

// BleveConfig configures newly-created Bleve indexes. Existing index
// directories keep their persisted mapping and are opened as-is by Bleve.
type BleveConfig struct {
	Analyzer string        `json:"analyzer,omitempty" yaml:"analyzer,omitempty"`
	Gojieba  GojiebaConfig `json:"gojieba,omitempty" yaml:"gojieba,omitempty"`
}

// GojiebaConfig configures BBH's built-in gojieba analyzer. It is used when
// Bleve.Analyzer is "gojieba".
type GojiebaConfig struct {
	Mode          string `json:"mode,omitempty" yaml:"mode,omitempty"`
	HMM           *bool  `json:"hmm,omitempty" yaml:"hmm,omitempty"`
	DictPath      string `json:"dict_path,omitempty" yaml:"dict_path,omitempty"`
	HMMPath       string `json:"hmm_path,omitempty" yaml:"hmm_path,omitempty"`
	UserDictPath  string `json:"user_dict_path,omitempty" yaml:"user_dict_path,omitempty"`
	IDFPath       string `json:"idf_path,omitempty" yaml:"idf_path,omitempty"`
	StopWordsPath string `json:"stop_words_path,omitempty" yaml:"stop_words_path,omitempty"`
}

// HNSWConfig controls persistence for per-namespace HNSW graph checkpoints.
// Writes update the live graph immediately and mark it dirty; the checkpoint is
// saved by the shard flush loop and again during Close.
type HNSWConfig struct {
	FlushInterval Duration `json:"flush_interval,omitempty" yaml:"flush_interval,omitempty"`
}

// Duration is a config-friendly time.Duration wrapper that accepts strings
// such as "5s" as well as numeric nanoseconds.
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parsed, perr := time.ParseDuration(s)
		if perr != nil {
			return perr
		}
		d.Duration = parsed
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	d.Duration = time.Duration(n)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!str" {
			parsed, err := time.ParseDuration(value.Value)
			if err != nil {
				return err
			}
			d.Duration = parsed
			return nil
		}
		var n int64
		if err := value.Decode(&n); err != nil {
			return err
		}
		d.Duration = time.Duration(n)
		return nil
	default:
		return fmt.Errorf("duration must be a string or integer")
	}
}

type optionState struct {
	config         *Config
	configFilePath string
}

// Option configures an Index.
type Option func(*optionState)

// WithConfig supplies the full BBH configuration and skips workspace config
// discovery. Zero-valued fields still receive BBH defaults.
func WithConfig(cfg Config) Option {
	return func(o *optionState) {
		copied := cfg
		o.config = &copied
	}
}

// WithConfigFilePath loads BBH configuration from path. Relative paths are
// resolved against the workspace root.
func WithConfigFilePath(path string) Option {
	return func(o *optionState) {
		o.configFilePath = strings.TrimSpace(path)
	}
}

func resolveConfig(root string, opts []Option) (Config, error) {
	state := optionState{}
	for _, opt := range opts {
		if opt != nil {
			opt(&state)
		}
	}

	cfg := defaultConfig()
	switch {
	case state.config != nil:
		cfg = *state.config
	case state.configFilePath != "":
		loaded, err := loadConfigFile(root, state.configFilePath)
		if err != nil {
			return Config{}, err
		}
		cfg = loaded
	default:
		loaded, found, err := discoverConfig(root)
		if err != nil {
			return Config{}, err
		}
		if found {
			cfg = loaded
		}
	}
	cfg.applyDefaults()
	cfg.resolvePaths(root)
	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		SearchOverfetch: defaultSearchOverfetch,
		Bleve: BleveConfig{
			Analyzer: defaultBleveAnalyzer,
		},
		HNSW: HNSWConfig{
			FlushInterval: Duration{Duration: defaultHNSWFlushEvery},
		},
	}
}

func (c *Config) applyDefaults() {
	if c.SearchOverfetch <= 0 {
		c.SearchOverfetch = defaultSearchOverfetch
	}
	if strings.TrimSpace(c.Bleve.Analyzer) == "" {
		c.Bleve.Analyzer = defaultBleveAnalyzer
	} else {
		c.Bleve.Analyzer = strings.TrimSpace(c.Bleve.Analyzer)
	}
	if strings.TrimSpace(c.Bleve.Gojieba.Mode) == "" {
		c.Bleve.Gojieba.Mode = "search"
	} else {
		c.Bleve.Gojieba.Mode = strings.ToLower(strings.TrimSpace(c.Bleve.Gojieba.Mode))
	}
	if c.HNSW.FlushInterval.Duration <= 0 {
		c.HNSW.FlushInterval.Duration = defaultHNSWFlushEvery
	}
	c.Bleve.Gojieba.DictPath = strings.TrimSpace(c.Bleve.Gojieba.DictPath)
	c.Bleve.Gojieba.HMMPath = strings.TrimSpace(c.Bleve.Gojieba.HMMPath)
	c.Bleve.Gojieba.UserDictPath = strings.TrimSpace(c.Bleve.Gojieba.UserDictPath)
	c.Bleve.Gojieba.IDFPath = strings.TrimSpace(c.Bleve.Gojieba.IDFPath)
	c.Bleve.Gojieba.StopWordsPath = strings.TrimSpace(c.Bleve.Gojieba.StopWordsPath)
}

func (c *Config) resolvePaths(root string) {
	c.Bleve.Gojieba.DictPath = resolveOptionalPath(root, c.Bleve.Gojieba.DictPath)
	c.Bleve.Gojieba.HMMPath = resolveOptionalPath(root, c.Bleve.Gojieba.HMMPath)
	c.Bleve.Gojieba.UserDictPath = resolveOptionalPath(root, c.Bleve.Gojieba.UserDictPath)
	c.Bleve.Gojieba.IDFPath = resolveOptionalPath(root, c.Bleve.Gojieba.IDFPath)
	c.Bleve.Gojieba.StopWordsPath = resolveOptionalPath(root, c.Bleve.Gojieba.StopWordsPath)
}

func resolveOptionalPath(root, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

func discoverConfig(root string) (Config, bool, error) {
	for _, name := range []string{"config.yaml", "config.yml", "config.json"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Config{}, false, fmt.Errorf("retrieval/bbh: stat config %s: %w", path, err)
		}
		cfg, err := loadConfigFile(root, path)
		if err != nil {
			return Config{}, false, err
		}
		return cfg, true, nil
	}
	return Config{}, false, nil
}

func loadConfigFile(root, path string) (Config, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("retrieval/bbh: read config %s: %w", path, err)
	}
	var cfg Config
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("retrieval/bbh: parse config %s: %w", path, err)
		}
	case ".yaml", ".yml", "":
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("retrieval/bbh: parse config %s: %w", path, err)
		}
	default:
		return Config{}, fmt.Errorf("retrieval/bbh: unsupported config extension %q", filepath.Ext(path))
	}
	return cfg, nil
}
