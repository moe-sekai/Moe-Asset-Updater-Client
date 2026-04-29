package unpack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeResultRelativePathReplacesServerUnsafeCharacters(t *testing.T) {
	got, err := sanitizeResultRelativePath("live_pv/streaming_live/timeline/0289_3/セカイ:滞留.json")
	if err != nil {
		t.Fatalf("sanitizeResultRelativePath failed: %v", err)
	}
	want := "live_pv/streaming_live/timeline/0289_3/セカイ_滞留.json"
	if got != want {
		t.Fatalf("unexpected sanitized path: got %q want %q", got, want)
	}
}

func TestSanitizeResultRelativePathRejectsTraversal(t *testing.T) {
	if got, err := sanitizeResultRelativePath("../escape.json"); err == nil {
		t.Fatalf("expected traversal path to be rejected, got %q", got)
	}
}

func TestResolvePostProcessExportPathFallsBackToOutputRoot(t *testing.T) {
	root := t.TempDir()
	actualFile := filepath.Join(root, "a", "b", "c.png")
	if err := os.MkdirAll(filepath.Dir(actualFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(actualFile, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	expectedPath := filepath.Join(root, "a", "b", "c")
	got, mismatch, err := resolvePostProcessExportPath(expectedPath, root)
	if err != nil {
		t.Fatalf("resolvePostProcessExportPath failed: %v", err)
	}
	if !mismatch {
		t.Fatalf("expected container path mismatch fallback")
	}
	if got != root {
		t.Fatalf("unexpected fallback path: got %q want %q", got, root)
	}
}

func TestResolvePostProcessExportPathKeepsExpectedPathWhenItExists(t *testing.T) {
	root := t.TempDir()
	expectedPath := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(expectedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(expectedPath, "asset.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, mismatch, err := resolvePostProcessExportPath(expectedPath, root)
	if err != nil {
		t.Fatalf("resolvePostProcessExportPath failed: %v", err)
	}
	if mismatch {
		t.Fatalf("did not expect mismatch when expected path exists")
	}
	if got != expectedPath {
		t.Fatalf("unexpected postprocess path: got %q want %q", got, expectedPath)
	}
}

func TestResolvePostProcessExportPathFallsBackWhenFilesExistOutsideExpectedPath(t *testing.T) {
	root := t.TempDir()
	expectedPath := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(expectedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(root, "a", "b", "c.png")
	if err := os.WriteFile(outsideFile, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, mismatch, err := resolvePostProcessExportPath(expectedPath, root)
	if err != nil {
		t.Fatalf("resolvePostProcessExportPath failed: %v", err)
	}
	if !mismatch {
		t.Fatalf("expected mismatch when files exist outside expected path")
	}
	if got != root {
		t.Fatalf("unexpected fallback path: got %q want %q", got, root)
	}
}
