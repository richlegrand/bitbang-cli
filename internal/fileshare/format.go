package fileshare

import (
	"fmt"
	"strings"
)

// FormatSize is a port of Python's format_size (core.py:9-15). Renders a
// byte count with binary-multiple unit suffixes.
func FormatSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	f := float64(size)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", f/1024)
}

// FileIcon returns an emoji for the file type based on extension.
// Mirrors Python's get_file_icon (core.py:18-33).
func FileIcon(filename string) string {
	idx := strings.LastIndex(filename, ".")
	if idx < 0 {
		return "\U0001F4C4" // default: page
	}
	ext := strings.ToLower(filename[idx+1:])
	switch ext {
	case "pdf":
		return "\U0001F4D5"
	case "doc", "docx":
		return "\U0001F4D8"
	case "xls", "xlsx":
		return "\U0001F4D7"
	case "ppt", "pptx":
		return "\U0001F4D9"
	case "zip", "tar", "gz", "rar", "7z":
		return "\U0001F4E6"
	case "jpg", "jpeg", "png", "gif", "webp":
		return "\U0001F5BC️"
	case "mp4", "mov", "avi", "mkv", "webm":
		return "\U0001F3AC"
	case "mp3", "wav", "flac", "ogg":
		return "\U0001F3B5"
	case "py":
		return "\U0001F40D"
	case "js", "ts":
		return "\U0001F4DC"
	case "html":
		return "\U0001F310"
	case "css":
		return "\U0001F3A8"
	case "txt":
		return "\U0001F4C4"
	case "md":
		return "\U0001F4DD"
	default:
		return "\U0001F4C4"
	}
}
