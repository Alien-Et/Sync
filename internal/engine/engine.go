package engine

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"github.com/studio-b12/gowebdav"
	"golang.org/x/sync/errgroup"
	"WebdavSync/internal/models"
)

const (
	maxTaskRetries    = 5
	taskRetryInterval = time.Second
	networkCheckInterval = 10 * time.Second
	pollInterval      = 1 * time.Second
	logChannelBuffer  = 100
)

type SyncEngine struct {
	client           *gowebdav.Client
	localDir         string
	remoteDir        string
	mode             string
	conflicts        chan models.Conflict
	taskQueue        chan models.Task
	logger           zerolog.Logger
	db               *sql.DB
	networkAvailable bool
	config           models.Config
	paused           bool
	logChan          chan models.LogMessage
	logSubscribers   map[string]chan models.LogMessage
	mu               sync.Mutex
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
}

func NewSyncEngine(cfg models.Config, db *sql.DB) *SyncEngine {
	ctx, cancel := context.WithCancel(context.Background())
	engine := &SyncEngine{
		client:           gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass),
		localDir:         cfg.LocalDir,
		remoteDir:        cfg.RemoteDir,
		mode:             cfg.Mode,
		conflicts:        make(chan models.Conflict, logChannelBuffer),
		taskQueue:        make(chan models.Task, logChannelBuffer),
		logger:           zerolog.New(os.Stdout).With().Timestamp().Logger(),
		db:               db,
		networkAvailable: true,
		config:           cfg,
		paused:           false,
		logChan:          make(chan models.LogMessage, logChannelBuffer),
		logSubscribers:   make(map[string]chan models.LogMessage),
		ctx:              ctx,
		cancel:           cancel,
	}

	engine.startBackgroundServices()
	return engine
}

func (se *SyncEngine) startBackgroundServices() {
	se.wg.Add(4)
	go se.monitorNetwork()
	go se.processTaskQueue()
	go se.broadcastLogs()
	go se.cleanupResources()
}

func (se *SyncEngine) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}

	g, ctx := errgroup.WithContext(se.ctx)

	g.Go(func() error {
		defer watcher.Close()
		<-ctx.Done()
		return nil
	})

	g.Go(func() error {
		return se.watchFiles(ctx, watcher)
	})

	g.Go(func() error {
		return se.pollRemoteChanges(ctx)
	})

	return g.Wait()
}

func (se *SyncEngine) watchFiles(ctx context.Context, watcher *fsnotify.Watcher) error {
	if err := filepath.Walk(se.localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return err
		}
		return watcher.Add(path)
	}); err != nil {
		return fmt.Errorf("failed to watch directories: %w", err)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !se.IsPaused() {
				se.handleFileEvent(event)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			se.log(fmt.Sprintf("file watcher error: %v", err))

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) handleFileEvent(event fsnotify.Event) {
	relPath, err := filepath.Rel(se.localDir, event.Name)
	if err != nil {
		se.log(fmt.Sprintf("failed to get relative path: %v", err))
		return
	}

	file := models.FileInfo{Path: relPath}

	if event.Op&fsnotify.Remove == fsnotify.Remove {
		file.Status = "local_deleted"
	} else {
		hash, mtime, err := se.calculateFileHash(event.Name)
		if err != nil {
			se.log(fmt.Sprintf("failed to calculate file hash: %v", err))
			return
		}
		file.LocalHash = hash
		file.LocalMtime = mtime
		file.Status = "local_modified"
	}

	if err := se.updateFileInDB(file); err != nil {
		se.log(fmt.Sprintf("failed to update file in database: %v", err))
		return
	}

	if se.networkAvailable {
		se.syncFile(file)
	} else {
		se.queueTask(se.createSyncTask(file))
	}
}

func (se *SyncEngine) calculateFileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}

	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), fi.ModTime().Unix(), nil
}

func (se *SyncEngine) updateFileInDB(file models.FileInfo) error {
	_, err := se.db.Exec(`
		INSERT OR REPLACE INTO files 
		(path, local_hash, local_mtime, status) 
		VALUES (?, ?, ?, ?)`,
		file.Path, file.LocalHash, file.LocalMtime, file.Status)
	return err
}

