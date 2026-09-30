package queue

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"

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
	// Priority orders the queue: higher runs sooner, 0 is the default.
	Priority int `json:"priority,omitempty"`
	// IdempotencyKey deduplicates a retried POS request so the same receipt cannot
	// be enqueued twice.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// NextAttemptAt is when a retrying job becomes eligible again. Retries are
	// scheduled in the database rather than by sleeping in the worker, so one
	// struggling printer cannot stall the others.
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
}

// jobColumns is the full select list for a job row, shared so the scanners cannot
// drift apart. Keep it in sync with scanJob.
const jobColumns = `id, printer_name, job_name, payload, status, created_at, last_attempt_at,
	attempts, max_attempts, error_message, priority, idempotency_key, next_attempt_at`

// jobColumnsSummary is the dashboard variant: identical except that the payload
// (which can be kilobytes and is never displayed) is omitted.
const jobColumnsSummary = `id, printer_name, job_name, status, created_at, last_attempt_at,
	attempts, max_attempts, error_message, priority, idempotency_key, next_attempt_at`

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

	if dbPath == ":memory:" {
		// An in-memory database lives inside one connection. Without this the pool can
		// open a second connection mid-test and see an empty database ("no such table:
		// jobs"), which is a flaky-failure generator rather than a real limit.
		db.SetMaxOpenConns(1)
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
		error_message TEXT,
		priority INTEGER NOT NULL DEFAULT 0,
		idempotency_key TEXT,
		next_attempt_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
	-- Indexes over columns added by the additive migrations below are created *after*
	-- them: on a database written by an older version those columns do not exist yet,
	-- and CREATE INDEX would fail the whole migration.

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
		state TEXT,
		status_description TEXT,
		status_detail TEXT,
		last_seen_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS printer_bindings (
		printer_name TEXT PRIMARY KEY,
		port_name TEXT NOT NULL,
		device_id TEXT,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS printer_roles (
		role TEXT PRIMARY KEY,
		printer_name TEXT NOT NULL,
		label TEXT,
		capabilities TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
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
	if err != nil {
		return err
	}

	// Additive migrations: keep databases created by older versions working.
	// Each statement is idempotent because a duplicate column is treated as done.
	additive := []string{
		`ALTER TABLE printer_telemetry ADD COLUMN state TEXT`,
		`ALTER TABLE printer_telemetry ADD COLUMN status_detail TEXT`,
		`ALTER TABLE jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN idempotency_key TEXT`,
		`ALTER TABLE jobs ADD COLUMN next_attempt_at DATETIME`,
	}
	for _, stmt := range additive {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("additive migration failed (%s): %w", stmt, err)
		}
	}

	// Indexes for columns added above; these must run after the ALTERs on a database
	// created by an older version.
	postAdditive := []string{
		`CREATE INDEX IF NOT EXISTS idx_jobs_next_attempt ON jobs(next_attempt_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_idempotency ON jobs(idempotency_key) WHERE idempotency_key IS NOT NULL`,
	}
	for _, stmt := range postAdditive {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("index migration failed (%s): %w", stmt, err)
		}
	}

	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateJob inserts a new print job into the queue.
func (s *Store) CreateJob(job *Job) error {
	query := `
	INSERT INTO jobs (id, printer_name, job_name, payload, status, created_at, attempts, max_attempts,
		error_message, priority, idempotency_key, next_attempt_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		nullIfEmpty(job.ErrorMessage),
		job.Priority,
		nullIfEmpty(job.IdempotencyKey),
		nullTime(job.NextAttemptAt),
	)
	return err
}

// nullIfEmpty stores an empty string as SQL NULL, so the partial unique index on
// idempotency keys treats "no key" correctly.
func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// nullTime stores a zero time as SQL NULL.
func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.Format(time.RFC3339)
}

// GetJob retrieves a job by ID.
func (s *Store) GetJob(id string) (*Job, error) {
	row := s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// FindJobByIdempotencyKey returns the job a key already names, if any.
func (s *Store) FindJobByIdempotencyKey(key string) (*Job, error) {
	if strings.TrimSpace(key) == "" {
		return nil, sql.ErrNoRows
	}

	row := s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = ?`, key)
	return scanJob(row)
}

