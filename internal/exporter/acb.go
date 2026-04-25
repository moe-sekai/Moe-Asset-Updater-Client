package exporter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"moe-asset-client/internal/cricodecs/acb"
	"moe-asset-client/internal/utils"
)

func ExportACB(ctx context.Context, acbFile string, outputDir string, decodeHCA bool, deleteOriginalWav bool, convertToMP3 bool, convertToFLAC bool, ffmpegPath string, concurrentHCA int, hcaLimiter chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	parentDir := filepath.Dir(acbFile)
	extractDir, err := os.MkdirTemp(parentDir, "acb-extract-*")
	if err != nil {
		return fmt.Errorf("failed to create extraction directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(extractDir)
	}()

	_, err = acb.ExtractACBFromFile(acbFile, extractDir)
	if err != nil {
		return fmt.Errorf("failed to extract ACB file: %w", err)
	}
	hcaFiles, err := utils.FindFilesByExtension(extractDir, ".hca")
	if err != nil {
		return fmt.Errorf("failed to find HCA files: %w", err)
	}

	acbPathSlash := strings.ToLower(filepath.ToSlash(acbFile))
	if strings.Contains(acbPathSlash, "music/long") {
		var filtered []string
		for _, hf := range hcaFiles {
			bn := strings.ToLower(filepath.Base(hf))
			if strings.HasSuffix(bn, "_vr.hca") || strings.HasSuffix(bn, "_screen.hca") {
				if err := os.Remove(hf); err != nil {
					// fmt.Fprintf(os.Stderr, "failed to remove HCA variant %s: %v\n", hf, err)
				} else {
					// fmt.Fprintf(os.Stderr, "removed HCA variant: %s\n", hf)
				}
				continue
			}
			filtered = append(filtered, hf)
		}
		hcaFiles = filtered
	}

	if decodeHCA && len(hcaFiles) > 0 {
		// Write all HCA output (WAV/MP3/FLAC) to the unique extractDir first,
		// then move to outputDir after all workers complete.
		// This avoids race conditions when multiple ACBs share the same track names.
		if err := exportHCAFiles(ctx, hcaFiles, extractDir, convertToMP3, convertToFLAC, deleteOriginalWav, ffmpegPath, concurrentHCA, hcaLimiter); err != nil {
			return err
		}

		// Move result files from extractDir to outputDir
		if err := moveResultFiles(extractDir, outputDir); err != nil {
			return fmt.Errorf("failed to move results to output dir: %w", err)
		}
	}
	if err := os.Remove(acbFile); err != nil {
		return fmt.Errorf("failed to delete original ACB file: %w", err)
	}
	return nil
}

func exportHCAFiles(ctx context.Context, hcaFiles []string, extractDir string, convertToMP3 bool, convertToFLAC bool, deleteOriginalWav bool, ffmpegPath string, concurrentHCA int, hcaLimiter chan struct{}) error {
	maxWorkers := concurrentHCA
	if maxWorkers <= 0 {
		maxWorkers = 16
	}
	const maxWorkersPerACB = 4
	if maxWorkers > maxWorkersPerACB {
		maxWorkers = maxWorkersPerACB
	}
	if maxWorkers > len(hcaFiles) {
		maxWorkers = len(hcaFiles)
	}
	if maxWorkers <= 0 {
		maxWorkers = 1
	}

	jobs := make(chan string)
	errChan := make(chan error, len(hcaFiles))
	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hcaPath := range jobs {
				if err := acquireLimiter(ctx, hcaLimiter); err != nil {
					errChan <- err
					continue
				}
				func() {
					defer releaseLimiter(hcaLimiter)
					defer func() {
						if r := recover(); r != nil {
							errChan <- fmt.Errorf("panic in HCA export %s: %v", hcaPath, r)
						}
					}()
					if err := ExportHCA(hcaPath, extractDir, convertToMP3, convertToFLAC, deleteOriginalWav, ffmpegPath); err != nil {
						errChan <- fmt.Errorf("failed to export HCA %s: %w", hcaPath, err)
					}
				}()
			}
		}()
	}

	canceled := false
	for _, hcaFile := range hcaFiles {
		select {
		case <-ctx.Done():
			canceled = true
		case jobs <- hcaFile:
		}
		if canceled {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(errChan)

	if canceled {
		return ctx.Err()
	}
	var firstError error
	errorCount := 0
	for err := range errChan {
		errorCount++
		if firstError == nil {
			firstError = err
		}
		fmt.Fprintf(os.Stderr, "HCA export error: %v\n", err)
	}

	if errorCount > 0 {
		fmt.Fprintf(os.Stderr, "Error: %d HCA files failed to export\n", errorCount)
		return fmt.Errorf("failed to export %d HCA files: %w", errorCount, firstError)
	}
	return nil
}

func acquireLimiter(ctx context.Context, limiter chan struct{}) error {
	if limiter == nil {
		return nil
	}
	select {
	case limiter <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseLimiter(limiter chan struct{}) {
	if limiter != nil {
		<-limiter
	}
}

// moveResultFiles moves final audio output files from srcDir to dstDir.
// This is used to move MP3/FLAC/WAV results out of the per-ACB temp directory
// into the shared output directory after all concurrent goroutines have finished.
func moveResultFiles(srcDir, dstDir string) error {
	resultExts := map[string]bool{".mp3": true, ".flac": true, ".wav": true}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !resultExts[ext] {
			continue
		}
		src := filepath.Join(srcDir, entry.Name())
		dst := filepath.Join(dstDir, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("failed to move %s to %s: %w", src, dst, err)
		}
	}
	return nil
}
