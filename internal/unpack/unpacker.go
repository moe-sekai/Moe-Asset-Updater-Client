package unpack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"moe-asset-client/internal/config"
	"moe-asset-client/internal/exporter"
	harukiLogger "moe-asset-client/internal/logger"
	"moe-asset-client/internal/protocol"
	"moe-asset-client/internal/utils"
)

type ProgressFunc func(stage protocol.ProgressStage, progress float64, message string)

type Unpacker struct {
	cfg            *config.Config
	logger         *harukiLogger.Logger
	downloadSem    chan struct{}
	assetStudioSem chan struct{}
	postProcessSem chan struct{}
	usmSem         chan struct{}
	acbSem         chan struct{}
	hcaSem         chan struct{}
}

var (
	convertWavToMP3  = exporter.ConvertWavToMP3
	convertWavToFLAC = exporter.ConvertWavToFLAC
)

func New(cfg *config.Config, logger *harukiLogger.Logger) *Unpacker {
	download := effectiveConcurrency(logger, "download", cfg.Concurrency.Download, 2, 0)
	assetStudio := cfg.Concurrency.AssetStudio
	if assetStudio <= 0 {
		assetStudio = cfg.Worker.MaxTasks
	}
	assetStudio = effectiveConcurrency(logger, "asset_studio", assetStudio, cfg.Worker.MaxTasks, 0)
	postProcess := cfg.Concurrency.PostProcess
	if postProcess <= 0 {
		postProcess = cfg.Worker.MaxTasks
	}
	postProcess = effectiveConcurrency(logger, "postprocess", postProcess, cfg.Worker.MaxTasks, 0)
	usm := effectiveConcurrency(logger, "usm", cfg.Concurrency.USM, 4, 0)
	acb := effectiveConcurrency(logger, "acb", cfg.Concurrency.ACB, 16, 0)
	hca := effectiveConcurrency(logger, "hca", cfg.Concurrency.HCA, 16, 0)
	return &Unpacker{
		cfg:            cfg,
		logger:         logger,
		downloadSem:    make(chan struct{}, download),
		assetStudioSem: make(chan struct{}, assetStudio),
		postProcessSem: make(chan struct{}, postProcess),
		usmSem:         make(chan struct{}, usm),
		acbSem:         make(chan struct{}, acb),
		hcaSem:         make(chan struct{}, hca),
	}
}

func effectiveConcurrency(logger *harukiLogger.Logger, name string, value int, defaultValue int, maxValue int) int {
	if value <= 0 {
		value = defaultValue
	}
	if maxValue > 0 && value > maxValue {
		if logger != nil {
			logger.Warnf("concurrency.%s=%d is high; clamping to %d", name, value, maxValue)
		}
		value = maxValue
	}
	if value >= 64 && logger != nil {
		logger.Warnf("concurrency.%s=%d is high; this is allowed, but rely on GOMEMLIMIT/container memory or lower it if OOM killer appears", name, value)
	}
	return value
}

func acquireSemaphore(ctx context.Context, sem chan struct{}) error {
	if sem == nil {
		return nil
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSemaphore(sem chan struct{}) {
	if sem != nil {
		<-sem
	}
}

func (u *Unpacker) Process(ctx context.Context, task protocol.TaskPayload, report ProgressFunc) (protocol.TaskResultManifest, string, string, error) {
	if report == nil {
		report = func(protocol.ProgressStage, float64, string) {}
	}
	taskDir := filepath.Join(u.cfg.Workspace.Root, "tasks", safeName(task.TaskID))
	bundlePath := filepath.Join(taskDir, "bundle", filepath.Base(task.BundlePath)+".bundle")
	exportRoot := filepath.Join(taskDir, "export")
	if err := os.MkdirAll(filepath.Dir(bundlePath), 0o755); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}
	if err := os.MkdirAll(exportRoot, 0o755); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}

	report(protocol.StageDownload, 0.05, "downloading bundle")
	if err := u.downloadBundle(ctx, task, bundlePath); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}

	report(protocol.StageAssetStudioExport, 0.30, "exporting bundle")
	if err := acquireSemaphore(ctx, u.assetStudioSem); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}
	exportErr := u.ExtractUnityAssetBundle(ctx, bundlePath, task.BundlePath, exportRoot, task.Category, task.Export)
	releaseSemaphore(u.assetStudioSem)
	if exportErr != nil {
		return protocol.TaskResultManifest{}, "", taskDir, exportErr
	}
	_ = os.Remove(bundlePath)

	report(protocol.StagePostProcess, 0.80, "building manifest")
	manifest, err := buildManifest(exportRoot, task)
	if err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}
	manifestPath := filepath.Join(taskDir, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}

	report(protocol.StageArchive, 0.88, "archiving result")
	archivePath := filepath.Join(taskDir, "result.tar.gz")
	if err := createArchive(exportRoot, archivePath); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}
	return manifest, archivePath, taskDir, nil
}

