//go:build !windows

package tui

import "fmt"

func clipboardShortcutSupported() bool { return false }

func clipboardShortcutDown() bool { return false }

// readClipboardImage is a stub for non-Windows platforms.
func readClipboardImage() (string, error) {
	return "", fmt.Errorf("clipboard image paste not supported on this platform")
}

// readClipboardFiles is a stub for non-Windows platforms.
func readClipboardFiles() ([]string, error) {
	return nil, fmt.Errorf("clipboard file paste not supported on this platform")
}

// readClipboardText is a stub for non-Windows platforms. On Unix the
// terminal's bracketed-paste mode already delivers a paste as one atomic
// KeyMsg, so no direct clipboard read is needed.
func readClipboardText() (string, error) {
	return "", fmt.Errorf("clipboard text paste not supported on this platform")
}

// writeClipboardText is a stub for non-Windows platforms.
func writeClipboardText(text string) error {
	return fmt.Errorf("clipboard text copy not supported on this platform")
}
