package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"moe-asset-client/internal/config"
	harukiLogger "moe-asset-client/internal/logger"
	"moe-asset-client/internal/protocol"
	"moe-asset-client/internal/unpack"

	"github.com/go-resty/resty/v2"
)

type Worker struct {
	cfg      *config.Config
	logger   *harukiLogger.Logger
	http     *resty.Client
	unpacker *unpack.Unpacker

	clientID string
	activeMu sync.Mutex
	active   map[string]struct{}
}

func NewWorker(cfg *config.Config, logger *harukiLogger.Logger) *Worker {
	httpClient := resty.New().SetBaseURL(cfg.Client.ServerURL)
	httpClient.SetHeader("User-Agent", cfg.Client.UserAgent)
	if cfg.Client.BearerToken != "" {
		httpClient.SetHeader("Authorization", "Bearer "+cfg.Client.BearerToken)
	}
	return &Worker{
		cfg:      cfg,
		logger:   logger,
		http:     httpClient,
		unpacker: unpack.New(cfg, logger),
		active:   make(map[string]struct{}),
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.cfg.Workspace.Root, 0o755); err != nil {
		return err
	}
	if err := w.register(ctx); err != nil {
		return err
	}
	w.logger.Infof("registered as client %s", w.clientID)
	w.warnOnHighConcurrency()
	w.startDiagnostics(ctx)

	go w.heartbeatLoop(ctx)

	sem := make(chan struct{}, w.cfg.Worker.MaxTasks)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		available := cap(sem) - len(sem)
		if available <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		tasks, err := w.lease(ctx, available)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warnf("lease failed: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if len(tasks) == 0 {
			continue
		}
		for _, task := range tasks {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(task protocol.TaskPayload) {
				defer wg.Done()
				defer func() { <-sem }()
				w.runTaskSafely(ctx, task)
			}(task)
		}
	}
}

func (w *Worker) register(ctx context.Context) error {
	req := protocol.ClientRegistrationRequest{
		ClientID: w.cfg.Worker.ID,
		Name:     w.cfg.Worker.Name,
		Version:  w.cfg.Worker.Version,
		MaxTasks: w.cfg.Worker.MaxTasks,
		Tags:     w.cfg.Worker.Tags,
	}
	var resp protocol.ClientRegistrationResponse
	r, err := w.http.R().SetContext(ctx).SetBody(req).SetResult(&resp).Post("/api/v1/clients/register")
	if err != nil {
		return err
	}
	if r.StatusCode() >= 300 {
		return fmt.Errorf("register returned %d: %s", r.StatusCode(), r.String())
	}
	w.clientID = resp.ClientID
	return nil
}

func (w *Worker) warnOnHighConcurrency() {
	if w.cfg.Worker.MaxTasks > 16 {
		w.logger.Warnf("worker.max_tasks=%d is high; for large bundles consider 4-8 to avoid memory/process exhaustion", w.cfg.Worker.MaxTasks)
	}
	if w.cfg.Concurrency.Download > 8 || w.cfg.Concurrency.ACB > 16 || w.cfg.Concurrency.USM > 8 || w.cfg.Concurrency.HCA > 16 {
		w.logger.Warnf("configured concurrency is high (download=%d acb=%d usm=%d hca=%d); client will clamp internal post-processing limits where possible", w.cfg.Concurrency.Download, w.cfg.Concurrency.ACB, w.cfg.Concurrency.USM, w.cfg.Concurrency.HCA)
	}
}

func (w *Worker) startDiagnostics(ctx context.Context) {
	if w.cfg.Diagnostics.PprofAddress != "" {
		addr := w.cfg.Diagnostics.PprofAddress
		go func() {
			w.logger.Infof("pprof diagnostics listening on %s", addr)
			if err := http.ListenAndServe(addr, nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
				w.logger.Warnf("pprof diagnostics stopped: %v", err)
			}
		}()
	}
	if w.cfg.Diagnostics.RuntimeStatsIntervalSeconds > 0 {
		go w.runtimeStatsLoop(ctx)
	}
}

func (w *Worker) runtimeStatsLoop(ctx context.Context) {
	interval := time.Duration(w.cfg.Diagnostics.RuntimeStatsIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.logRuntimeStats()
		}
	}
}

func (w *Worker) logRuntimeStats() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	goroutines := runtime.NumGoroutine()
	active := w.activeTaskCount()
	w.logger.Infof("runtime stats: goroutines=%d active_tasks=%d heap_alloc=%s heap_sys=%s stack_inuse=%s next_gc=%s gc=%d", goroutines, active, formatBytes(mem.HeapAlloc), formatBytes(mem.HeapSys), formatBytes(mem.StackInuse), formatBytes(mem.NextGC), mem.NumGC)
	if w.cfg.Diagnostics.WarnGoroutines > 0 && goroutines >= w.cfg.Diagnostics.WarnGoroutines {
		w.logger.Warnf("goroutine count %d reached warning threshold %d", goroutines, w.cfg.Diagnostics.WarnGoroutines)
	}
	warnHeap := w.cfg.Diagnostics.WarnHeapMB * 1024 * 1024
	if warnHeap > 0 && mem.HeapAlloc >= warnHeap {
		w.logger.Warnf("heap allocation %s reached warning threshold %s", formatBytes(mem.HeapAlloc), formatBytes(warnHeap))
	}
}

func formatBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%dB", v)
	}
	div, exp := uint64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(v)/float64(div), "KMGTPE"[exp])
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.HeartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.heartbeat(ctx); err != nil {
				w.logger.Warnf("heartbeat failed: %v", err)
			}
		}
	}
}

