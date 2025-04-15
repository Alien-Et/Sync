// Unchanged from your original model.go, with minor additions
package models

import (
	"database/sql"
)

type Config struct {
	URL       string
	User      string
	Pass      string
	LocalDir  string
	RemoteDir string
	Mode      string
}

func DefaultConfig() Config {
	return Config{
		URL:       "",
		User:      "",
		Pass:      "",
		LocalDir:  "",
		RemoteDir: "",
		Mode:      "bidirectional",
	}
}

func Load(db *sql.DB) (Config, error) {
	cfg := DefaultConfig()
	rows, err := db.Query("SELECT key, value FROM config")
	if err != nil {
		return cfg, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return cfg, err
		}
		switch key {
		case "url":
			cfg.URL = value
		case "user":
			cfg.User = value
		case "pass":
			cfg.Pass = value
		case "local_dir":
			cfg.LocalDir = value
		case "remote_dir":
			cfg.RemoteDir = value
		case "mode":
			cfg.Mode = value
		}
	}
	return cfg, nil
}

func Save(db *sql.DB, cfg Config) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsert := `INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`
	_, err = tx.Exec(upsert, "url", cfg.URL)
	if err != nil {
		return err
	}
	_, err = tx.Exec(upsert, "user", cfg.User)
	if err != nil {
		return err
	}
	_, err = tx.Exec(upsert, "pass", cfg.Pass)
	if err != nil {
		return err
	}
	_, err = tx.Exec(upsert, "local_dir", cfg.LocalDir)
	if err != nil {
		return err
	}
	_, err = tx.Exec(upsert, "remote_dir", cfg.RemoteDir)
	if err != nil {
		return err
	}
	_, err = tx.Exec(upsert, "mode", cfg.Mode)
	if err != nil {
		return err
	}

	return tx.Commit()
}

type FileInfo struct {
	Path        string
	LocalHash   string
	RemoteHash  string
	LocalMtime  int64
	RemoteMtime int64
	LastSync    int64
	Status      string
}

type Task struct {
	ID          int64
	Path        string
	Operation   string
	Status      string
	Retries     int
	LastAttempt int64
	ChunkOffset int64
}

type Conflict struct {
	File   FileInfo
	Choice chan string
}

type LogMessage struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}