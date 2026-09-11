package model

import "testing"

func TestModelIDValidate(t *testing.T) {
	cases := []struct {
		name string
		id   ModelID
		want string
	}{
		{name: "provider missing", id: ModelID{Name: "m"}, want: "model provider is required"},
		{name: "name missing", id: ModelID{Provider: "p"}, want: "model name is required"},
		{name: "complete", id: ModelID{Provider: "p", Name: "m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.id.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestModelRefValidate pins that a ref is only as valid as the identity it
// addresses; the credential profile is not part of validation.
func TestModelRefValidate(t *testing.T) {
	if err := (ModelRef{ID: ModelID{Provider: "p", Name: "m"}}).Validate(); err != nil {
		t.Fatalf("complete ref: %v", err)
	}
	if err := (ModelRef{Profile: "secondary"}).Validate(); err == nil {
		t.Fatal("ref without identity must not validate")
	}
}