// GetPendingJobs returns the jobs that are eligible to run right now: queued or
// retrying, and either never attempted or past their scheduled retry time. Higher
// priority first, then oldest first so a station keeps its order.
func (s *Store) GetPendingJobs() ([]*Job, error) {
	query := `
	SELECT ` + jobColumns + `
	FROM jobs
	WHERE status IN ('queued', 'retrying')
	  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
	ORDER BY priority DESC, created_at ASC
	`
	rows, err := s.db.Query(query, time.Now().Format(time.RFC3339))
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
	return jobs, rows.Err()
}

// CountUnfinishedJobs returns how many jobs are still waiting or in flight, which is
// what the queue depth limit is measured against.
func (s *Store) CountUnfinishedJobs() (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'retrying', 'sending')`).Scan(&count)
	return count, err
}

// ResetInflightJobs requeues jobs left in 'sending' state by a previous run, and
// clears any stale retry schedule so they run immediately.
func (s *Store) ResetInflightJobs() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'queued', next_attempt_at = NULL WHERE status = 'sending'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateJobStatus updates job execution state, attempt counter, and error message.
// It also clears any retry schedule: a status change means the job is no longer
// waiting to be retried.
func (s *Store) UpdateJobStatus(id string, status JobStatus, attempts int, errMsg string) error {
	query := `
	UPDATE jobs SET status = ?, attempts = ?, last_attempt_at = ?, error_message = ?, next_attempt_at = NULL
	WHERE id = ?
	`
	_, err := s.db.Exec(query, string(status), attempts, time.Now().Format(time.RFC3339), errMsg, id)
	return err
}

// ScheduleRetry records a failed attempt and the time the next one becomes eligible,
// instead of sleeping inside the worker loop.
func (s *Store) ScheduleRetry(id string, attempts int, errMsg string, nextAttempt time.Time) error {
	query := `
	UPDATE jobs SET status = ?, attempts = ?, last_attempt_at = ?, error_message = ?, next_attempt_at = ?
	WHERE id = ?
	`
	_, err := s.db.Exec(query,
		string(StatusRetrying),
		attempts,
		time.Now().Format(time.RFC3339),
		errMsg,
		nextAttempt.Format(time.RFC3339),
		id,
	)
	return err
}

// RequeueJob puts a failed (dead-letter) job back at the front of the queue with a
// clean attempt count, which is the operator's "try that again" action.
func (s *Store) RequeueJob(id string) error {
	res, err := s.db.Exec(`
	UPDATE jobs SET status = 'queued', attempts = 0, error_message = NULL, next_attempt_at = NULL
	WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return fmt.Errorf("job %q not found", id)
	}
	return nil
}

// DeleteFailedJobs removes dead-letter jobs, which is the operator's "clear failed".
func (s *Store) DeleteFailedJobs() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE status = 'failed'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListRecentJobs returns recent jobs up to the specified limit. Payloads are omitted:
// the dashboard only displays metadata and a payload can be kilobytes.
func (s *Store) ListRecentJobs(limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.Query(`SELECT `+jobColumnsSummary+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJobSummary(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
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

// JobCounts summarises the job queue by status.
type JobCounts struct {
	Queued   int `json:"queued"`
	Sending  int `json:"sending"`
	Success  int `json:"success"`
	Failed   int `json:"failed"`
	Retrying int `json:"retrying"`
	Total    int `json:"total"`
}

// CountJobsByStatus returns per-status job counts for health reporting.
func (s *Store) CountJobsByStatus() (JobCounts, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return JobCounts{}, err
	}
	defer rows.Close()

	var counts JobCounts
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return JobCounts{}, err
		}

		switch JobStatus(status) {
		case StatusQueued:
			counts.Queued = n
		case StatusSending:
			counts.Sending = n
		case StatusSuccess:
			counts.Success = n
		case StatusFailed:
			counts.Failed = n
		case StatusRetrying:
			counts.Retrying = n
		}
		counts.Total += n
	}

	return counts, rows.Err()
}

