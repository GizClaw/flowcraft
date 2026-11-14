package script

import "testing"

// FuzzDecodeConfig checks the one property a decoder must never lose: an accepted
// config is one the rest of the package can act on. A value that decodes but then
// fails the allow or version parser would be a configuration the node half
// accepts, which is worse than a rejection.
func FuzzDecodeConfig(f *testing.F) {
	f.Add([]byte(`{"backend":"fake","path":"x","reads":["a"],"writes":["b"]}`))
	f.Add([]byte(`{"backend":"fake","path":"x","allow":["tool:t"],"budget":{"max_steps":1}}`))
	f.Add([]byte(`{"backend":"fake","path":"x","version":"pinned","max_output_bytes":10}`))
	f.Add([]byte(`{"backend":""}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"backend":"fake","path":"x","reads":["a","a"]}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		cfg, err := decodeConfig(raw)
		if err != nil {
			return
		}
		if _, err := toolNames(cfg.Allow); err != nil {
			t.Fatalf("accepted a config whose allow list does not parse: %v", err)
		}
		if err := checkVersion(cfg.Version); err != nil {
			t.Fatalf("accepted a config whose version does not parse: %v", err)
		}
		if cfg.Backend == "" || cfg.Path == "" {
			t.Fatalf("accepted a config without a backend or a path: %+v", cfg)
		}
	})
}
