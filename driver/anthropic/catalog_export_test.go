package anthropic

import "testing"

func TestCatalogExportsBuiltinModels(t *testing.T) {
	const provider = "anthropic"
	descriptors, err := Catalog(provider)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(descriptors) != len(catalog) {
		t.Fatalf("Catalog returned %d descriptors, want %d",
			len(descriptors), len(catalog))
	}
	for i, descriptor := range descriptors {
		if descriptor.ID.Provider != provider {
			t.Errorf("descriptor %d provider = %q, want %q",
				i, descriptor.ID.Provider, provider)
		}
		if i > 0 && descriptors[i-1].ID.Name >= descriptor.ID.Name {
			t.Errorf("descriptors not sorted at %d (%q after %q)",
				i, descriptor.ID.Name, descriptors[i-1].ID.Name)
		}
	}
	if _, err := Catalog(""); err == nil {
		t.Fatal(`Catalog("") succeeded, want error`)
	}
}
