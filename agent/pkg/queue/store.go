package queue

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type JobStatus string

const (
	StatusQueued   JobStatus = "queued"
	StatusSending  JobStatus = "sending"
	StatusSuccess  JobStatus = "success"
	StatusFailed   JobStatus = "failed"
	StatusRetrying JobStatus = "retrying"
)

type Job struct {
	ID            string    `json:"id"`
	PrinterName   string    `json:"printer_name"`
	JobName       string    `json:"job_name"`
	Payload       []byte    `json:"payload,omitempty"`
	Status        JobStatus `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	Attempts      int       `json:"attempts"`
	MaxAttempts   int       `json:"max_attempts"`
	ErrorMessage  string    `json:"error_message,omitempty"`
}

type Store struct {
	db *sql.DB
}

// DB returns the underlying sql.DB connection.
func (s *Store) DB() *sql.DB {
	return s.db
}

// NewStore initializes a SQLite database connection and runs migrations.
func NewStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("dbPath cannot be empty")
	}

	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory for SQLite DB: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Optimize SQLite performance for local agent queue
	_, _ = db.Exec("PRAGMA journal_mode=WAL;")
	_, _ = db.Exec("PRAGMA synchronous=NORMAL;")

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return s, nil
}

func (s *Store) migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		printer_name TEXT NOT NULL,
		job_name TEXT NOT NULL,
		payload BLOB NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		last_attempt_at DATETIME,
		attempts INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 5,
		error_message TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);

	CREATE TABLE IF NOT EXISTS paired_apps (
		id TEXT PRIMARY KEY,
		app_name TEXT NOT NULL,
		origin TEXT NOT NULL,
		token TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		last_used_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_paired_apps_origin ON paired_apps(origin);
	CREATE INDEX IF NOT EXISTS idx_paired_apps_token ON paired_apps(token);

	CREATE TABLE IF NOT EXISTS printer_telemetry (
		printer_name TEXT PRIMARY KEY,
		driver_name TEXT,
		port_name TEXT,
		is_online INTEGER NOT NULL,
		status_description TEXT,
		last_seen_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_type TEXT NOT NULL,
		origin TEXT,
		app_name TEXT,
		details TEXT,
		ip_address TEXT,
		created_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_audit_logs_type ON audit_logs(event_type);
	CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at);
	`
	_, err := s.db.Exec(query)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateJob inserts a new print job into the queue.
func (s *Store) CreateJob(job *Job) error {
	query := `
	INSERT INTO jobs (id, printer_name, job_name, payload, status, created_at, attempts, max_attempts)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.Exec(query,
		job.ID,
		job.PrinterName,
		job.JobName,
		job.Payload,
		string(job.Status),
		job.CreatedAt.Format(time.RFC3339),
		job.Attempts,
		job.MaxAttempts,
	)
	return err
}

// GetJob retrieves a job by ID.
func (s *Store) GetJob(id string) (*Job, error) {
	query := `
	SELECT id, printer_name, job_name, payload, status, created_at, last_attempt_at, attempts, max_attempts, error_message
	FROM jobs WHERE id = ?
	`
	row := s.db.QueryRow(query, id)
	return scanJob(row)
}

// GetPendingJobs returns all jobs currently in queued or retrying status.
func (s *Store) GetPendingJobs() ([]*Job, error) {
	query := `
	SELECT id, printer_name, job_name, payload, status, created_at, last_attempt_at, attempts, max_attempts, error_message
	FROM jobs WHERE status IN ('queued', 'retrying') ORDER BY created_at ASC
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJobRows(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// ResetInflightJobs resets jobs stuck in 'sending' state back to 'queued' on agent startup.
func (s *Store) ResetInflightJobs() (int64, error) {
	res, err := s.db.Exec("UPDATE jobs SET status = 'queued' WHERE status = 'sending'")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateJobStatus updates job execution state, attempt counter, and error message.
func (s *Store) UpdateJobStatus(id string, status JobStatus, attempts int, errMsg string) error {
	now := time.Now().Format(time.RFC3339)
	query := `
	UPDATE jobs SET status = ?, attempts = ?, last_attempt_at = ?, error_message = ?
	WHERE id = ?
	`
	_, err := s.db.Exec(query, string(status), attempts, now, errMsg, id)
	return err
}

// ListRecentJobs returns recent jobs up to the specified limit.
func (s *Store) ListRecentJobs(limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
	SELECT id, printer_name, job_name, status, created_at, last_attempt_at, attempts, max_attempts, error_message
	FROM jobs ORDER BY created_at DESC LIMIT ?
	`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var j Job
		var statusStr string
		var createdAtStr string
		var lastAttemptStr sql.NullString
		var errStr sql.NullString

		err := rows.Scan(
			&j.ID,
			&j.PrinterName,
			&j.JobName,
			&statusStr,
			&createdAtStr,
			&lastAttemptStr,
			&j.Attempts,
			&j.MaxAttempts,
			&errStr,
		)
		if err != nil {
			return nil, err
		}
		j.Status = JobStatus(statusStr)
		j.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if lastAttemptStr.Valid {
			j.LastAttemptAt, _ = time.Parse(time.RFC3339, lastAttemptStr.String)
		}
		if errStr.Valid {
			j.ErrorMessage = errStr.String
		}
		jobs = append(jobs, &j)
	}
	return jobs, nil
}

// CleanOldJobs removes successful jobs older than specified retention days.
func (s *Store) CleanOldJobs(retentionDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	res, err := s.db.Exec("DELETE FROM jobs WHERE status = 'success' AND created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(s rowScanner) (*Job, error) {
	var j Job
	var statusStr string
	var createdAtStr string
	var lastAttemptStr sql.NullString
	var errStr sql.NullString

	err := s.Scan(
		&j.ID,
		&j.PrinterName,
		&j.JobName,
		&j.Payload,
		&statusStr,
		&createdAtStr,
		&lastAttemptStr,
		&j.Attempts,
		&j.MaxAttempts,
		&errStr,
	)
	if err != nil {
		return nil, err
	}
	j.Status = JobStatus(statusStr)
	j.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
	if lastAttemptStr.Valid {
		j.LastAttemptAt, _ = time.Parse(time.RFC3339, lastAttemptStr.String)
	}
	if errStr.Valid {
		j.ErrorMessage = errStr.String
	}
	return &j, nil
}

func scanJobRows(rows *sql.Rows) (*Job, error) {
	return scanJob(rows)
}

// AuditEvent types for security compliance logging.
const (
	AuditPairingRequested = "pairing_requested"
	AuditPairingConfirmed = "pairing_confirmed"
	AuditPairingFailed    = "pairing_failed"
	AuditTokenValidated   = "token_validated"
	AuditTokenRejected    = "token_rejected"
	AuditAppRevoked      = "app_revoked"
	AuditDrawerKick      = "drawer_kick"
	AuditPrintJob        = "print_job"
)

// LogAuditEvent inserts a security audit trail record.
func (s *Store) LogAuditEvent(eventType, origin, appName, details, ipAddr string) error {
	query := `
	INSERT INTO audit_logs (event_type, origin, app_name, details, ip_address, created_at)
	VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.Exec(query, eventType, origin, appName, details, ipAddr, time.Now().Format(time.RFC3339))
	return err
}

// GetAuditLogs returns recent audit log entries with an optional type filter.
func (s *Store) GetAuditLogs(eventType string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}

	var query string
	var args []any
	if eventType != "" {
		query = `SELECT id, event_type, origin, app_name, details, ip_address, created_at FROM audit_logs WHERE event_type = ? ORDER BY created_at DESC LIMIT ?`
		args = []any{eventType, limit}
	} else {
		query = `SELECT id, event_type, origin, app_name, details, ip_address, created_at FROM audit_logs ORDER BY created_at DESC LIMIT ?`
		args = []any{limit}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var createdAtStr string
		if err := rows.Scan(&e.ID, &e.EventType, &e.Origin, &e.AppName, &e.Details, &e.IPAddress, &createdAtStr); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
		entries = append(entries, e)
	}
	return entries, nil
}

type AuditEntry struct {
	ID        int       `json:"id"`
	EventType string    `json:"event_type"`
	Origin    string    `json:"origin,omitempty"`
	AppName   string    `json:"app_name,omitempty"`
	Details   string    `json:"details,omitempty"`
	IPAddress string    `json:"ip_address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
