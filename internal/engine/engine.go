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
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"github.com/studio-b12/gowebdav"
	"WebdavSync/internal/models"
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
}

func NewSyncEngine(cfg models.Config, db *sql.DB) *SyncEngine {
	client := gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass)
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	engine := &SyncEngine{
		client:           client,
		localDir:         cfg.LocalDir,
		remoteDir:        cfg.RemoteDir,
		mode:             cfg.Mode,
		conflicts:        make(chan models.Conflict, 100),
		taskQueue:        make(chan models.Task, 100),
		logger:           logger,
		db:               db,
		networkAvailable: true,
		config:           cfg,
		paused:           false,
		logChan:          make(chan models.LogMessage, 100),
		logSubscribers:   make(map[string]chan models.LogMessage),
	}
	go engine.monitorNetwork()
	go engine.retryTasks()
	go engine.broadcastLogs()
	return engine
}

func (se *SyncEngine) createSyncTask(file models.FileInfo) models.Task {
	var operation string
	switch file.Status {
	case "local_modified":
		operation = "upload"
	case "local_deleted":
		operation = "delete_remote"
	case "remote_modified":
		operation = "download"
	case "remote_deleted":
		operation = "delete_local"
	default:
		operation = "none"
	}

	return models.Task{
		Path:      file.Path,
		Operation: operation,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
}

func (se *SyncEngine) Conflicts() <-chan models.Conflict {
	return se.conflicts
}

func (se *SyncEngine) Pause() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.paused = true
	se.log("同步已暂停")
}

func (se *SyncEngine) Resume() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.paused = false
	se.log("同步已恢复")
	se.resumeTasks()
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
	se.log("同步配置已更新")
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
		se.logger.Warn().Msg("日志通道已满，丢弃消息")
	}
}

func (se *SyncEngine) SubscribeLogs(id string) chan models.LogMessage {
	se.mu.Lock()
	defer se.mu.Unlock()
	ch := make(chan models.LogMessage, 100)
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

func (se *SyncEngine) broadcastLogs() {
	for msg := range se.logChan {
		se.mu.Lock()
		for _, ch := range se.logSubscribers {
			select {
			case ch <- msg:
			default:
				se.logger.Warn().Msg("日志订阅者通道已满")
			}
		}
		se.mu.Unlock()
	}
}

func (se *SyncEngine) monitorNetwork() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		wasAvailable := se.networkAvailable
		se.networkAvailable = se.checkNetwork()
		
		se.mu.Lock()
		if !wasAvailable && se.networkAvailable {
			se.log("网络恢复，同步已恢复")
			se.resumeTasks()
		} else if wasAvailable && !se.networkAvailable {
			se.log("网络断开，正在缓存变更")
		}
		se.mu.Unlock()
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
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func (se *SyncEngine) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		se.log("启动文件监控失败: " + err.Error())
		return err
	}
	defer watcher.Close()

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				se.mu.Lock()
				paused := se.paused
				se.mu.Unlock()
				
				if !paused {
					se.handleLocalChange(event)
				}
				
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				se.log("文件监控错误: " + err.Error())
				
			case <-ctx.Done():
				return
			}
		}
	}()

	err = filepath.Walk(se.localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		return watcher.Add(path)
	})
	if err != nil {
		se.log("添加监控目录失败: " + err.Error())
		return err
	}

	go se.pollRemote(ctx)
	return nil
}

