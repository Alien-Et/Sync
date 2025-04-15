package engine

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
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
	MaxRetryAttempts = 3
	RetryDelay       = 5 * time.Second
	NetworkCheckInterval = 10 * time.Second
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
	pauseMu          sync.RWMutex
	logChan          chan models.LogMessage
	logSubscribers   map[string]chan models.LogMessage
	subscribersMu    sync.RWMutex
	wg               sync.WaitGroup
	shutdown         chan struct{}
}

func NewSyncEngine(cfg models.Config, db *sql.DB) *SyncEngine {
	client := gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass)
	client.SetTimeout(30 * time.Second)

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "sync-engine").
		Logger()

	return &SyncEngine{
		client:           client,
		localDir:         filepath.Clean(cfg.LocalDir),
		remoteDir:        filepath.Clean(cfg.RemoteDir),
		mode:             cfg.Mode,
		conflicts:        make(chan models.Conflict, 100),
		taskQueue:        make(chan models.Task, 1000),
		logger:           logger,
		db:               db,
		networkAvailable: true,
		config:           cfg,
		paused:           false,
		logChan:          make(chan models.LogMessage, 1000),
		logSubscribers:   make(map[string]chan models.LogMessage),
		shutdown:         make(chan struct{}),
	}
}

func (se *SyncEngine) Start(ctx context.Context) error {
	se.log("启动同步引擎...")

	// Initialize database tables
	if err := se.initDB(); err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建文件监视器失败: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Start all background services
	g.Go(func() error { return se.monitorNetwork(ctx) })
	g.Go(func() error { return se.processTasks(ctx) })
	g.Go(func() error { return se.broadcastLogs(ctx) })
	g.Go(func() error { return se.watchLocalFiles(ctx, watcher) })
	g.Go(func() error { return se.pollRemoteChanges(ctx) })
	g.Go(func() error { return se.retryFailedTasks(ctx) })

	// Wait for shutdown or context cancellation
	select {
	case <-ctx.Done():
		se.log("收到关闭信号，正在停止引擎...")
		close(se.shutdown)
		se.wg.Wait()
		watcher.Close()
		return ctx.Err()
	case <-se.shutdown:
		watcher.Close()
		return nil
	}
}

func (se *SyncEngine) initDB() error {
	_, err := se.db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY,
			local_hash TEXT,
			remote_hash TEXT,
			local_mtime INTEGER,
			remote_mtime INTEGER,
			last_sync INTEGER,
			status TEXT,
			retry_count INTEGER DEFAULT 0
		)`)
	if err != nil {
		return err
	}

	_, err = se.db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT,
			operation TEXT,
			status TEXT,
			created_at INTEGER,
			updated_at INTEGER,
			retry_count INTEGER,
			last_error TEXT,
			FOREIGN KEY(path) REFERENCES files(path)
		)`)
	return err
}

