package main

import "testing"

func TestParseCommand(t *testing.T) {
	if got := parseCommand("/start@demo_bot hello"); got != "/start" {
		t.Fatalf("expected /start, got %q", got)
	}
	if got := parseCommand("hello"); got != "" {
		t.Fatalf("expected empty command, got %q", got)
	}
}

func TestParseCommandArgs(t *testing.T) {
	if got := parseCommandArgs("/mkdir /images/2024/01/"); got != "/images/2024/01/" {
		t.Fatalf("unexpected command args: %q", got)
	}
	if got := parseCommandArgs("/mkdir"); got != "" {
		t.Fatalf("expected empty args, got %q", got)
	}
}

func TestSanitizeSubPath(t *testing.T) {
	got, err := sanitizeSubPath(" album/2026 /raw/ ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "album/2026/raw" {
		t.Fatalf("unexpected sanitized path: %q", got)
	}
}

func TestSanitizeSubPathRejectsTraversal(t *testing.T) {
	if _, err := sanitizeSubPath("../secret/"); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestSanitizeFileName(t *testing.T) {
	got := sanitizeFileName("../../my file?.jpg", "fallback.jpg")
	if got != "my file_.jpg" {
		t.Fatalf("unexpected sanitized file name: %q", got)
	}
}

func TestSanitizeFileNamePreservesUnicode(t *testing.T) {
	got := sanitizeFileName("../../中文 文件#1?.jpg", "fallback.jpg")
	if got != "中文 文件_1_.jpg" {
		t.Fatalf("unexpected unicode file name: %q", got)
	}
}

func TestEncodePathSegments(t *testing.T) {
	got := encodePathSegments("images/2024/01", "中文 文件#1?.jpg")
	if got != "images/2024/01/%E4%B8%AD%E6%96%87%20%E6%96%87%E4%BB%B6%231%3F.jpg" {
		t.Fatalf("unexpected encoded path: %q", got)
	}
}

func TestResolveFolderPathInputRelative(t *testing.T) {
	got, err := resolveFolderPathInput("2024/01/", "images", []string{"images", "videos", "documents", "others"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "images/2024/01" {
		t.Fatalf("unexpected resolved path: %q", got)
	}
}

func TestResolveFolderPathInputAbsolute(t *testing.T) {
	got, err := resolveFolderPathInput("/images/2024/01/", "others", []string{"images", "videos", "documents", "others"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "images/2024/01" {
		t.Fatalf("unexpected resolved path: %q", got)
	}
}

func TestResolveFolderPathInputAbsoluteRejectsUnknownRoot(t *testing.T) {
	if _, err := resolveFolderPathInput("/private/2024/", "others", []string{"images", "videos", "documents", "others"}); err == nil {
		t.Fatal("expected unknown root folder to be rejected")
	}
}
