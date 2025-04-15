// Unchanged from your original db.go
package db

import (
	"database/sql"
	_ "github.com/mattn/go-sqlite3"
	"WebdavSync/internal/models"
)

type DB struct {
	*sql.DB
}

func NewDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS files (
            path TEXT PRIMARY KEY,
            local_hash TEXT,
            remote_hash TEXT,
            local_mtime INTEGER,
            remote_mtime INTEGER,
            last_sync INTEGER,
            status TEXT
        );
        CREATE TABLE IF NOT EXISTS tasks (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            path TEXT,
            operation TEXT,
            status TEXT,
            retries INTEGER,
            last_attempt INTEGER,
            chunk_offset INTEGER
        );
        CREATE TABLE IF NOT EXISTS config (
            key TEXT PRIMARY KEY,
            value TEXT
        );
        CREATE TABLE IF NOT EXISTS users (
            username TEXT PRIMARY KEY,
            password TEXT
        );
    `)
	if err != nil {
		return nil, err
	}

	// Initialize default admin user if not exists
	_, err = db.Exec(`INSERT OR IGNORE INTO users (username, password) VALUES (?, ?)`, "admin", "admin123")
	if err != nil {
		return nil, err
	}

	return &DB{db}, nil
}

func (d *DB) SaveFile(file models.FileInfo) error {
	_, err := d.Exec(`
        INSERT OR REPLACE INTO files (path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, file.Path, file.LocalHash, file.RemoteHash, file.LocalMtime, file.RemoteMtime, file.LastSync, file.Status)
	return err
}

func (d *DB) GetFile(path string) (models.FileInfo, error) {
	var file models.FileInfo
	row := d.QueryRow(`
        SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status
        FROM files WHERE path = ?
    `, path)
	err := row.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status)
	return file, err
}

func (d *DB) GetFiles() ([]models.FileInfo, error) {
	rows, err := d.Query(`
        SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status
        FROM files
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []models.FileInfo
	for rows.Next() {
		var file models.FileInfo
		if err := rows.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func (d *DB) SaveTask(task models.Task) error {
	_, err := d.Exec(`
        INSERT OR REPLACE INTO tasks (id, path, operation, status, retries, last_attempt, chunk_offset)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, task.ID, task.Path, task.Operation, task.Status, task.Retries, task.LastAttempt, task.ChunkOffset)
	return err
}

func (d *DB) GetTask(path, operation string) (models.Task, error) {
	var task models.Task
	row := d.QueryRow(`
        SELECT id, path, operation, status, retries, last_attempt, chunk_offset
        FROM tasks WHERE path = ? AND operation = ?
    `, path, operation)
	err := row.Scan(&task.ID, &task.Path, &task.Operation, &task.Status, &task.Retries, &task.LastAttempt, &task.ChunkOffset)
	return task, err
}

func (d *DB) GetPendingTasks() ([]models.Task, error) {
	rows, err := d.Query(`
        SELECT id, path, operation, status, retries, last_attempt, chunk_offset
        FROM tasks WHERE status = 'pending'
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []models.Task
	for rows.Next() {
		var task models.Task
		if err := rows.Scan(&task.ID, &task.Path, &task.Operation, &task.Status, &task.Retries, &task.LastAttempt, &task.ChunkOffset); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (d *DB) AuthenticateUser(username, password string) bool {
	var dbPassword string
	err := d.QueryRow("SELECT password FROM users WHERE username = ?", username).Scan(&dbPassword)
	return err == nil && dbPassword == password
}