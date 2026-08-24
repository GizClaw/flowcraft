package windows

import "testing"

func TestParseHelperArgs(t *testing.T) {
	install, err := parseHelperArgs([]string{"install", "--config", `C:\AppData\flowcraft`})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if install.mode != helperModeInstall || install.config != `C:\AppData\flowcraft` {
		t.Fatalf("install = %+v", install)
	}

	serve, err := parseHelperArgs([]string{"serve", "--pipe", `\\.\pipe\x`, "--config", "dir", "--root", `C:\ws`})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if serve.mode != helperModeServe || serve.pipe != `\\.\pipe\x` || serve.config != "dir" || serve.root != `C:\ws` {
		t.Fatalf("serve = %+v", serve)
	}
}

func TestParseHelperArgsRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"bogus", "--config", "x"},
		{"install"},
		{"serve", "--pipe", "p", "--config", "d"},
		{"serve", "--pipe", "p", "--config", "d", "--root", "r", "extra"},
	} {
		if _, err := parseHelperArgs(args); err == nil {
			t.Fatalf("parseHelperArgs(%v) accepted invalid input", args)
		}
	}
}