func (se *SyncEngine) watchLocalFiles(ctx context.Context, watcher *fsnotify.Watcher) error {
	defer watcher.Close()

	// Initial directory walk to add all existing directories
	err := filepath.Walk(se.localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("初始目录遍历失败: %w", err)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if se.IsPaused() {
				continue
			}
			se.handleLocalChange(event)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			se.logError("文件监视错误", err)

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) handleLocalChange(event fsnotify.Event) {
	// Skip temporary files and hidden files
	if filepath.Base(event.Name)[0] == '.' || filepath.Ext(event.Name) == ".tmp" {
		return
	}

	relPath, err := filepath.Rel(se.localDir, event.Name)
	if err != nil {
		se.logError("获取相对路径失败", err)
		return
	}

	file := models.FileInfo{Path: relPath}

	switch {
	case event.Op&fsnotify.Remove == fsnotify.Remove:
		file.Status = models.StatusLocalDeleted
		se.log(fmt.Sprintf("检测到本地文件删除: %s", file.Path))

	case event.Op&fsnotify.Create == fsnotify.Create, event.Op&fsnotify.Write == fsnotify.Write:
		hash, mtime, err := se.calculateFileHash(event.Name)
		if err != nil {
			se.logError(fmt.Sprintf("计算文件哈希失败: %s", file.Path), err)
			return
		}
		file.LocalHash = hash
		file.LocalMtime = mtime
		file.Status = models.StatusLocalModified
		se.log(fmt.Sprintf("检测到本地文件变更: %s", file.Path))
	}

	if err := se.updateFileInDB(file); err != nil {
		se.logError("更新数据库失败", err)
		return
	}

	if se.networkAvailable {
		se.compareAndSync(file)
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

func (se *SyncEngine) compareAndSync(file models.FileInfo) {
	dbFile, err := se.getFileFromDB(file.Path)
	if err != nil {
		se.logError("从数据库获取文件失败", err)
		return
	}

	if se.isConflict(dbFile) {
		se.handleConflict(dbFile)
		return
	}

	se.queueTask(se.createSyncTask(dbFile))
}

func (se *SyncEngine) isConflict(file models.FileInfo) bool {
	switch {
	case file.Status == models.StatusLocalDeleted && file.RemoteMtime > file.LastSync:
		return true
	case file.Status == models.StatusRemoteDeleted && file.LocalMtime > file.LastSync:
		return true
	case file.Status == models.StatusLocalModified && file.RemoteMtime > file.LastSync:
		return true
	default:
		return false
	}
}

func (se *SyncEngine) handleConflict(file models.FileInfo) {
	choice := make(chan string, 1)
	se.conflicts <- models.Conflict{
		File:   file,
		Choice: choice,
	}

	select {
	case resolution := <-choice:
		switch resolution {
		case "local":
			se.queueTask(models.Task{
				Path:      file.Path,
				Operation: se.getLocalResolutionOperation(file),
			})
		case "remote":
			se.queueTask(models.Task{
				Path:      file.Path,
				Operation: se.getRemoteResolutionOperation(file),
			})
		}
	case <-time.After(30 * time.Second):
		se.log(fmt.Sprintf("冲突解决超时，跳过文件: %s", file.Path))
	}
}

func (se *SyncEngine) getLocalResolutionOperation(file models.FileInfo) string {
	switch file.Status {
	case models.StatusLocalDeleted:
		return models.OpDeleteRemote
	case models.StatusLocalModified:
		return models.OpUpload
	case models.StatusRemoteDeleted:
		return models.OpKeepLocal
	default:
		return models.OpNone
	}
}

func (se *SyncEngine) getRemoteResolutionOperation(file models.FileInfo) string {
	switch file.Status {
	case models.StatusLocalDeleted:
		return models.OpDownload
	case models.StatusLocalModified:
		return models.OpDownload
	case models.StatusRemoteDeleted:
		return models.OpDeleteLocal
	default:
		return models.OpNone
	}
}

func (se *SyncEngine) queueTask(task models.Task) {
	task.CreatedAt = time.Now().Unix()
	task.Status = models.TaskStatusPending

	select {
	case se.taskQueue <- task:
		se.log(fmt.Sprintf("任务已排队: %s %s", task.Operation, task.Path))
	default:
		se.logError("任务队列已满", errors.New("无法排队新任务"))
	}
}

func (se *SyncEngine) processTasks(ctx context.Context) error {
	for {
		select {
		case task := <-se.taskQueue:
			if err := se.executeTask(task); err != nil {
				se.logError(fmt.Sprintf("任务执行失败: %s %s", task.Operation, task.Path), err)
				task.Status = models.TaskStatusFailed
				task.LastError = err.Error()
				task.UpdatedAt = time.Now().Unix()
				se.saveTaskToDB(task)
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) executeTask(task models.Task) error {
	file, err := se.getFileFromDB(task.Path)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	switch task.Operation {
	case models.OpUpload:
		return se.uploadFile(file)
	case models.OpDownload:
		return se.downloadFile(file)
	case models.OpDeleteRemote:
		return se.deleteRemoteFile(file)
	case models.OpDeleteLocal:
		return se.deleteLocalFile(file)
	default:
		return fmt.Errorf("未知操作: %s", task.Operation)
	}
}

func (se *SyncEngine) uploadFile(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	remotePath := filepath.Join(se.remoteDir, file.Path)

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer f.Close()

	if err := se.client.WriteStream(remotePath, f, 0644); err != nil {
		return fmt.Errorf("上传文件失败: %w", err)
	}

	// Verify upload
	remoteHash, remoteMtime, err := se.getRemoteFileInfo(remotePath)
	if err != nil {
		return fmt.Errorf("验证上传失败: %w", err)
	}

	file.RemoteHash = remoteHash
	file.RemoteMtime = remoteMtime
	file.Status = models.StatusSynced
	file.LastSync = time.Now().Unix()

	if err := se.updateFileInDB(file); err != nil {
		return fmt.Errorf("更新数据库失败: %w", err)
	}

	se.log(fmt.Sprintf("文件上传成功: %s", file.Path))
	return nil
}

func (se *SyncEngine) downloadFile(file models.FileInfo) error {
	remotePath := filepath.Join(se.remoteDir, file.Path)
	localPath := filepath.Join(se.localDir, file.Path)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	reader, err := se.client.ReadStream(remotePath)
	if err != nil {
		return fmt.Errorf("打开远程文件失败: %w", err)
	}
	defer reader.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("创建本地文件失败: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, reader); err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}

	// Update local file info
	hash, mtime, err := se.calculateFileHash(localPath)
	if err != nil {
		return fmt.Errorf("计算本地哈希失败: %w", err)
	}

	file.LocalHash = hash
	file.LocalMtime = mtime
	file.Status = models.StatusSynced
	file.LastSync = time.Now().Unix()

	if err := se.updateFileInDB(file); err != nil {
		return fmt.Errorf("更新数据库失败: %w", err)
	}

	se.log(fmt.Sprintf("文件下载成功: %s", file.Path))
	return nil
}

func (se *SyncEngine) deleteRemoteFile(file models.FileInfo) error {
	remotePath := filepath.Join(se.remoteDir, file.Path)
	if err := se.client.Remove(remotePath); err != nil {
		return fmt.Errorf("删除远程文件失败: %w", err)
	}

	file.RemoteHash = ""
	file.RemoteMtime = 0
	file.Status = models.StatusSynced
	file.LastSync = time.Now().Unix()

	if err := se.updateFileInDB(file); err != nil {
		return fmt.Errorf("更新数据库失败: %w", err)
	}

	se.log(fmt.Sprintf("远程文件删除成功: %s", file.Path))
	return nil
}

func (se *SyncEngine) deleteLocalFile(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	if err := os.Remove(localPath); err != nil {
		return fmt.Errorf("删除本地文件失败: %w", err)
	}

	file.LocalHash = ""
	file.LocalMtime = 0
	file.Status = models.StatusSynced
	file.LastSync = time.Now().Unix()

	if err := se.updateFileInDB(file); err != nil {
		return fmt.Errorf("更新数据库失败: %w", err)
	}

	se.log(fmt.Sprintf("本地文件删除成功: %s", file.Path))
	return nil
}

func (se *SyncEngine) getRemoteFileInfo(path string) (string, int64, error) {
	info, err := se.client.Stat(path)
	if err != nil {
		return "", 0, err
	}

	reader, err := se.client.ReadStream(path)
	if err != nil {
		return "", 0, err
	}
	defer reader.Close()

	h := sha1.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", 0, err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), info.ModTime().Unix(), nil
}

func (se *SyncEngine) pollRemoteChanges(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if se.IsPaused() || !se.networkAvailable {
				continue
			}

			files, err := se.client.ReadDir(se.remoteDir)
			if err != nil {
				se.logError("读取远程目录失败", err)
				continue
			}

			for _, file := range files {
				if file.IsDir() {
					continue
				}

				relPath, err := filepath.Rel(se.remoteDir, filepath.Join(se.remoteDir, file.Name()))
				if err != nil {
					se.logError("获取相对路径失败", err)
					continue
				}

				remotePath := filepath.Join(se.remoteDir, relPath)
				remoteHash, remoteMtime, err := se.getRemoteFileInfo(remotePath)
				if err != nil {
					se.logError("获取远程文件信息失败", err)
					continue
				}

				dbFile, err := se.getFileFromDB(relPath)
				if err != nil {
					// New remote file
					dbFile = models.FileInfo{
						Path:        relPath,
						RemoteHash:  remoteHash,
						RemoteMtime: remoteMtime,
						Status:      models.StatusRemoteModified,
					}
				} else {
					dbFile.RemoteHash = remoteHash
					dbFile.RemoteMtime = remoteMtime
					dbFile.Status = models.StatusRemoteModified
				}

				if err := se.updateFileInDB(dbFile); err != nil {
					se.logError("更新数据库失败", err)
					continue
				}

				se.compareAndSync(dbFile)
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) retryFailedTasks(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if se.IsPaused() || !se.networkAvailable {
				continue
			}

			rows, err := se.db.Query(`
				SELECT path, operation, status, retry_count, last_error 
				FROM tasks 
				WHERE status = ? AND retry_count < ?`,
				models.TaskStatusFailed, MaxRetryAttempts)
			if err != nil {
				se.logError("查询失败任务失败", err)
				continue
			}

			for rows.Next() {
				var task models.Task
				if err := rows.Scan(&task.Path, &task.Operation, &task.Status, &task.RetryCount, &task.LastError); err != nil {
					se.logError("扫描任务失败", err)
					continue
				}

				task.RetryCount++
				task.Status = models.TaskStatusPending
				se.queueTask(task)
			}
			rows.Close()

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) monitorNetwork(ctx context.Context) error {
	ticker := time.NewTicker(NetworkCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wasAvailable := se.networkAvailable
			se.networkAvailable = se.checkNetwork()

			if !wasAvailable && se.networkAvailable {
				se.log("网络连接恢复")
				se.resumePendingTasks()
			} else if wasAvailable && !se.networkAvailable {
				se.log("网络连接断开")
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) checkNetwork() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("HEAD", se.config.URL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	return resp.StatusCode < 500
}

func (se *SyncEngine) resumePendingTasks() {
	rows, err := se.db.Query(`
		SELECT path, operation 
		FROM files 
		WHERE status IN (?, ?, ?)`,
		models.StatusLocalModified, models.StatusRemoteModified, models.StatusLocalDeleted)
	if err != nil {
		se.logError("查询待处理文件失败", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var file models.FileInfo
		if err := rows.Scan(&file.Path, &file.Status); err != nil {
			se.logError("扫描文件失败", err)
			continue
		}
		se.compareAndSync(file)
	}
}

func (se *SyncEngine) saveTaskToDB(task models.Task) error {
	_, err := se.db.Exec(`
		INSERT OR REPLACE INTO tasks 
		(path, operation, status, created_at, updated_at, retry_count, last_error) 
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		task.Path, task.Operation, task.Status, task.CreatedAt, task.UpdatedAt, task.RetryCount, task.LastError)
	return err
}

func (se *SyncEngine) getFileFromDB(path string) (models.FileInfo, error) {
	var file models.FileInfo
	row := se.db.QueryRow(`
		SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status 
		FROM files 
		WHERE path = ?`, path)
	err := row.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status)
	if err != nil {
		return file, err
	}
	return file, nil
}

func (se *SyncEngine) Pause() {
	se.pauseMu.Lock()
	defer se.pauseMu.Unlock()
	se.paused = true
	se.log("同步已暂停")
}

func (se *SyncEngine) Resume() {
	se.pauseMu.Lock()
	defer se.pauseMu.Unlock()
	se.paused = false
	se.log("同步已恢复")
	se.resumePendingTasks()
}

func (se *SyncEngine) IsPaused() bool {
	se.pauseMu.RLock()
	defer se.pauseMu.RUnlock()
	return se.paused
}

func (se *SyncEngine) SubscribeLogs(id string) chan models.LogMessage {
	se.subscribersMu.Lock()
	defer se.subscribersMu.Unlock()

	ch := make(chan models.LogMessage, 100)
	se.logSubscribers[id] = ch
	return ch
}

func (se *SyncEngine) UnsubscribeLogs(id string) {
	se.subscribersMu.Lock()
	defer se.subscribersMu.Unlock()

	if ch, exists := se.logSubscribers[id]; exists {
		close(ch)
		delete(se.logSubscribers, id)
	}
}

func (se *SyncEngine) broadcastLogs(ctx context.Context) error {
	for {
		select {
		case msg := <-se.logChan:
			se.subscribersMu.RLock()
			for _, ch := range se.logSubscribers {
				select {
				case ch <- msg:
				default:
					// Skip if subscriber channel is full
				}
			}
			se.subscribersMu.RUnlock()

		case <-ctx.Done():
			return nil
		}
	}
}

func (se *SyncEngine) log(msg string) {
	logEntry := models.LogMessage{
		Timestamp: time.Now().Format(time.RFC3339),
		Message:   msg,
		Level:     "info",
	}

	se.logger.Info().Msg(msg)
	select {
	case se.logChan <- logEntry:
	default:
		se.logger.Warn().Msg("日志通道已满，丢弃消息")
	}
}

func (se *SyncEngine) logError(msg string, err error) {
	logEntry := models.LogMessage{
		Timestamp: time.Now().Format(time.RFC3339),
		Message:   fmt.Sprintf("%s: %v", msg, err),
		Level:     "error",
	}

	se.logger.Error().Err(err).Msg(msg)
	select {
	case se.logChan <- logEntry:
	default:
		se.logger.Warn().Msg("日志通道已满，丢弃错误消息")
	}
}

func (se *SyncEngine) Close() {
	close(se.shutdown)
	se.wg.Wait()

	close(se.conflicts)
	close(se.taskQueue)
	close(se.logChan)

	se.subscribersMu.Lock()
	for id, ch := range se.logSubscribers {
		close(ch)
		delete(se.logSubscribers, id)
	}
	se.subscribersMu.Unlock()

	se.log("同步引擎已关闭")
}