func (se *SyncEngine) syncFile(file models.FileInfo) {
	dbFile, err := se.getFileFromDB(file.Path)
	if err != nil {
		se.log(fmt.Sprintf("failed to get file from database: %v", err))
		return
	}

	if se.isConflict(dbFile) {
		se.handleFileConflict(dbFile)
		return
	}

	se.queueTask(se.createSyncTask(dbFile))
}

func (se *SyncEngine) isConflict(file models.FileInfo) bool {
	switch {
	case file.Status == "local_deleted" && file.RemoteMtime > file.LastSync:
		return true
	case file.Status == "remote_deleted" && file.LocalMtime > file.LastSync:
		return true
	case file.Status == "local_modified" && file.RemoteMtime > file.LastSync:
		return true
	default:
		return false
	}
}

func (se *SyncEngine) handleFileConflict(file models.FileInfo) {
	choice := make(chan string, 1)
	select {
	case se.conflicts <- models.Conflict{File: file, Choice: choice}:
	case <-se.ctx.Done():
		return
	}

	select {
	case resolution := <-choice:
		se.resolveConflict(file, resolution)
	case <-se.ctx.Done():
	}
}

func (se *SyncEngine) resolveConflict(file models.FileInfo, resolution string) {
	var task models.Task
	switch resolution {
	case "local":
		task = models.Task{
			Path:      file.Path,
			Operation: se.getLocalResolutionOperation(file),
		}
	case "remote":
		task = models.Task{
			Path:      file.Path,
			Operation: se.getRemoteResolutionOperation(file),
		}
	default:
		se.log(fmt.Sprintf("ignoring conflict for file: %s", file.Path))
		return
	}

	se.queueTask(task)
	se.log(fmt.Sprintf("resolved conflict for %s: %s", file.Path, resolution))
}

func (se *SyncEngine) getLocalResolutionOperation(file models.FileInfo) string {
	if file.Status == "local_deleted" {
		return "delete_remote"
	}
	return "upload"
}

func (se *SyncEngine) getRemoteResolutionOperation(file models.FileInfo) string {
	if file.Status == "remote_deleted" {
		return "delete_local"
	}
	return "download"
}

func (se *SyncEngine) queueTask(task models.Task) {
	task.LastAttempt = time.Now().Unix()
	if _, err := se.db.Exec(`
		INSERT OR REPLACE INTO tasks 
		(path, operation, status, retries, last_attempt) 
		VALUES (?, ?, ?, ?, ?)`,
		task.Path, task.Operation, "pending", task.Retries, task.LastAttempt); err != nil {
		se.log(fmt.Sprintf("failed to save task: %v", err))
		return
	}

	select {
	case se.taskQueue <- task:
		se.log(fmt.Sprintf("queued task: %s %s", task.Operation, task.Path))
	case <-se.ctx.Done():
	}
}

func (se *SyncEngine) processTaskQueue() {
	defer se.wg.Done()
	
	for {
		select {
		case task := <-se.taskQueue:
			se.processTask(task)
		case <-se.ctx.Done():
			return
		}
	}
}

func (se *SyncEngine) processTask(task models.Task) {
	if se.shouldSkipTask(task) {
		time.Sleep(taskRetryInterval << uint(task.Retries))
		task.Retries++
		se.updateTaskStatus(task, "pending")
		se.queueTask(task)
		return
	}

	if err := se.executeTask(task); err != nil {
		se.log(fmt.Sprintf("task failed: %s %s: %v", task.Operation, task.Path, err))
		task.Retries++
		se.updateTaskStatus(task, "failed")
		se.queueTask(task)
	} else {
		se.updateTaskStatus(task, "completed")
		se.log(fmt.Sprintf("task completed: %s %s", task.Operation, task.Path))
	}
}

func (se *SyncEngine) shouldSkipTask(task models.Task) bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	return !se.networkAvailable || task.Retries >= maxTaskRetries || se.paused
}

func (se *SyncEngine) executeTask(task models.Task) error {
	file, err := se.getFileFromDB(task.Path)
	if err != nil {
		return fmt.Errorf("get file info: %w", err)
	}

	switch task.Operation {
	case "upload":
		return se.uploadFile(file)
	case "download":
		return se.downloadFile(file)
	case "delete_remote":
		return se.deleteRemoteFile(file)
	case "delete_local":
		return se.deleteLocalFile(file)
	default:
		return fmt.Errorf("unknown operation: %s", task.Operation)
	}
}

