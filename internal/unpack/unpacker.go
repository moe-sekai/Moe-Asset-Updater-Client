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

	"github.com/go-resty/resty/v2"
)

type ProgressFunc func(stage protocol.ProgressStage, progress float64, message string)

type Unpacker struct {
	cfg    *config.Config
	logger *harukiLogger.Logger
	usmSem chan struct{}
	acbSem chan struct{}
}

func New(cfg *config.Config, logger *harukiLogger.Logger) *Unpacker {
	usm := cfg.Concurrency.USM
	if usm <= 0 {
		usm = 4
	}
	acb := cfg.Concurrency.ACB
	if acb <= 0 {
		acb = 16
	}
	return &Unpacker{
		cfg:    cfg,
		logger: logger,
		usmSem: make(chan struct{}, usm),
		acbSem: make(chan struct{}, acb),
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
	body, err := u.download(ctx, task)
	if err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}

	report(protocol.StageDeobfuscate, 0.20, "deobfuscating bundle")
	body = Deobfuscate(body)
	if err := os.WriteFile(bundlePath, body, 0o644); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
	}

	report(protocol.StageAssetStudioExport, 0.30, "exporting bundle")
	if err := u.ExtractUnityAssetBundle(ctx, bundlePath, task.BundlePath, exportRoot, task.Category, task.Export); err != nil {
		return protocol.TaskResultManifest{}, "", taskDir, err
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

func (u *Unpacker) download(ctx context.Context, task protocol.TaskPayload) ([]byte, error) {
	client := resty.New()
	for k, v := range task.Headers {
		client.SetHeader(k, v)
	}

	const maxRetries = 4
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			u.logger.Warnf("download %s attempt %d/%d failed: %v, retrying in %s", task.BundlePath, attempt, maxRetries, lastErr, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		resp, err := client.R().SetContext(ctx).Get(task.DownloadURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("download %s: %w", task.BundlePath, err)
			continue
		}
		if resp.StatusCode() >= 500 {
			lastErr = fmt.Errorf("download %s returned %d", task.BundlePath, resp.StatusCode())
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
			return nil, fmt.Errorf("download %s returned %d", task.BundlePath, resp.StatusCode())
		}
		return resp.Body(), nil
	}
	return nil, lastErr
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
	if err := u.postProcessExportedFiles(actualExportPath, options); err != nil {
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

func (u *Unpacker) postProcessExportedFiles(exportPath string, options protocol.ExportOptions) error {
	if _, err := os.Stat(exportPath); os.IsNotExist(err) {
		return nil
	}
	if err := u.handleUSMFiles(exportPath, options); err != nil {
		return fmt.Errorf("failed to handle USM files in %s: %w", exportPath, err)
	}
	if err := u.handleACBFiles(exportPath, options); err != nil {
		return fmt.Errorf("failed to handle ACB files in %s: %w", exportPath, err)
	}
	if err := handlePNGConversion(exportPath, options); err != nil {
		return fmt.Errorf("failed to handle PNG conversion in %s: %w", exportPath, err)
	}
	return nil
}

func (u *Unpacker) handleUSMFiles(exportPath string, options protocol.ExportOptions) error {
	usmFiles, err := utils.FindFilesByExtension(exportPath, ".usm")
	if err != nil {
		return err
	}
	if options.ExportUSMFiles && options.DecodeUSMFiles {
		if len(usmFiles) == 0 {
			return nil
		}
		u.usmSem <- struct{}{}
		defer func() { <-u.usmSem }()
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

func (u *Unpacker) handleACBFiles(exportPath string, options protocol.ExportOptions) error {
	acbFiles, err := utils.FindFilesByExtension(exportPath, ".acb")
	if err != nil {
		return err
	}
	if options.ExportACBFiles && options.DecodeACBFiles {
		if len(acbFiles) == 0 {
			return nil
		}
		var wg sync.WaitGroup
		errChan := make(chan error, len(acbFiles))
		for _, acbFile := range acbFiles {
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				u.acbSem <- struct{}{}
				defer func() { <-u.acbSem }()
				u.logger.Infof("Exporting ACB file: %s", a)
				acbOutputDir := filepath.Dir(a)
				if err := exporter.ExportACB(a, acbOutputDir, options.DecodeHCAFiles, options.RemoveWav, options.ConvertAudioToMP3, options.ConvertWavToFLAC, u.cfg.Tools.FFMPEGPath, u.cfg.Concurrency.HCA); err != nil {
					errChan <- fmt.Errorf("failed to export ACB %s: %w", a, err)
				}
			}(acbFile)
		}
		wg.Wait()
		close(errChan)
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
