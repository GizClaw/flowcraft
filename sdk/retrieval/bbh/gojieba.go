package bbh

import (
	"fmt"
	"runtime"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/registry"
	"github.com/yanyiwu/gojieba"
)

const (
	bleveAnalyzerGojieba        = "gojieba"
	bleveGojiebaTokenizer       = "sdk_bbh_gojieba"
	bleveGojiebaTokenizerConfig = "bbh_gojieba_tokenizer"
)

type gojiebaTokenizer struct {
	jieba *gojieba.Jieba
	mode  string
	hmm   bool
}

func newGojiebaTokenizer(cfg GojiebaConfig) (*gojiebaTokenizer, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = "search"
	}
	if mode != "search" && mode != "default" && mode != "all" {
		return nil, fmt.Errorf("retrieval/bbh: unsupported gojieba mode %q", mode)
	}
	hmm := true
	if cfg.HMM != nil {
		hmm = *cfg.HMM
	}
	var jieba *gojieba.Jieba
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		jieba = gojieba.NewJieba(gojiebaDictArgs(cfg)...)
	}()
	if panicValue != nil {
		return nil, fmt.Errorf("retrieval/bbh: open gojieba tokenizer: %v", panicValue)
	}
	tok := &gojiebaTokenizer{jieba: jieba, mode: mode, hmm: hmm}
	runtime.SetFinalizer(tok, func(t *gojiebaTokenizer) {
		if t.jieba != nil {
			t.jieba.Free()
		}
	})
	return tok, nil
}

func (t *gojiebaTokenizer) Tokenize(input []byte) analysis.TokenStream {
	text := string(input)
	var words []gojieba.Word
	switch t.mode {
	case "all":
		parts := t.jieba.CutAll(text)
		words = makeWordsBySearch(text, parts)
	case "default":
		words = t.jieba.Tokenize(text, gojieba.DefaultMode, t.hmm)
	default:
		words = t.jieba.Tokenize(text, gojieba.SearchMode, t.hmm)
	}
	out := make(analysis.TokenStream, 0, len(words))
	pos := 1
	for _, w := range words {
		if w.Str == "" {
			continue
		}
		out = append(out, &analysis.Token{
			Term:     []byte(w.Str),
			Start:    w.Start,
			End:      w.End,
			Position: pos,
			Type:     analysis.AlphaNumeric,
		})
		pos++
	}
	return out
}

func makeWordsBySearch(text string, parts []string) []gojieba.Word {
	out := make([]gojieba.Word, 0, len(parts))
	byteStart := 0
	for _, part := range parts {
		if part == "" {
			continue
		}
		offset := byteIndexFrom(text, part, byteStart)
		if offset < 0 {
			offset = byteStart
		}
		end := offset + len([]byte(part))
		out = append(out, gojieba.Word{Str: part, Start: offset, End: end})
		byteStart = end
	}
	return out
}

func byteIndexFrom(s, substr string, start int) int {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		return -1
	}
	for i := start; i < len(s); {
		if len(s)-i >= len(substr) && s[i:i+len(substr)] == substr {
			return i
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		if n == 0 {
			break
		}
		i += n
	}
	return -1
}

func gojiebaTokenizerConstructor(config map[string]interface{}, _ *registry.Cache) (analysis.Tokenizer, error) {
	cfg := GojiebaConfig{
		Mode: stringFromConfig(config, "mode"),
	}
	if hmm, ok := boolFromConfig(config, "hmm"); ok {
		cfg.HMM = &hmm
	}
	cfg.DictPath = stringFromConfig(config, "dict_path")
	cfg.HMMPath = stringFromConfig(config, "hmm_path")
	cfg.UserDictPath = stringFromConfig(config, "user_dict_path")
	cfg.IDFPath = stringFromConfig(config, "idf_path")
	cfg.StopWordsPath = stringFromConfig(config, "stop_words_path")
	return newGojiebaTokenizer(cfg)
}

func configureGojiebaAnalyzer(m *mapping.IndexMappingImpl, cfg GojiebaConfig) error {
	tokenizerConfig := map[string]interface{}{
		"type":            bleveGojiebaTokenizer,
		"mode":            cfg.Mode,
		"dict_path":       cfg.DictPath,
		"hmm_path":        cfg.HMMPath,
		"user_dict_path":  cfg.UserDictPath,
		"idf_path":        cfg.IDFPath,
		"stop_words_path": cfg.StopWordsPath,
	}
	if cfg.HMM != nil {
		tokenizerConfig["hmm"] = *cfg.HMM
	}
	if err := m.AddCustomTokenizer(bleveGojiebaTokenizerConfig, tokenizerConfig); err != nil {
		return err
	}
	return m.AddCustomAnalyzer(bleveAnalyzerGojieba, map[string]interface{}{
		"type":          custom.Name,
		"tokenizer":     bleveGojiebaTokenizerConfig,
		"token_filters": []string{lowercase.Name},
	})
}

func stringFromConfig(config map[string]interface{}, key string) string {
	v, _ := config[key].(string)
	return v
}

func boolFromConfig(config map[string]interface{}, key string) (bool, bool) {
	v, ok := config[key].(bool)
	return v, ok
}

func gojiebaDictArgs(cfg GojiebaConfig) []string {
	if cfg.DictPath == "" &&
		cfg.HMMPath == "" &&
		cfg.UserDictPath == "" &&
		cfg.IDFPath == "" &&
		cfg.StopWordsPath == "" {
		return nil
	}
	return []string{
		cfg.DictPath,
		cfg.HMMPath,
		cfg.UserDictPath,
		cfg.IDFPath,
		cfg.StopWordsPath,
	}
}

func init() {
	if err := registry.RegisterTokenizer(bleveGojiebaTokenizer, gojiebaTokenizerConstructor); err != nil {
		panic(err)
	}
}