func (se *SyncEngine) handleLocalChange(event fsnotify.Event) {
	relPath, err := filepath.Rel(se.localDir, event.Name)
	if err != nil {
		se.log("获取相对路径失败: " + err.Error())
		return
	}
	file := models.FileInfo{Path: relPath}

	if event.Op&fsnotify.Remove == fsnotify.Remove {
		file.Status = "local_deleted"
		file.LocalMtime = 0
		file.LocalHash = ""
		se.log(fmt.Sprintf("本地文件 %s 已删除", file.Path))
		_, err := se.db.Exec("INSERT OR REPLACE INTO files (path, status, local_mtime, local_hash) VALUES (?, ?, ?, ?)", 
			file.Path, file.Status, file.LocalMtime, file.LocalHash)
		if err != nil {
			se.log("保存文件状态失败: " + err.Error())
		}
		
		se.mu.Lock()
		networkAvailable := se.networkAvailable
		se.mu.Unlock()
		
		if networkAvailable {
			se.compareAndSync(file)
		} else {
			se.queueTask(models.Task{
				Path:      file.Path,
				Operation: "delete_remote",
				Status:    "pending",
			})
		}
		return
	}

	f, err := os.Open(event.Name)
	if err != nil {
		se.log("打开文件失败: " + err.Error())
		return
	}
	defer f.Close()
	
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		se.log("计算文件哈希失败: " + err.Error())
		return
	}
	
	file.LocalHash = fmt.Sprintf("%x", h.Sum(nil))
	fi, err := f.Stat()
	if err != nil {
		se.log("获取文件信息失败: " + err.Error())
		return
	}
	
	file.LocalMtime = fi.ModTime().Unix()
	file.Status = "local_modified"
	se.log(fmt.Sprintf("本地文件 %s 已修改", file.Path))
	
	_, err = se.db.Exec("INSERT OR REPLACE INTO files (path, local_hash, local_mtime, status) VALUES (?, ?, ?, ?)",
		file.Path, file.LocalHash, file.LocalMtime, file.Status)
	if err != nil {
		se.log("保存文件状态失败: " + err.Error())
	}
	
	se.mu.Lock()
	networkAvailable := se.networkAvailable
	se.mu.Unlock()
	
	if networkAvailable {
		se.compareAndSync(file)
	} else {
		se.queueTask(models.Task{
			Path:      file.Path,
			Operation: "upload",
			Status:    "pending",
		})
	}
}

