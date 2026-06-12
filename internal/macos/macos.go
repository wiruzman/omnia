package macos

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

func OpenPath(path string) error {
	cmd := exec.Command("open", path)
	return cmd.Run()
}

func RevealInFinder(path string) error {
	cmd := exec.Command("open", "-R", path)
	return cmd.Run()
}

func CopyToClipboard(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = bytes.NewBufferString(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pbcopy failed: %w", err)
	}
	return nil
}

func MoveToTrash(path string) error {
	escaped := strings.ReplaceAll(path, "\"", "\\\"")
	script := fmt.Sprintf("tell application \"Finder\" to delete POSIX file \"%s\"", escaped)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("move to trash failed: %w", err)
	}
	return nil
}
