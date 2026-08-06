package config

import (
	"testing"
	"time"
)

func TestDecodeSettingsYAMLAndJSON(t *testing.T) {
	for name, data := range map[string]string{
		"yaml": `generate: {provider: fake, name: echo}
embed: {provider: fake, name: echo}
interval: 1h
lifecycle:
  interval: 2h
  lease_ttl: 5m
  decay:
    half_life: 720h
`,
		"json": `{
			"generate": {"provider": "fake", "name": "echo"},
			"embed": {"provider": "fake", "name": "echo"},
			"interval": "1h",
			"lifecycle": {
				"interval": "2h",
				"lease_ttl": "5m",
				"decay": {"half_life": "720h"}
			}
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			settings, err := decodeSettings([]byte(data))
			if err != nil {
				t.Fatalf("decodeSettings: %v", err)
			}
			if settings.Interval != Duration(time.Hour) {
				t.Fatalf("interval = %v, want 1h", time.Duration(settings.Interval))
			}
			if settings.Lifecycle.Interval != Duration(2*time.Hour) {
				t.Fatalf("lifecycle interval = %v, want 2h",
					time.Duration(settings.Lifecycle.Interval))
			}
			if settings.Lifecycle.LeaseTTL != Duration(5*time.Minute) {
				t.Fatalf("lease_ttl = %v, want 5m",
					time.Duration(settings.Lifecycle.LeaseTTL))
			}
			if settings.Lifecycle.Decay.HalfLife != 30*24*time.Hour {
				t.Fatalf("decay half_life = %v, want 720h",
					settings.Lifecycle.Decay.HalfLife)
			}
		})
	}
}

func TestDecodeSettingsRejectsUnknownFieldsAndNumericDurations(t *testing.T) {
	if _, err := decodeSettings([]byte(
		`{"generate":{"provider":"fake","name":"echo"},"bogus":1}`,
	)); err == nil {
		t.Fatal("decodeSettings accepted an unknown field")
	}
	if _, err := decodeSettings([]byte(
		`{"generate":{"provider":"fake","name":"echo"},"interval":30}`,
	)); err == nil {
		t.Fatal("decodeSettings accepted a numeric duration")
	}
	if _, err := decodeSettings([]byte(
		`{"generate":{"provider":"fake","name":"echo"},"lifecycle":{"decay":{"bogus":1}}}`,
	)); err == nil {
		t.Fatal("decodeSettings accepted an unknown lifecycle field")
	}
}