func (w *Worker) heartbeat(ctx context.Context) error {
	req := protocol.HeartbeatRequest{ClientID: w.clientID, ActiveTaskIDs: w.activeTaskIDs()}
	r, err := w.http.R().SetContext(ctx).SetBody(req).Post("/api/v1/clients/" + w.clientID + "/heartbeat")
	if err != nil {
		return err
	}
	if r.StatusCode() >= 300 {
		return fmt.Errorf("heartbeat returned %d: %s", r.StatusCode(), r.String())
	}
	return nil
}

func (w *Worker) lease(ctx context.Context, maxTasks int) ([]protocol.TaskPayload, error) {
	if maxTasks <= 0 || maxTasks > w.cfg.Worker.MaxTasks {
		maxTasks = w.cfg.Worker.MaxTasks
	}
	req := protocol.LeaseRequest{
		ClientID:    w.clientID,
		MaxTasks:    maxTasks,
		WaitSeconds: w.cfg.LeaseWaitSeconds(),
	}

	var resp protocol.LeaseResponse
	r, err := w.http.R().SetContext(ctx).SetBody(req).SetResult(&resp).Post("/api/v1/tasks/lease")
	if err != nil {
		return nil, err
	}
	if r.StatusCode() >= 300 {
		return nil, fmt.Errorf("lease returned %d: %s", r.StatusCode(), r.String())
	}
	return resp.Tasks, nil
}

func (w *Worker) runTaskSafely(parent context.Context, task protocol.TaskPayload) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			w.logger.Errorf("task %s panicked: %v\n%s", task.TaskID, r, stack)
			failCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := w.fail(failCtx, task.TaskID, fmt.Sprintf("panic: %v", r)); err != nil {
				w.logger.Warnf("failed to report panic for %s: %v", task.TaskID, err)
			}
		}
	}()
	w.handleTask(parent, task)
}

func (w *Worker) handleTask(parent context.Context, task protocol.TaskPayload) {
	w.addActive(task.TaskID)
	defer w.removeActive(task.TaskID)

	ctx, cancel := context.WithTimeout(parent, time.Duration(w.cfg.Client.TimeoutSeconds)*time.Second)
	defer cancel()

	w.logger.Infof("start task %s (%s)", task.TaskID, task.BundlePath)
	report := func(stage protocol.ProgressStage, progress float64, message string) {
		if err := w.reportProgress(ctx, task.TaskID, stage, progress, message); err != nil {
			w.logger.Warnf("failed to report progress for %s: %v", task.TaskID, err)
		}
	}

	_, archivePath, taskDir, err := w.unpacker.Process(ctx, task, report)
	manifestPath := filepath.Join(taskDir, "manifest.json")
	if err != nil {
		w.logger.Errorf("task %s failed: %v", task.TaskID, err)
		_ = w.fail(ctx, task.TaskID, err.Error())
		w.cleanupTaskDir(taskDir, true)
		return
	}

	report(protocol.StageUploadResult, 0.92, "uploading result to server")
	if err := w.uploadResult(ctx, task.TaskID, manifestPath, archivePath); err != nil {
		w.logger.Errorf("task %s result upload failed: %v", task.TaskID, err)
		_ = w.fail(ctx, task.TaskID, err.Error())
		w.cleanupTaskDir(taskDir, true)
		return
	}
	w.logger.Infof("task %s completed", task.TaskID)
	w.cleanupTaskDir(taskDir, false)
}

func (w *Worker) reportProgress(ctx context.Context, taskID string, stage protocol.ProgressStage, progress float64, message string) error {
	req := protocol.ProgressRequest{ClientID: w.clientID, Stage: stage, Progress: progress, Message: message}
	r, err := w.http.R().SetContext(ctx).SetBody(req).Post("/api/v1/tasks/" + taskID + "/progress")
	if err != nil {
		return err
	}
	if r.StatusCode() >= 300 {
		return fmt.Errorf("progress returned %d: %s", r.StatusCode(), r.String())
	}
	return nil
}

func (w *Worker) uploadResult(ctx context.Context, taskID string, manifestPath string, archivePath string) error {
	r, err := w.http.R().
		SetContext(ctx).
		SetFile("manifest", manifestPath).
		SetFile("archive", archivePath).
		Post("/api/v1/tasks/" + taskID + "/result")
	if err != nil {
		return err
	}
	if r.StatusCode() >= http.StatusMultipleChoices {
		return fmt.Errorf("result upload returned %d: %s", r.StatusCode(), r.String())
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, taskID string, message string) error {
	req := protocol.FailRequest{ClientID: w.clientID, Error: message}
	r, err := w.http.R().SetContext(ctx).SetBody(req).Post("/api/v1/tasks/" + taskID + "/fail")
	if err != nil {
		return err
	}
	if r.StatusCode() >= 300 {
		return fmt.Errorf("fail returned %d: %s", r.StatusCode(), r.String())
	}
	return nil
}

func (w *Worker) addActive(taskID string) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	w.active[taskID] = struct{}{}
}

func (w *Worker) removeActive(taskID string) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	delete(w.active, taskID)
}

func (w *Worker) activeTaskIDs() []string {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	out := make([]string, 0, len(w.active))
	for taskID := range w.active {
		out = append(out, taskID)
	}
	return out
}

func (w *Worker) activeTaskCount() int {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	return len(w.active)
}

func (w *Worker) cleanupTaskDir(taskDir string, failed bool) {
	if taskDir == "" || !w.cfg.Workspace.Cleanup {
		return
	}
	if failed && w.cfg.Workspace.KeepFailed {
		return
	}
	_ = os.RemoveAll(taskDir)
}