func (se *SyncEngine) pollRemote(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			se.mu.Lock()
			paused := se.paused
			networkAvailable := se.networkAvailable
			se.mu.Unlock()

			if !paused && networkAvailable {
				files, err := se.client.ReadDir(se.remoteDir)
				if err != nil {
					se.log("获取远程文件列表失败: " + err.Error())
					continue
				}

				for _, file := range files {
					relPath, err := filepath.Rel(se.remoteDir, file.Path())
					if err != nil {
						se.log("获取相对路径失败: " + err.Error())
						continue
					}

					remoteFile := models.FileInfo{
						Path:        relPath,
						RemoteMtime: file.ModTime().Unix(),
						Status:      "remote_modified",
					}

					if file.IsDir() {
						continue
					}

					se.compareAndSync(remoteFile)
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

func (se *SyncEngine) compareAndSync(file models.FileInfo) {
	dbFile, err := se.getFileFromDB(file.Path)
	if err != nil {
		se.log("获取文件信息失败: " + err.Error())
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

func (se *SyncEngine) handleConflict(file models.FileInfo) {
	choice := make(chan string, 1)
	se.conflicts <- models.Conflict{File: file, Choice: choice}

	resolution := <-choice
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
}

func (se *SyncEngine) getLocalResolutionOperation(file models.FileInfo) string {
	switch file.Status {
	case "local_modified":
		return "upload"
	case "remote_modified":
		return "delete_remote"
	default:
		return "none"
	}
}

func (se *SyncEngine) getRemoteResolutionOperation(file models.FileInfo) string {
	switch file.Status {
	case "local_modified":
		return "download"
	case "remote_modified":
		return "delete_local"
	default:
		return "none"
	}
}

func (se *SyncEngine) queueTask(task models.Task) {
	select {
	case se.taskQueue <- task:
		se.log(fmt.Sprintf("任务已排队: %s %s", task.Operation, task.Path))
	default:
		se.log("任务队列已满，丢弃任务")
	}
}

func (se *SyncEngine) retryTasks() {
	for task := range se.taskQueue {
		if err := se.executeTask(task); err != nil {
			se.log(fmt.Sprintf("任务执行失败: %s %s (%v)", task.Operation, task.Path, err))
			se.queueTask(task) // 重新排队
		}
	}
}

func (se *SyncEngine) resumeTasks() {
	tasks, err := se.getPendingTasksFromDB()
	if err != nil {
		se.log("获取待处理任务失败: " + err.Error())
		return
	}

	for _, task := range tasks {
		se.queueTask(task)
	}
}

func (se *SyncEngine) executeTask(task models.Task) error {
	file, err := se.getFileFromDB(task.Path)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	switch task.Operation {
	case "upload":
		return se.uploadWithResume(file, task)
	case "download":
		return se.download(file)
	case "delete_remote":
		return se.deleteRemote(file)
	case "delete_local":
		return se.deleteLocal(file)
	default:
		return fmt.Errorf("未知操作类型: %s", task.Operation)
	}
}

func (se *SyncEngine) uploadWithResume(file models.FileInfo, task models.Task) error {
	localPath := filepath.Join(se.localDir, file.Path)
	remotePath := filepath.Join(se.remoteDir, file.Path)

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(task.ChunkOffset, 0); err != nil {
		return fmt.Errorf("文件定位失败: %w", err)
	}

	if err := se.client.WriteStream(remotePath, f, 0644); err != nil {
		return fmt.Errorf("上传文件失败: %w", err)
	}

	file.Status = "synced"
	file.LastSync = time.Now().Unix()
	if _, err := se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
		file.Status, file.LastSync, file.Path); err != nil {
		se.log("更新文件状态失败: " + err.Error())
	}

	return nil
}

func (se *SyncEngine) download(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	remotePath := filepath.Join(se.remoteDir, file.Path)

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("创建本地目录失败: %w", err)
	}

	data, err := se.client.Read(remotePath)
	if err != nil {
		return fmt.Errorf("读取远程文件失败: %w", err)
	}

	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return fmt.Errorf("写入本地文件失败: %w", err)
	}

	file.Status = "synced"
	file.LastSync = time.Now().Unix()
	if _, err := se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
		file.Status, file.LastSync, file.Path); err != nil {
		se.log("更新文件状态失败: " + err.Error())
	}

	return nil
}

func (se *SyncEngine) deleteRemote(file models.FileInfo) error {
	remotePath := filepath.Join(se.remoteDir, file.Path)
	if err := se.client.Remove(remotePath); err != nil {
		return fmt.Errorf("删除远程文件失败: %w", err)
	}

	file.Status = "synced"
	file.LastSync = time.Now().Unix()
	if _, err := se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
		file.Status, file.LastSync, file.Path); err != nil {
		se.log("更新文件状态失败: " + err.Error())
	}

	return nil
}

func (se *SyncEngine) deleteLocal(file models.FileInfo) error {
	localPath := filepath.Join(se.localDir, file.Path)
	if err := os.Remove(localPath); err != nil {
		return fmt.Errorf("删除本地文件失败: %w", err)
	}

	file.Status = "synced"
	file.LastSync = time.Now().Unix()
	if _, err := se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
		file.Status, file.LastSync, file.Path); err != nil {
		se.log("更新文件状态失败: " + err.Error())
	}

	return nil
}

func (se *SyncEngine) getFileFromDB(path string) (models.FileInfo, error) {
	var file models.FileInfo
	row := se.db.QueryRow("SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status FROM files WHERE path = ?", path)
	err := row.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status)
	if err != nil {
		return file, fmt.Errorf("查询文件信息失败: %w", err)
	}
	return file, nil
}

func (se *SyncEngine) getPendingTasksFromDB() ([]models.Task, error) {
	rows, err := se.db.Query("SELECT path, operation, status, created_at FROM tasks WHERE status = 'pending'")
	if err != nil {
		return nil, fmt.Errorf("查询待处理任务失败: %w", err)
	}
	defer rows.Close()

	var tasks []models.Task
	for rows.Next() {
		var task models.Task
		if err := rows.Scan(&task.Path, &task.Operation, &task.Status, &task.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描任务失败: %w", err)
		}
		tasks = append(tasks, task)
	}

	return tasks, nil
}

func (se *SyncEngine) Close() {
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