func (u *Unpacker) downloadBundle(ctx context.Context, task protocol.TaskPayload, bundlePath string) error {
	if err := acquireSemaphore(ctx, u.downloadSem); err != nil {
		return err
	}
	defer releaseSemaphore(u.downloadSem)

	const maxRetries = 4
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			u.logger.Warnf("download %s attempt %d/%d failed: %v, retrying in %s", task.BundlePath, attempt, maxRetries, lastErr, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		if err := u.downloadBundleOnce(ctx, task, bundlePath); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func (u *Unpacker) downloadBundleOnce(ctx context.Context, task protocol.TaskPayload, bundlePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.DownloadURL, nil)
	if err != nil {
		return err
	}
	for k, v := range task.Headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", task.BundlePath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("download %s returned %d", task.BundlePath, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s returned %d", task.BundlePath, resp.StatusCode)
	}

	tmpPath := bundlePath + ".download"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	copyErr := DeobfuscateToWriter(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("download/deobfuscate %s: %w", task.BundlePath, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, bundlePath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (u *Unpacker) ExtractUnityAssetBundle(ctx context.Context, filePath string, exportPath string, outputDir string, category protocol.AssetCategory, options protocol.ExportOptions) error {
	assetStudioCLIPath := u.cfg.Tools.AssetStudioCLIPath
	if assetStudioCLIPath == "" {
		u.logger.Warnf("AssetStudioCLIPath is not configured, skipping exporting of %s", filePath)
		return nil
	}

	var excludePathPrefix string
	if options.ExportByCategory {
		excludePathPrefix = "assets/sekai/assetbundle/resources"
	} else if strings.HasPrefix(exportPath, "mysekai") && !options.ExportByCategory {
		excludePathPrefix = "assets/sekai/assetbundle/resources/ondemand"
	} else {
		excludePathPrefix = "assets/sekai/assetbundle/resources/" + strings.ToLower(string(category))
	}

	var actualExportPath string
	if options.ExportByCategory {
		actualExportPath = filepath.Join(outputDir, strings.ToLower(string(category)), exportPath)
	} else {
		actualExportPath = filepath.Join(outputDir, exportPath)
	}

	args := []string{
		filePath,
		"-m", "export",
		"-t", "monoBehaviour,textAsset,tex2d,tex2dArray,audio",
		"-g", getExportGroup(exportPath),
		"-f", "assetName",
		"-o", outputDir,
		"--strip-path-prefix", excludePathPrefix,
		"-r",
		"--filter-exclude-mode",
		"--filter-with-regex",
		"--sekai-keep-single-container-filename",
	}
	if options.UnityVersion != "" {
		args = append(args, "--unity-version", options.UnityVersion)
	}

	var exts []string
	if !options.ExportUSMFiles {
		exts = append(exts, "usm")
	}
	if !options.ExportACBFiles {
		exts = append(exts, "acb")
	}
	if len(exts) > 0 {
		regex := fmt.Sprintf(`.*\.(%s)$`, strings.Join(exts, "|"))
		args = append(args, "--filter-by-name", regex)
	}

	u.logger.Infof("Exporting asset bundle: %s to %s", filePath, actualExportPath)
	cmd := exec.CommandContext(ctx, assetStudioCLIPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to export asset bundle %s: %w", filePath, err)
	}
	u.logger.Infof("Successfully exported asset bundle: %s", filePath)
	if err := u.postProcessExportedFiles(ctx, actualExportPath, options); err != nil {
		return fmt.Errorf("post-processing failed for %s: %w", actualExportPath, err)
	}
	return nil
}

func getExportGroup(exportPath string) string {
	if exportPath == "" {
		return "container"
	}
	p := filepath.ToSlash(exportPath)
	p = strings.TrimPrefix(p, "/")
	p = strings.ToLower(p)
	prefixes := []string{
		"event/center",
		"event/thumbnail",
		"gacha/icon",
		"fix_prefab/mc_new",
		"mysekai/character/",
	}
	for _, pre := range prefixes {
		if strings.HasPrefix(p, pre) {
			return "containerFull"
		}
	}
	return "container"
}

func (u *Unpacker) postProcessExportedFiles(ctx context.Context, exportPath string, options protocol.ExportOptions) error {
	if _, err := os.Stat(exportPath); os.IsNotExist(err) {
		return nil
	}
	if err := acquireSemaphore(ctx, u.postProcessSem); err != nil {
		return err
	}
	defer releaseSemaphore(u.postProcessSem)
	if err := u.handleUSMFiles(ctx, exportPath, options); err != nil {
		return fmt.Errorf("failed to handle USM files in %s: %w", exportPath, err)
	}
	if err := u.handleACBFiles(ctx, exportPath, options); err != nil {
		return fmt.Errorf("failed to handle ACB files in %s: %w", exportPath, err)
	}
	if err := handleStandaloneWAVFiles(exportPath, options, u.cfg.Tools.FFMPEGPath); err != nil {
		return fmt.Errorf("failed to handle WAV conversion in %s: %w", exportPath, err)
	}
	if err := handlePNGConversion(exportPath, options); err != nil {
		return fmt.Errorf("failed to handle PNG conversion in %s: %w", exportPath, err)
	}
	return nil
}

func (u *Unpacker) handleUSMFiles(ctx context.Context, exportPath string, options protocol.ExportOptions) error {
	usmFiles, err := utils.FindFilesByExtension(exportPath, ".usm")
	if err != nil {
		return err
	}
	if options.ExportUSMFiles && options.DecodeUSMFiles {
		if len(usmFiles) == 0 {
			return nil
		}
		if err := acquireSemaphore(ctx, u.usmSem); err != nil {
			return err
		}
		defer releaseSemaphore(u.usmSem)
		if len(usmFiles) == 1 {
			u.logger.Infof("Exporting single USM file: %s", usmFiles[0])
			return exporter.ExportUSM(usmFiles[0], exportPath, options.ConvertVideoToMP4, options.DirectUSMToMP4WithFFmpeg, options.RemoveM2V, u.cfg.Tools.FFMPEGPath)
		}
		u.logger.Infof("Found %d USM files in %s, merging before export", len(usmFiles), exportPath)
		mergedFile, err := mergeUSMFiles(exportPath, usmFiles)
		if err != nil {
			return fmt.Errorf("failed to merge USM files: %w", err)
		}
		return exporter.ExportUSM(mergedFile, exportPath, options.ConvertVideoToMP4, options.DirectUSMToMP4WithFFmpeg, options.RemoveM2V, u.cfg.Tools.FFMPEGPath)
	}
	return nil
}

func (u *Unpacker) handleACBFiles(ctx context.Context, exportPath string, options protocol.ExportOptions) error {
	acbFiles, err := utils.FindFilesByExtension(exportPath, ".acb")
	if err != nil {
		return err
	}
	if !options.ExportACBFiles || !options.DecodeACBFiles || len(acbFiles) == 0 {
		return nil
	}

	workerCount := len(acbFiles)
	if cap(u.acbSem) > 0 && workerCount > cap(u.acbSem) {
		workerCount = cap(u.acbSem)
	}
	if workerCount <= 0 {
		workerCount = 1
	}

	jobs := make(chan string)
	errChan := make(chan error, len(acbFiles))
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				if err := acquireSemaphore(ctx, u.acbSem); err != nil {
					errChan <- err
					continue
				}
				func() {
					defer releaseSemaphore(u.acbSem)
					defer func() {
						if r := recover(); r != nil {
							errChan <- fmt.Errorf("panic in ACB export %s: %v", a, r)
						}
					}()
					u.logger.Infof("Exporting ACB file: %s", a)
					acbOutputDir := filepath.Dir(a)
					var hcaLimiter chan struct{}
					if u.cfg.Concurrency.HCAGlobal {
						hcaLimiter = u.hcaSem
					}
					if err := exporter.ExportACB(ctx, a, acbOutputDir, options.DecodeHCAFiles, options.RemoveWav, options.ConvertAudioToMP3, options.ConvertWavToFLAC, u.cfg.Tools.FFMPEGPath, u.cfg.Concurrency.HCA, hcaLimiter); err != nil {
						errChan <- fmt.Errorf("failed to export ACB %s: %w", a, err)
					}
				}()
			}
		}()
	}

	sendErr := false
	for _, acbFile := range acbFiles {
		select {
		case <-ctx.Done():
			sendErr = true
		case jobs <- acbFile:
		}
		if sendErr {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(errChan)

	if sendErr {
		return ctx.Err()
	}
	var firstErr error
	errorCount := 0
	for e := range errChan {
		errorCount++
		if firstErr == nil {
			firstErr = e
		}
		u.logger.Warnf("ACB export error: %v", e)
	}
	if errorCount > 0 {
		return fmt.Errorf("failed to export %d ACB files: %w", errorCount, firstErr)
	}
	return nil
}

func handleStandaloneWAVFiles(exportPath string, options protocol.ExportOptions, ffmpegPath string) error {
	if !options.ConvertAudioToMP3 && !options.ConvertWavToFLAC && !options.RemoveWav {
		return nil
	}
	wavFiles, err := utils.FindFilesByExtension(exportPath, ".wav")
	if err != nil {
		return err
	}
	for _, wavFile := range wavFiles {
		basePath := strings.TrimSuffix(wavFile, filepath.Ext(wavFile))
		switch {
		case options.ConvertAudioToMP3:
			mp3File := basePath + ".mp3"
			exists, err := outputFileExistsNonEmpty(mp3File)
			if err != nil {
				return err
			}
			if !exists {
				if err := convertWavToMP3(wavFile, mp3File, options.RemoveWav, ffmpegPath); err != nil {
					return fmt.Errorf("failed to convert standalone WAV %s to MP3: %w", wavFile, err)
				}
				continue
			}
			if options.RemoveWav {
				if err := removeFileIfExists(wavFile); err != nil {
					return err
				}
			}
		case options.ConvertWavToFLAC:
			flacFile := basePath + ".flac"
			exists, err := outputFileExistsNonEmpty(flacFile)
			if err != nil {
				return err
			}
			if !exists {
				if err := convertWavToFLAC(wavFile, flacFile, options.RemoveWav, ffmpegPath); err != nil {
					return fmt.Errorf("failed to convert standalone WAV %s to FLAC: %w", wavFile, err)
				}
				continue
			}
			if options.RemoveWav {
				if err := removeFileIfExists(wavFile); err != nil {
					return err
				}
			}
		case options.RemoveWav:
			if err := removeFileIfExists(wavFile); err != nil {
				return err
			}
		}
	}
	return nil
}

func outputFileExistsNonEmpty(path string) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if stat.IsDir() {
		return false, fmt.Errorf("output path %s is a directory", path)
	}
	return stat.Size() > 0, nil
}

func removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove WAV file %s: %w", path, err)
	}
	return nil
}

func handlePNGConversion(exportPath string, options protocol.ExportOptions) error {
	if !options.ConvertPhotoToWebP {
		return nil
	}
	pngFiles, err := utils.FindFilesByExtension(exportPath, ".png")
	if err != nil {
		return err
	}
	for _, pngFile := range pngFiles {
		webpFile := strings.TrimSuffix(pngFile, ".png") + ".webp"
		if err := exporter.ConvertPNGToWebP(pngFile, webpFile); err != nil {
			return fmt.Errorf("failed to convert %s to WebP: %w", pngFile, err)
		}
		if options.RemovePNG {
			if err := os.Remove(pngFile); err != nil {
				return fmt.Errorf("failed to remove original PNG %s: %w", pngFile, err)
			}
		}
	}
	return nil
}

func mergeUSMFiles(dir string, usmFiles []string) (string, error) {
	parentDirName := filepath.Base(dir)
	mergedFilePath := filepath.Join(dir, parentDirName+".usm")
	mergedFile, err := os.Create(mergedFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to create merged file: %w", err)
	}
	defer func() { _ = mergedFile.Close() }()
	for _, usmFile := range usmFiles {
		if usmFile == mergedFilePath {
			continue
		}
		src, err := os.Open(usmFile)
		if err != nil {
			return "", fmt.Errorf("failed to open %s: %w", usmFile, err)
		}
		if _, err := mergedFile.ReadFrom(src); err != nil {
			_ = src.Close()
			return "", fmt.Errorf("failed to copy %s: %w", usmFile, err)
		}
		_ = src.Close()
		if err := os.Remove(usmFile); err != nil {
			return "", fmt.Errorf("failed to delete merged USM file %s: %w", usmFile, err)
		}
	}
	return mergedFilePath, nil
}

func buildManifest(root string, task protocol.TaskPayload) (protocol.TaskResultManifest, error) {
	manifest := protocol.TaskResultManifest{
		TaskID:     task.TaskID,
		JobID:      task.JobID,
		Region:     task.Region,
		BundlePath: task.BundlePath,
		BundleHash: task.BundleHash,
		Files:      []protocol.ResultFile{},
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return manifest, nil
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		stat, err := d.Info()
		if err != nil {
			return err
		}
		sha, err := fileSHA256(path)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, protocol.ResultFile{Path: rel, Size: stat.Size(), SHA256: sha})
		return nil
	})
	if err != nil {
		return protocol.TaskResultManifest{}, err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	return manifest, nil
}

func createArchive(root string, archivePath string) error {
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gz := gzip.NewWriter(file)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		stat, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(stat, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}
