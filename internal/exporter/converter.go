package exporter

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"strconv"

	"github.com/HugoSmits86/nativewebp"
)

type FrameRate struct {
	Numerator   int
	Denominator int
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 || b.buf.Len() < b.limit {
		remaining := b.limit - b.buf.Len()
		if b.limit <= 0 || remaining > len(p) {
			remaining = len(p)
		}
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func ffmpegBaseArgs(args ...string) []string {
	base := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	return append(base, args...)
}

func (f FrameRate) String() string {
	if f.Denominator <= 1 {
		return strconv.Itoa(f.Numerator)
	}
	return fmt.Sprintf("%d/%d", f.Numerator, f.Denominator)
}

func ConvertPNGToWebP(pngFile string, webpFile string) error {
	// Decode PNG from a file stream instead of reading the full image into memory first.
	inFile, err := os.Open(pngFile)
	if err != nil {
		return fmt.Errorf("failed to open PNG file: %w", err)
	}
	img, err := png.Decode(inFile)
	closeErr := inFile.Close()
	if err != nil {
		return fmt.Errorf("failed to decode PNG: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close PNG file: %w", closeErr)
	}

	// Encode to WebP (VP8L lossless)
	outFile, err := os.Create(webpFile)
	if err != nil {
		return fmt.Errorf("failed to create WebP file: %w", err)
	}
	defer func() { _ = outFile.Close() }()

	if err := nativewebp.Encode(outFile, img, nil); err != nil {
		_ = os.Remove(webpFile) // clean up partial file
		return fmt.Errorf("failed to encode WebP: %w", err)
	}

	return nil
}

func ConvertM2VToMP4(m2vFile string, mp4File string, deleteOriginal bool, ffmpegPath string, frameRate *FrameRate) error {
	args := make([]string, 0, 10)
	if frameRate != nil {
		rate := frameRate.String()
		// Raw M2V lacks reliable timestamps after extraction, so we rebuild them from USM metadata.
		args = append(args, "-r", rate)
	}
	args = append(args, "-i", m2vFile, "-c:v", "libx264")
	if frameRate != nil {
		args = append(args, "-r", frameRate.String())
	}
	args = append(args, "-y", mp4File)
	args = ffmpegBaseArgs(args...)

	stderr := &limitedBuffer{limit: 64 * 1024}
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert M2V to MP4: %w\nffmpeg stderr: %s", err, stderr.String())
	}
	if err := validateOutputFile(mp4File, "mp4"); err != nil {
		return err
	}
	if deleteOriginal {
		if err := os.Remove(m2vFile); err != nil {
			return fmt.Errorf("failed to delete original M2V file: %w", err)
		}
	}

	return nil
}

func ConvertUSMToMP4(usmFile string, mp4File string, ffmpegPath string) error {
	args := ffmpegBaseArgs(
		"-i", usmFile,
		"-c:v", "libx264",
		"-c:a", "aac",
		"-b:a", "192k",
		"-movflags", "+faststart",
		"-y", mp4File,
	)

	stderr := &limitedBuffer{limit: 64 * 1024}
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert USM to MP4: %w\nffmpeg stderr: %s", err, stderr.String())
	}
	if err := validateOutputFile(mp4File, "mp4"); err != nil {
		return err
	}

	return nil
}

func ConvertWavToFLAC(wavFile string, flacFile string, deleteOriginal bool, ffmpegPath string) error {
	args := ffmpegBaseArgs("-i", wavFile, "-compression_level", "12", "-y", flacFile)
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert WAV to FLAC: %w", err)
	}
	if err := validateOutputFile(flacFile, "flac"); err != nil {
		return err
	}
	if deleteOriginal {
		if _, err := os.Stat(wavFile); err == nil {
			if err := os.Remove(wavFile); err != nil {
				return fmt.Errorf("failed to delete original WAV file: %w", err)
			}
		}
	}
	return nil
}

func ConvertWavToMP3(wavFile string, mp3File string, deleteOriginal bool, ffmpegPath string) error {
	stderr := &limitedBuffer{limit: 64 * 1024}
	args := ffmpegBaseArgs("-i", wavFile, "-b:a", "320k", "-y", mp3File)
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to convert WAV to MP3: %w\nffmpeg stderr: %s", err, stderr.String())
	}
	if err := validateOutputFile(mp3File, "mp3"); err != nil {
		return err
	}
	if deleteOriginal {
		if _, err := os.Stat(wavFile); err == nil {
			if err := os.Remove(wavFile); err != nil {
				return fmt.Errorf("failed to delete original WAV file: %w", err)
			}
		}
	}
	return nil
}

// validateOutputFile checks that a converted output file exists and is non-empty.
func validateOutputFile(path string, format string) error {
	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s output file not found after ffmpeg conversion: %w", format, err)
	}
	if stat.Size() == 0 {
		_ = os.Remove(path)
		return fmt.Errorf("%s output file is empty after ffmpeg conversion: %s", format, path)
	}
	return nil
}
