package unpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moe-asset-client/internal/protocol"
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

func TestAssetStudioExportTypesAddsMeshForConfiguredPaths(t *testing.T) {
	options := protocol.ExportOptions{
		ExportMeshOBJ: true,
		MeshOBJPathPatterns: []string{
			`^mysekai/fixture(?:/|$)`,
			`^mysekai/site/field/my_room_asset/skin(?:/|$)`,
		},
	}
	tests := []string{
		"mysekai/fixture/foo",
		`mysekai\fixture\foo`,
		"/mysekai/site/field/my_room_asset/skin/foo",
		"mysekai/site/field/my_room_asset/skin",
	}
	for _, path := range tests {
		got, err := assetStudioExportTypes(path, options)
		if err != nil {
			t.Fatalf("assetStudioExportTypes(%q) failed: %v", path, err)
		}
		if !exportTypesContain(got, "mesh") {
			t.Fatalf("expected mesh export type for %q, got %q", path, got)
		}
	}
}

func TestAssetStudioExportTypesDoesNotOvermatchMeshPaths(t *testing.T) {
	options := protocol.ExportOptions{
		ExportMeshOBJ: true,
		MeshOBJPathPatterns: []string{
			`^mysekai/fixture(?:/|$)`,
			`^mysekai/site/field/my_room_asset/skin(?:/|$)`,
		},
	}
	tests := []string{
		"mysekai/fixtures/foo",
		"mysekai/site/field/my_room_asset/skinny/foo",
		"mysekai/character/foo",
	}
	for _, path := range tests {
		got, err := assetStudioExportTypes(path, options)
		if err != nil {
			t.Fatalf("assetStudioExportTypes(%q) failed: %v", path, err)
		}
		if exportTypesContain(got, "mesh") {
			t.Fatalf("did not expect mesh export type for %q, got %q", path, got)
		}
	}
}

func TestAssetStudioExportTypesKeepsMeshDisabledWhenOptionIsFalse(t *testing.T) {
	got, err := assetStudioExportTypes("mysekai/fixture/foo", protocol.ExportOptions{
		ExportMeshOBJ:       false,
		MeshOBJPathPatterns: []string{`^mysekai/fixture(?:/|$)`},
	})
	if err != nil {
		t.Fatalf("assetStudioExportTypes failed: %v", err)
	}
	if exportTypesContain(got, "mesh") {
		t.Fatalf("did not expect mesh export type when ExportMeshOBJ is false, got %q", got)
	}
	for _, want := range []string{"monoBehaviour", "textAsset", "tex2d", "tex2dArray", "audio"} {
		if !exportTypesContain(got, want) {
			t.Fatalf("expected default export type %q in %q", want, got)
		}
	}
}

func TestAssetStudioExportTypesReturnsInvalidMeshPatternError(t *testing.T) {
	_, err := assetStudioExportTypes("mysekai/fixture/foo", protocol.ExportOptions{
		ExportMeshOBJ:       true,
		MeshOBJPathPatterns: []string{"["},
	})
	if err == nil {
		t.Fatalf("expected invalid regex error")
	}
	if !strings.Contains(err.Error(), "invalid mesh obj path pattern") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func exportTypesContain(types string, want string) bool {
	for _, t := range strings.Split(types, ",") {
		if t == want {
			return true
		}
	}
	return false
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
