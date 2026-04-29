package client

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"moe-asset-client/internal/protocol"
	"moe-asset-client/internal/tcpclient"
)

func (w *Worker) runTCP(ctx context.Context) error {
	if err := os.MkdirAll(w.cfg.Workspace.Root, 0o755); err != nil {
		return err
	}
	w.configureMemoryLimit()
	w.warnOnHighConcurrency()
	w.startDiagnostics(ctx)

	reconnectDelay := time.Duration(w.cfg.Client.TCPReconnectSeconds) * time.Second
	if reconnectDelay <= 0 {
		reconnectDelay = 5 * time.Second
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := w.runTCPOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.logger.Warnf("TCP task connection stopped: %v; reconnecting in %s", err, reconnectDelay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

func (w *Worker) runTCPOnce(ctx context.Context) error {
	writeTimeout := 30 * time.Second
	conn, err := tcpclient.Dial(ctx, w.cfg.Client.TCPAddress, writeTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	clientID := w.clientID
	if clientID == "" {
		clientID = w.cfg.Worker.ID
	}
	register := tcpclient.Message{
		Type:          tcpclient.MessageRegister,
		ClientID:      clientID,
		Name:          w.cfg.Worker.Name,
		Version:       w.cfg.Worker.Version,
		MaxTasks:      w.cfg.Worker.MaxTasks,
		Tags:          w.cfg.Worker.Tags,
		UserAgent:     w.cfg.Client.UserAgent,
		BearerToken:   w.cfg.Client.BearerToken,
		ActiveTaskIDs: w.activeTaskIDs(),
	}
	if err := conn.Send(register); err != nil {
		return err
	}
	registered, err := conn.Receive()
	if err != nil {
		return err
	}
	if registered.Type == tcpclient.MessageError {
		return fmt.Errorf("tcp register rejected: %s", registered.Error)
	}
	if registered.Type != tcpclient.MessageRegistered {
		return fmt.Errorf("unexpected tcp register response %q", registered.Type)
	}
	w.clientID = registered.ClientID
	w.logger.Infof("registered TCP task connection as client %s", w.clientID)
	return w.runTCPConnection(ctx, conn)
}

func (w *Worker) runTCPConnection(parent context.Context, conn *tcpclient.Conn) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	sem := make(chan struct{}, w.cfg.Worker.MaxTasks)
	var wg sync.WaitGroup
	defer wg.Wait()

	heartbeatErr := make(chan error, 1)
	go w.tcpHeartbeatLoop(ctx, conn, heartbeatErr)
	transport := w.tcpTaskTransport(conn)

	for {
		select {
		case err := <-heartbeatErr:
			cancel()
			_ = conn.Close()
			return err
		default:
		}

		msg, err := conn.Receive()
		if err != nil {
			cancel()
			return err
		}
		switch msg.Type {
		case tcpclient.MessageTaskPush:
			for _, task := range msg.Tasks {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case sem <- struct{}{}:
				}
				wg.Add(1)
				go func(task protocol.TaskPayload) {
					defer wg.Done()
					defer func() { <-sem }()
					w.runTaskSafelyWithTransport(ctx, task, transport)
				}(task)
			}
		case tcpclient.MessageError:
			w.logger.Warnf("TCP server error: %s", msg.Error)
		default:
			w.logger.Warnf("ignoring unexpected TCP message type %q", msg.Type)
		}
	}
}

func (w *Worker) tcpHeartbeatLoop(ctx context.Context, conn *tcpclient.Conn, errCh chan<- error) {
	sendHeartbeat := func() error {
		return conn.Send(tcpclient.Message{Type: tcpclient.MessageHeartbeat, ClientID: w.clientID, ActiveTaskIDs: w.activeTaskIDs()})
	}
	if err := sendHeartbeat(); err != nil {
		select {
		case errCh <- err:
		default:
		}
		return
	}
	ticker := time.NewTicker(w.cfg.HeartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				_ = conn.Close()
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}
}

func (w *Worker) tcpTaskTransport(conn *tcpclient.Conn) taskTransport {
	send := func(msg tcpclient.Message) error {
		err := conn.Send(msg)
		if err != nil {
			_ = conn.Close()
		}
		return err
	}
	return taskTransport{
		reportProgress: func(ctx context.Context, taskID string, stage protocol.ProgressStage, progress float64, message string) error {
			return send(tcpclient.Message{Type: tcpclient.MessageProgress, ClientID: w.clientID, TaskID: taskID, Stage: stage, Progress: progress, Message: message})
		},
		fail: func(ctx context.Context, taskID string, message string) error {
			return send(tcpclient.Message{Type: tcpclient.MessageFail, ClientID: w.clientID, TaskID: taskID, Error: message})
		},
	}
}
