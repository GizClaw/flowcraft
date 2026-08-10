package app

import (
	"testing"
)

func TestDecodeSpeakers(t *testing.T) {
	raw := []byte("narrator: 旁白\nrole_wukong: 悟空\n")
	speakers, err := decodeSpeakers(raw)
	if err != nil {
		t.Fatalf("decodeSpeakers: %v", err)
	}
	if speakers["narrator"] != "旁白" || speakers["role_wukong"] != "悟空" {
		t.Fatalf("speakers = %v", speakers)
	}
}

func TestDecodeInferenceCredentialsIgnoresProviderFields(t *testing.T) {
	raw := []byte(`
version: v1
providers:
  - id: deepseek
    driver: deepseek
    profiles:
      - secrets:
          api_key: {resolver: env, key: DEEPSEEK_API_KEY}
`)
	doc, err := decodeInferenceCredentials(raw)
	if err != nil {
		t.Fatalf("decodeInferenceCredentials: %v", err)
	}
	if len(doc.Providers) != 1 || len(doc.Providers[0].Profiles) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	secret := doc.Providers[0].Profiles[0].Secrets["api_key"]
	if secret.Resolver != "env" || secret.Key != "DEEPSEEK_API_KEY" {
		t.Fatalf("secret = %+v", secret)
	}
}