// SavePrinterBinding records the last known good port (and device identity) for a
// queue, so a later cable move can be detected.
func (s *Store) SavePrinterBinding(b printer.PortBinding) error {
	if b.PrinterName == "" || b.PortName == "" {
		return fmt.Errorf("printer name and port name are required")
	}

	query := `
	INSERT INTO printer_bindings (printer_name, port_name, device_id, updated_at)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(printer_name) DO UPDATE SET
		port_name = excluded.port_name,
		device_id = excluded.device_id,
		updated_at = excluded.updated_at
	`
	_, err := s.db.Exec(query, b.PrinterName, b.PortName, b.DeviceID, time.Now().Format(time.RFC3339))
	return err
}

// ListPrinterBindings returns the recorded port bindings.
func (s *Store) ListPrinterBindings() ([]printer.PortBinding, error) {
	rows, err := s.db.Query(`SELECT printer_name, port_name, COALESCE(device_id, '') FROM printer_bindings ORDER BY printer_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []printer.PortBinding
	for rows.Next() {
		var b printer.PortBinding
		if err := rows.Scan(&b.PrinterName, &b.PortName, &b.DeviceID); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}

	return bindings, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(s rowScanner) (*Job, error) {
	var j Job
	var statusStr, createdAtStr string
	var lastAttemptStr, errStr, idempotencyStr, nextAttemptStr sql.NullString

	if err := s.Scan(
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
		&j.Priority,
		&idempotencyStr,
		&nextAttemptStr,
	); err != nil {
		return nil, err
	}

	applyJobStrings(&j, statusStr, createdAtStr, lastAttemptStr, errStr, idempotencyStr, nextAttemptStr)
	return &j, nil
}

// scanJobSummary scans jobColumnsSummary, which omits the payload.
func scanJobSummary(s rowScanner) (*Job, error) {
	var j Job
	var statusStr, createdAtStr string
	var lastAttemptStr, errStr, idempotencyStr, nextAttemptStr sql.NullString

	if err := s.Scan(
		&j.ID,
		&j.PrinterName,
		&j.JobName,
		&statusStr,
		&createdAtStr,
		&lastAttemptStr,
		&j.Attempts,
		&j.MaxAttempts,
		&errStr,
		&j.Priority,
		&idempotencyStr,
		&nextAttemptStr,
	); err != nil {
		return nil, err
	}

	applyJobStrings(&j, statusStr, createdAtStr, lastAttemptStr, errStr, idempotencyStr, nextAttemptStr)
	return &j, nil
}

// applyJobStrings maps the nullable column values onto a job. Both scanners share it
// so the two select lists cannot drift apart.
func applyJobStrings(j *Job, statusStr, createdAtStr string, lastAttemptStr, errStr, idempotencyStr, nextAttemptStr sql.NullString) {
	j.Status = JobStatus(statusStr)
	j.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)

	if lastAttemptStr.Valid {
		j.LastAttemptAt, _ = time.Parse(time.RFC3339, lastAttemptStr.String)
	}
	if errStr.Valid {
		j.ErrorMessage = errStr.String
	}
	if idempotencyStr.Valid {
		j.IdempotencyKey = idempotencyStr.String
	}
	if nextAttemptStr.Valid {
		j.NextAttemptAt, _ = time.Parse(time.RFC3339, nextAttemptStr.String)
	}
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
	AuditRoleAssigned    = "role_assigned"
	AuditRoleRemoved     = "role_removed"
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
