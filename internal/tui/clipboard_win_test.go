//go:build windows

package tui

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// The Ctrl+V paste poll uses GetAsyncKeyState, which reads physical key state
// process-wide. terminalHasFocus is the only thing stopping a Ctrl+V pressed in
// another application from pasting into the TUI, so these tests pin down the
// membership rules it depends on.

func TestOwnPIDsIncludesSelf(t *testing.T) {
	if self := uint32(os.Getpid()); !ownPIDs()[self] {
		t.Fatalf("ownPIDs() must contain our own pid %d", self)
	}
}

// The ancestor walk climbs through the terminal host toward the desktop shell.
// It must stop before explorer.exe: otherwise every File Explorer window and
// the desktop itself would count as "our" terminal and Ctrl+V there would paste.
func TestOwnPIDsExcludesDesktopShell(t *testing.T) {
	names := processNames(t)
	for pid := range ownPIDs() {
		if ancestorStops[names[pid]] {
			t.Errorf("ownPIDs() contains shell process %q (pid %d); "+
				"Ctrl+V in that window would paste into the TUI", names[pid], pid)
		}
	}
}

func TestAncestorStopsCoversExplorer(t *testing.T) {
	for _, want := range []string{"explorer.exe", "winlogon.exe", "services.exe"} {
		if !ancestorStops[want] {
			t.Errorf("ancestorStops missing %q", want)
		}
	}
	if ancestorStops["windowsterminal.exe"] || ancestorStops["conhost.exe"] {
		t.Error("terminal hosts must NOT be stops — they own our visible window")
	}
}

// A bounded walk keeps a PID-reuse cycle from hanging startup.
func TestAncestorWalkIsBounded(t *testing.T) {
	if got := len(ownPIDs()); got > maxAncestorWalk+1 {
		t.Fatalf("ownPIDs() returned %d pids, want <= %d", got, maxAncestorWalk+1)
	}
}

// ownsWindow drives both outcomes without stealing real focus.
func TestOwnsWindow(t *testing.T) {
	const (
		ourConsole = uintptr(0x1000)
		foreign    = uintptr(0x2000)
	)
	self := uint32(os.Getpid())
	// A pid that is certainly not ours: pick one absent from the snapshot.
	other := uint32(0x7FFFFFF0)
	for ownPIDs()[other] {
		other--
	}

	for _, tc := range []struct {
		name    string
		hwnd    uintptr
		console uintptr
		pid     uint32
		want    bool
	}{
		{"no foreground window", 0, ourConsole, self, false},
		{"our console is foreground", ourConsole, ourConsole, other, true},
		{"foreign window owned by us (terminal host)", foreign, ourConsole, self, true},
		{"foreign window, foreign pid", foreign, ourConsole, other, false},
		{"foreign window, zero pid", foreign, ourConsole, 0, false},
		{"no console, our pid", foreign, 0, self, true},
		{"no console, foreign pid", foreign, 0, other, false},
	} {
		if got := ownsWindow(tc.hwnd, tc.console, tc.pid); got != tc.want {
			t.Errorf("%s: ownsWindow(%#x, %#x, %d) = %v, want %v",
				tc.name, tc.hwnd, tc.console, tc.pid, got, tc.want)
		}
	}
}

// processNames maps pid -> lowercased image name via the same toolhelp
// snapshot ownPIDs uses.
func processNames(t *testing.T) map[uint32]string {
	t.Helper()
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == invalidHandle {
		t.Skip("cannot snapshot processes")
	}
	defer procCloseHandle.Call(snap)

	out := map[uint32]string{}
	var e processEntry32
	e.size = uint32(unsafe.Sizeof(e))
	ok, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e)))
	for ok != 0 {
		out[e.processID] = strings.ToLower(syscall.UTF16ToString(e.exeFile[:]))
		ok, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e)))
	}
	return out
}

// Text clipboard round-trip: writeClipboardText must store text that
// readClipboardText returns byte-identical (CRLF normalized), so Ctrl+C/Ctrl+V
// in rick behaves like a normal terminal.
func TestClipboardTextRoundTrip(t *testing.T) {
	want := "line one\nline two\nthird"
	if err := writeClipboardText(want); err != nil {
		t.Skipf("clipboard unavailable in this session: %v", err)
	}
	got, err := readClipboardText()
	if err != nil {
		t.Fatalf("readClipboardText: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}