func (se *SyncEngine) uploadFile(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	remotePath := filepath.Join(se.remoteDir, file.Path)

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer f.Close()

	if err := se.client.WriteStream(remotePath, f, 0644); err != nil {
		return fmt.Errorf("upload file: %w", err)
	}

	return se.markFileSynced(file)
}

func (se *SyncEngine) downloadFile(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	remotePath := filepath.Join(se.remoteDir, file.Path)

	data, err := se.client.ReadStream(remotePath)
	if err != nil {
		return fmt.Errorf("read remote file: %w", err)
	}
	defer data.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create local directory: %w", err)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, data); err != nil {
		return fmt.Errorf("copy file data: %w", err)
	}

	return se.markFileSynced(file)
}

func (se *SyncEngine) deleteRemoteFile(file models.FileInfo) error {
	remotePath := filepath.Join(se.remoteDir, file.Path)
	if err := se.client.Remove(remotePath); err != nil {
		return fmt.Errorf("delete remote file: %w", err)
	}
	return se.markFileSynced(file)
}

func (se *SyncEngine) deleteLocalFile(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	if err := os.Remove(localPath); err != nil {
		return fmt.Errorf("delete local file: %w", err)
	}
	return se.markFileSynced(file)
}

func (se *SyncEngine) markFileSynced(file models.FileInfo) error {
	file.Status = "synced"
	file.LastSync = time.Now().Unix()
	_, err := se.db.Exec(`
		UPDATE files SET status = ?, last_sync = ? 
		WHERE path = ?`,
		file.Status, file.LastSync, file.Path)
	return err
}

func (se *SyncEngine) updateTaskStatus(task models.Task, status string) {
	_, err := se.db.Exec(`
		UPDATE tasks SET status = ?, last_attempt = ? 
		WHERE path = ? AND operation = ?`,
		status, time.Now().Unix(), task.Path, task.Operation)
	if err != nil {
		se.log(fmt.Sprintf("failed to update task status: %v", err))
	}
}

func (se *SyncEngine) pollRemoteChanges(ctx context.Context) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if se.IsPaused() || !se.networkAvailable {
				continue
			}

			if err := se.syncRemoteChanges(); err != nil {
				se.log(fmt.Sprintf("failed to sync remote changes: %v", err))
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) syncRemoteChanges() error {
	remoteFiles, err := se.client.ReadDir(se.remoteDir)
	if err != nil {
		return fmt.Errorf("read remote directory: %w", err)
	}

	localFiles, err := se.getLocalFilesFromDB()
	if err != nil {
		return fmt.Errorf("get local files: %w", err)
	}

	for _, lf := range localFiles {
		if err := se.processRemoteFile(lf, remoteFiles); err != nil {
			se.log(fmt.Sprintf("failed to process remote file %s: %v", lf.Path, err))
		}
	}

	return nil
}

func (se *SyncEngine) processRemoteFile(localFile models.FileInfo, remoteFiles []os.FileInfo) error {
	for _, rf := range remoteFiles {
		if localFile.Path == rf.Name() {
			localFile.RemoteMtime = rf.ModTime().Unix()
			if localFile.RemoteMtime > localFile.LastSync {
				localFile.Status = "remote_modified"
				if err := se.updateFileInDB(localFile); err != nil {
					return err
				}
				se.syncFile(localFile)
			}
			return nil
		}
	}

	if localFile.Status != "remote_deleted" && localFile.RemoteMtime > 0 {
		localFile.RemoteHash = ""
		localFile.RemoteMtime = 0
		localFile.Status = "remote_deleted"
		if err := se.updateFileInDB(localFile); err != nil {
			return err
		}
		se.syncFile(localFile)
	}

	return nil
}

