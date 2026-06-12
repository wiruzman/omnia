package model

import "time"

type FileType string

const (
	TypeFile      FileType = "file"
	TypeDirectory FileType = "directory"
	TypeSymlink   FileType = "symlink"
)

type Entry struct {
	Path       string
	Name       string
	ParentPath string
	RootPath   string
	Type       FileType
	Size       int64
	CreatedAt  time.Time
	ModifiedAt time.Time
}
