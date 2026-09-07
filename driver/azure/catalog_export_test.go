package azure

import "testing"

func TestCatalogHasNoBuiltinModels(t *testing.T) {
	descriptors, err := Catalog("azure")
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(descriptors) != 0 {
		t.Fatalf("Catalog returned %d descriptors, want none",
			len(descriptors))
	}
	if _, err := Catalog(""); err == nil {
		t.Fatal(`Catalog("") succeeded, want error`)
	}
}