func (se *SyncEngine) getLocalFilesFromDB() ([]models.FileInfo, error) {
	rows, err := se.db.Query(`
		SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status 
		FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []models.FileInfo
	for rows.Next() {
		var file models.FileInfo
		if err := rows.Scan(
			&file.Path,
			&file.LocalHash,
			&file.RemoteHash,
			&file.LocalMtime,
			&file.RemoteMtime,
			&file.LastSync,
			&file.Status,
		); err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	return files, nil
}

func (se *SyncEngine) getFileFromDB(path string) (models.FileInfo, error) {
	var file models.FileInfo
	err := se.db.QueryRow(`
		SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status 
		FROM files WHERE path = ?`,
		path).Scan(
		&file.Path,
		&file.LocalHash,
		&file.RemoteHash,
		&file.LocalMtime,
		&file.RemoteMtime,
		&file.LastSync,
		&file.Status,
	)
	return file, err
}

func (se *SyncEngine) monitorNetwork() {
	defer se.wg.Done()
	
	ticker := time.NewTicker(networkCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wasAvailable := se.networkAvailable
			se.networkAvailable = se.checkNetworkConnection()

			se.mu.Lock()
			if !wasAvailable && se.networkAvailable {
				se.log("network connection restored")
				se.resumePendingTasks()
			} else if wasAvailable && !se.networkAvailable {
				se.log("network connection lost")
			}
			se.mu.Unlock()

		case <-se.ctx.Done():
			return
		}
	}
}

func (se *SyncEngine) checkNetworkConnection() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(se.config.URL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (se *SyncEngine) resumePendingTasks() {
	tasks, err := se.getPendingTasksFromDB()
	if err != nil {
		se.log(fmt.Sprintf("failed to get pending tasks: %v", err))
		return
	}

	for _, task := range tasks {
		se.queueTask(task)
	}
}

func (se *SyncEngine) getPendingTasksFromDB() ([]models.Task, error) {
	rows, err := se.db.Query(`
		SELECT path, operation, retries, last_attempt 
		FROM tasks WHERE status = 'pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []models.Task
	for rows.Next() {
		var task models.Task
		if err := rows.Scan(
			&task.Path,
			&task.Operation,
			&task.Retries,
			&task.LastAttempt,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}

	return tasks, nil
}

func (se *SyncEngine) broadcastLogs() {
	defer se.wg.Done()
	
	for {
		select {
		case msg := <-se.logChan:
			se.mu.Lock()
			for _, ch := range se.logSubscribers {
				select {
				case ch <- msg:
				default:
					se.logger.Warn().Msg("log subscriber channel full")
				}
			}
			se.mu.Unlock()

		case <-se.ctx.Done():
			return
		}
	}
}

func (se *SyncEngine) log(msg string) {
	logEntry := models.LogMessage{
		Timestamp: time.Now().Format(time.RFC3339),
		Message:   msg,
	}

	se.logger.Info().Msg(msg)

	select {
	case se.logChan <- logEntry:
	default:
		se.logger.Warn().Msg("log channel full, message dropped")
	}
}

func (se *SyncEngine) cleanupResources() {
	defer se.wg.Done()
	
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := se.cleanupCompletedTasks(); err != nil {
				se.log(fmt.Sprintf("failed to cleanup tasks: %v", err))
			}

		case <-se.ctx.Done():
			return
		}
	}
}

func (se *SyncEngine) cleanupCompletedTasks() error {
	_, err := se.db.Exec(`
		DELETE FROM tasks 
		WHERE status = 'completed' 
		AND last_attempt < ?`,
		time.Now().Add(-24*time.Hour).Unix())
	return err
}

func (se *SyncEngine) Close() {
	se.cancel()
	se.wg.Wait()

	close(se.conflicts)
	close(se.taskQueue)
	close(se.logChan)

	se.mu.Lock()
	defer se.mu.Unlock()

	for id, ch := range se.logSubscribers {
		close(ch)
		delete(se.logSubscribers, id)
	}
}

func (se *SyncEngine) Conflicts() <-chan models.Conflict {
	return se.conflicts
}

func (se *SyncEngine) Pause() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.paused = true
	se.log("sync paused")
}

func (se *SyncEngine) Resume() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.paused = false
	se.log("sync resumed")
	se.resumePendingTasks()
}

func (se *SyncEngine) IsPaused() bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	return se.paused
}

func (se *SyncEngine) UpdateConfig(cfg models.Config) {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.config = cfg
	se.client = gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass)
	se.localDir = cfg.LocalDir
	se.remoteDir = cfg.RemoteDir
	se.mode = cfg.Mode
	se.log("configuration updated")
}

func (se *SyncEngine) SubscribeLogs(id string) <-chan models.LogMessage {
	se.mu.Lock()
	defer se.mu.Unlock()
	ch := make(chan models.LogMessage, logChannelBuffer)
	se.logSubscribers[id] = ch
	return ch
}

func (se *SyncEngine) UnsubscribeLogs(id string) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if ch, exists := se.logSubscribers[id]; exists {
		close(ch)
		delete(se.logSubscribers, id)
	}
}