package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"moe-asset-client/internal/cricodecs/hca"
)

func ExportHCA(hcaFile string, outputDir string, convertToMP3 bool, convertToFLAC bool, deleteOriginalWav bool, ffmpegPath string) error {
	baseName := strings.TrimSuffix(filepath.Base(hcaFile), filepath.Ext(hcaFile))
	// Write intermediate WAV to the HCA's own directory (unique temp extract dir)
	// to avoid race conditions when multiple ACBs have tracks with the same name.
	wavFile := filepath.Join(filepath.Dir(hcaFile), baseName+".wav")

	decoder, err := hca.NewHCADecoder(hcaFile)
	if err != nil {
		return fmt.Errorf("failed to create HCA decoder: %w", err)
	}
	file, err := os.Create(wavFile)
	if err != nil {
		_ = decoder.Close()
		return fmt.Errorf("failed to create WAV file: %w", err)
	}
	if err := decoder.DecodeToWav(file); err != nil {
		_ = file.Close()
		_ = decoder.Close()
		return fmt.Errorf("failed to decode HCA to WAV: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = decoder.Close()
		return fmt.Errorf("failed to close WAV file: %w", err)
	}
	if err := decoder.Close(); err != nil {
		return fmt.Errorf("failed to close HCA decoder: %w", err)
	}

	if convertToMP3 {
		mp3File := filepath.Join(outputDir, baseName+".mp3")
		if err := ConvertWavToMP3(wavFile, mp3File, deleteOriginalWav, ffmpegPath); err != nil {
			return fmt.Errorf("failed to convert WAV to MP3: %w", err)
		}
	} else if convertToFLAC {
		flacFile := filepath.Join(outputDir, baseName+".flac")
		if err := ConvertWavToFLAC(wavFile, flacFile, deleteOriginalWav, ffmpegPath); err != nil {
			return fmt.Errorf("failed to convert WAV to FLAC: %w", err)
		}
	} else if deleteOriginalWav {
		if _, err := os.Stat(wavFile); err == nil {
			if err := os.Remove(wavFile); err != nil {
				return fmt.Errorf("failed to delete original WAV file: %w", err)
			}
		}
	}
	if err := os.Remove(hcaFile); err != nil {
		return fmt.Errorf("failed to delete original HCA file: %w", err)
	}
	return nil
}
