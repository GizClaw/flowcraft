//go:build windows

package sandbox

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestProcThreadAttributeConstants pins the process-thread attribute
// ids used at spawn time against the authoritative values. A wrong id
// makes UpdateProcThreadAttribute fail with ERROR_NOT_SUPPORTED, which
// only shows up in runtime tests, so these are asserted as early as
// possible (x/sys is generated from Microsoft's Win32 metadata).
func TestProcThreadAttributeConstants(t *testing.T) {
	if procThreadAttributeJobList != 0x0002000d {
		t.Fatalf("procThreadAttributeJobList = %#x, want 0x0002000d", procThreadAttributeJobList)
	}
	if windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST != 0x00020002 {
		t.Fatalf("PROC_THREAD_ATTRIBUTE_HANDLE_LIST = %#x, want 0x00020002", windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST)
	}
	if windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE != 0x00020016 {
		t.Fatalf("PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = %#x, want 0x00020016", windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE)
	}
}
