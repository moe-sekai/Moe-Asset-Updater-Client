package unpack

import "testing"

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
