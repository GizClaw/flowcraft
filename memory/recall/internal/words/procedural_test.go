package words

import "testing"

func TestLooksProcedural(t *testing.T) {
	cases := []string{
		"When comparing options, use a markdown table.",
		"Before processing invoices, run OCR and then extract entities.",
		"Always check cache before calling the API.",
		"Prefer markdown output for answers.",
		"Cuando compares opciones, usa una tabla.",
		"Quand tu compares, utilise un tableau.",
		"Wenn du Optionen vergleichst, verwende eine Tabelle.",
		"Quando comparar opções, use uma tabela.",
		"Wanneer je opties vergelijkt, gebruik een tabel.",
		"Когда сравниваешь варианты, используй таблицу.",
		"当比较选项时，使用表格。",
	}
	for _, c := range cases {
		if !LooksProcedural(c) {
			t.Fatalf("expected procedural cue for %q", c)
		}
	}
	if LooksProcedural("Alice prefers tea in the morning.") {
		t.Fatal("simple preference text should not look procedural")
	}
}
