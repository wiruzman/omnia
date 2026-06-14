package app

import "github.com/wiruzman/omnia/internal/macos"

type SystemAdapter interface {
	OpenPath(path string) error
	RevealInFinder(path string) error
	CopyToClipboard(text string) error
	MoveToTrash(path string) error
}

type macOSAdapter struct{}

func NewMacOSAdapter() SystemAdapter {
	return macOSAdapter{}
}

func (macOSAdapter) OpenPath(path string) error {
	return macos.OpenPath(path)
}

func (macOSAdapter) RevealInFinder(path string) error {
	return macos.RevealInFinder(path)
}

func (macOSAdapter) CopyToClipboard(text string) error {
	return macos.CopyToClipboard(text)
}

func (macOSAdapter) MoveToTrash(path string) error {
	return macos.MoveToTrash(path)
}
