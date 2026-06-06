package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ScheduledTask is one user-definable recurring agent task (docs/scheduled-tasks.md).
// The schedule is period_days + at_hour (a local wall-clock boundary), or every_seconds
// when at_hour < 0 (pure interval). The target (agent group / session / channel / chat)
// says where it runs and where the reply is delivered; owner gates who may manage it.
type ScheduledTask struct {
	ID           string
	Name         string
	OwnerUserID  int64
	AgentGroupID int64
	SessionKey   string
	Channel      string
	ChatID       string
	PeriodDays   int
	AtHour       int   // 0-23, or <0 to use EverySeconds
	EverySeconds int64 // used only when AtHour < 0
	Prompt       string
	Enabled      bool
}

// ErrTaskExists is returned when an owner already has a task with the same name.
var ErrTaskExists = errors.New("db: a scheduled task with that name already exists")

// CreateScheduledTask inserts a new task, assigning a UUID. Fails with ErrTaskExists if
// the owner already has a task by that name (the per-owner unique index).
func (d *DB) CreateScheduledTask(t ScheduledTask) (string, error) {
	id := uuid.NewString()
	_, err := d.Exec(`
		INSERT INTO scheduled_tasks
			(id, name, owner_user_id, agent_group_id, session_key, channel, chat_id,
			 period_days, at_hour, every_seconds, prompt, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, t.Name, t.OwnerUserID, t.AgentGroupID, t.SessionKey, t.Channel, t.ChatID,
		t.PeriodDays, t.AtHour, t.EverySeconds, t.Prompt, boolToInt(t.Enabled))
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrTaskExists
		}
		return "", fmt.Errorf("db: create scheduled task: %w", err)
	}
	return id, nil
}

// EnabledScheduledTasks returns every enabled task, for the scheduler to evaluate.
func (d *DB) EnabledScheduledTasks() ([]ScheduledTask, error) {
	return d.queryScheduledTasks(`WHERE enabled = 1 ORDER BY id`)
}

// ScheduledTasksByOwner returns an owner's tasks (enabled and disabled), for listing.
func (d *DB) ScheduledTasksByOwner(ownerUserID int64) ([]ScheduledTask, error) {
	return d.queryScheduledTasks(`WHERE owner_user_id = ? ORDER BY name`, ownerUserID)
}

func (d *DB) queryScheduledTasks(where string, args ...any) ([]ScheduledTask, error) {
	rows, err := d.Query(`
		SELECT id, name, owner_user_id, agent_group_id, session_key, channel, chat_id,
			   period_days, at_hour, every_seconds, prompt, enabled
		FROM scheduled_tasks `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query scheduled tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		var enabled int
		if err := rows.Scan(&t.ID, &t.Name, &t.OwnerUserID, &t.AgentGroupID, &t.SessionKey,
			&t.Channel, &t.ChatID, &t.PeriodDays, &t.AtHour, &t.EverySeconds, &t.Prompt, &enabled); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteScheduledTaskByName removes an owner's task by name. Returns whether a row
// matched (so the caller can report "no such task"). Scoped to owner so one user cannot
// delete another's task by name.
func (d *DB) DeleteScheduledTaskByName(ownerUserID int64, name string) (bool, error) {
	res, err := d.Exec(`DELETE FROM scheduled_tasks WHERE owner_user_id = ? AND name = ?`, ownerUserID, name)
	if err != nil {
		return false, fmt.Errorf("db: delete scheduled task: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetScheduledTaskEnabledByName pauses/resumes an owner's task by name. Returns whether
// a row matched.
func (d *DB) SetScheduledTaskEnabledByName(ownerUserID int64, name string, enabled bool) (bool, error) {
	res, err := d.Exec(`UPDATE scheduled_tasks SET enabled = ? WHERE owner_user_id = ? AND name = ?`,
		boolToInt(enabled), ownerUserID, name)
	if err != nil {
		return false, fmt.Errorf("db: set scheduled task enabled: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountScheduledTasksByOwner returns how many tasks an owner has (for a per-owner cap).
func (d *DB) CountScheduledTasksByOwner(ownerUserID int64) (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM scheduled_tasks WHERE owner_user_id = ?`, ownerUserID).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("db: count scheduled tasks: %w", err)
	}
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure. The
// modernc.org/sqlite driver surfaces it in the error text ("UNIQUE constraint failed:
// ..."), so match on that rather than depending on the driver's concrete error type.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
