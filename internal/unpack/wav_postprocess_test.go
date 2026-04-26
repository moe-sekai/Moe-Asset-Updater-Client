package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"moe-asset-client/internal/protocol"
)

func TestHandleStandaloneWAVFilesConvertMP3AndRemoveWAV(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "voice.wav")
	if err := os.WriteFile(wavPath, []byte("wav"), 0o644); err != nil {
		t.Fatalf("write wav failed: %v", err)
	}

	restoreWAVConverters(t)
	convertWavToMP3 = func(wavFile string, mp3File string, deleteOriginal bool, ffmpegPath string) error {
		if wavFile != wavPath {
			t.Fatalf("unexpected wav path: %s", wavFile)
		}
		if mp3File != filepath.Join(dir, "voice.mp3") {
			t.Fatalf("unexpected mp3 path: %s", mp3File)
		}
		if !deleteOriginal {
			t.Fatalf("expected deleteOriginal=true")
		}
		if ffmpegPath != "ffmpeg-test" {
			t.Fatalf("unexpected ffmpeg path: %s", ffmpegPath)
		}
		if err := os.WriteFile(mp3File, []byte("mp3"), 0o644); err != nil {
			return err
		}
		return os.Remove(wavFile)
	}
	convertWavToFLAC = func(string, string, bool, string) error {
		t.Fatalf("flac converter should not be called")
		return nil
	}

	err := handleStandaloneWAVFiles(dir, protocol.ExportOptions{ConvertAudioToMP3: true, RemoveWav: true}, "ffmpeg-test", nil)
	if err != nil {
		t.Fatalf("handleStandaloneWAVFiles failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "voice.mp3")); err != nil {
		t.Fatalf("expected mp3 output: %v", err)
	}
	if _, err := os.Stat(wavPath); !os.IsNotExist(err) {
		t.Fatalf("expected wav to be removed, stat err=%v", err)
	}
}

func TestHandleStandaloneWAVFilesConvertFLAC(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "voice.wav")
	if err := os.WriteFile(wavPath, []byte("wav"), 0o644); err != nil {
		t.Fatalf("write wav failed: %v", err)
	}

	restoreWAVConverters(t)
	convertWavToMP3 = func(string, string, bool, string) error {
		t.Fatalf("mp3 converter should not be called")
		return nil
	}
	convertWavToFLAC = func(wavFile string, flacFile string, deleteOriginal bool, ffmpegPath string) error {
		if wavFile != wavPath {
			t.Fatalf("unexpected wav path: %s", wavFile)
		}
		if flacFile != filepath.Join(dir, "voice.flac") {
			t.Fatalf("unexpected flac path: %s", flacFile)
		}
		if deleteOriginal {
			t.Fatalf("expected deleteOriginal=false")
		}
		return os.WriteFile(flacFile, []byte("flac"), 0o644)
	}

	err := handleStandaloneWAVFiles(dir, protocol.ExportOptions{ConvertWavToFLAC: true}, "ffmpeg-test", nil)
	if err != nil {
		t.Fatalf("handleStandaloneWAVFiles failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "voice.flac")); err != nil {
		t.Fatalf("expected flac output: %v", err)
	}
	if _, err := os.Stat(wavPath); err != nil {
		t.Fatalf("expected wav to remain: %v", err)
	}
}

func TestHandleStandaloneWAVFilesRemoveOnly(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "nested", "voice.wav")
	if err := os.MkdirAll(filepath.Dir(wavPath), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(wavPath, []byte("wav"), 0o644); err != nil {
		t.Fatalf("write wav failed: %v", err)
	}

	restoreWAVConverters(t)
	convertWavToMP3 = func(string, string, bool, string) error {
		t.Fatalf("mp3 converter should not be called")
		return nil
	}
	convertWavToFLAC = func(string, string, bool, string) error {
		t.Fatalf("flac converter should not be called")
		return nil
	}

	err := handleStandaloneWAVFiles(dir, protocol.ExportOptions{RemoveWav: true}, "ffmpeg-test", nil)
	if err != nil {
		t.Fatalf("handleStandaloneWAVFiles failed: %v", err)
	}
	if _, err := os.Stat(wavPath); !os.IsNotExist(err) {
		t.Fatalf("expected wav to be removed, stat err=%v", err)
	}
}

func TestValidateExpectedManifestOutputsRejectsResidualWAV(t *testing.T) {
	err := validateExpectedManifestOutputs(protocol.TaskResultManifest{Files: []protocol.ResultFile{
		{Path: "streaming_live/music/se_000_joint_lon_vbs_encore/sound.wav"},
	}}, protocol.ExportOptions{
		ExportACBFiles:    true,
		DecodeACBFiles:    true,
		ConvertAudioToMP3: true,
		RemoveWav:         true,
	})
	if err == nil {
		t.Fatalf("expected residual wav validation error")
	}
}

func restoreWAVConverters(t *testing.T) {
	t.Helper()
	originalMP3 := convertWavToMP3
	originalFLAC := convertWavToFLAC
	t.Cleanup(func() {
		convertWavToMP3 = originalMP3
		convertWavToFLAC = originalFLAC
	})
}
