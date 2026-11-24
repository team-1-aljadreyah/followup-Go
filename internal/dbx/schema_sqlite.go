package dbx

import (
	"context"
	"fmt"
	"strings"
)

// BootstrapSQLite creates the complete FollowUP schema on an in-memory/file SQLite DB for testing.
func (d *DB) BootstrapSQLite(ctx context.Context) error {
	if !d.IsSQLite() {
		return nil
	}
	for _, stmt := range sqliteStatements {
		if _, err := d.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap sqlite stmt failed: %w\n%s", err, stmt)
		}
	}
	// Baseline rows matching BaselineSeeder
	_, _ = d.ExecContext(ctx, `INSERT OR IGNORE INTO code_sequences (name, last_value) VALUES ('project', 0), ('task', 0), ('bug', 0)`)
	_, _ = d.ExecContext(ctx, `INSERT OR IGNORE INTO app_settings (id, due_soon_days, lock_after, lock_minutes, created_at, updated_at) VALUES (1, 1, 5, 15, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	_, _ = d.ExecContext(ctx, `INSERT OR IGNORE INTO performance_criteria (id, items, updated_at) VALUES (1, '[]', CURRENT_TIMESTAMP)`)
	return nil
}

// InInt64 builds ("?,?...", args) safely.
func InInt64(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "NULL", nil
	}
	return Placeholders(len(ids)), Int64Args(ids)
}

// NextCode atomically increments code_sequences inside the active transaction/DB.
func NextCode(ctx context.Context, db *DB, q Queryer, kind string, customPrefix ...string) (string, error) {
	if _, err := q.ExecContext(ctx, db.InsertIgnore()+` INTO code_sequences (name, last_value) VALUES (?, 0)`, kind); err != nil {
		return "", err
	}
	var current int64
	if err := q.QueryRowContext(ctx, `SELECT last_value FROM code_sequences WHERE name = ?`+db.ForUpdate(), kind).Scan(&current); err != nil {
		return "", err
	}
	next := current + 1
	if _, err := q.ExecContext(ctx, `UPDATE code_sequences SET last_value = ? WHERE name = ?`, next, kind); err != nil {
		return "", err
	}
	prefix := map[string]string{"project": "PRJ", "task": "TSK", "bug": "BUG"}[kind]
	if len(customPrefix) > 0 && strings.TrimSpace(customPrefix[0]) != "" {
		prefix = strings.TrimSpace(customPrefix[0])
	}
	if kind == "task" {
		return fmt.Sprintf("%s-%d", prefix, 1000+next), nil
	}
	return fmt.Sprintf("%s-%d", prefix, next), nil
}

var sqliteStatements = []string{
	`CREATE TABLE IF NOT EXISTS teams (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		name_en TEXT,
		lead_id INTEGER,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		name_en TEXT,
		email TEXT NOT NULL UNIQUE,
		phone TEXT,
		role TEXT NOT NULL,
		custom_role TEXT,
		birth_date TEXT,
		preferred_locale TEXT NOT NULL DEFAULT 'ar',
		title TEXT NOT NULL DEFAULT '',
		team_id INTEGER REFERENCES teams(id) ON DELETE SET NULL,
		active INTEGER NOT NULL DEFAULT 1,
		password TEXT NOT NULL,
		remember_token TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS personal_access_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tokenable_type TEXT NOT NULL,
		tokenable_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		token TEXT NOT NULL UNIQUE,
		abilities TEXT,
		session_id TEXT,
		last_used_at TEXT,
		expires_at TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS app_settings (
		id INTEGER PRIMARY KEY,
		due_soon_days INTEGER NOT NULL DEFAULT 1,
		lock_after INTEGER NOT NULL DEFAULT 5,
		lock_minutes INTEGER NOT NULL DEFAULT 15,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS code_sequences (
		name TEXT PRIMARY KEY,
		last_value INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS login_security (
		user_id INTEGER PRIMARY KEY REFERENCES users(id),
		failed_logins INTEGER NOT NULL DEFAULT 0,
		locked_until TEXT,
		last_login_at TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS auth_sessions (
		id TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_id INTEGER,
		ip TEXT,
		user_agent TEXT,
		started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_seen_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		ended_at TEXT,
		ended_by INTEGER,
		end_reason TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS avatars (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		disk_path TEXT NOT NULL,
		mime TEXT NOT NULL,
		size INTEGER NOT NULL,
		width INTEGER NOT NULL,
		height INTEGER NOT NULL,
		cache_key TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		name_en TEXT,
		description TEXT NOT NULL,
		pm_id INTEGER NOT NULL REFERENCES users(id),
		start TEXT NOT NULL,
		"end" TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'active',
		priority TEXT NOT NULL DEFAULT 'medium',
		uses_sprints INTEGER NOT NULL DEFAULT 0,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS project_members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		user_id INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(project_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS project_statuses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		"key" TEXT NOT NULL,
		name TEXT NOT NULL,
		name_en TEXT NOT NULL,
		tone TEXT NOT NULL,
		position INTEGER NOT NULL DEFAULT 0,
		is_base INTEGER NOT NULL DEFAULT 0,
		created_at TEXT,
		updated_at TEXT,
		UNIQUE(project_id, "key"),
		UNIQUE(project_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS project_phases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		name TEXT NOT NULL,
		position INTEGER NOT NULL DEFAULT 0,
		created_at TEXT,
		updated_at TEXT,
		UNIQUE(project_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS sprints (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		name TEXT NOT NULL,
		start TEXT NOT NULL,
		"end" TEXT NOT NULL,
		closed_at TEXT,
		result TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS project_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		actor_id INTEGER NOT NULL REFERENCES users(id),
		kind TEXT NOT NULL,
		"from" TEXT,
		"to" TEXT,
		note TEXT,
		at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS project_templates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		description TEXT NOT NULL,
		statuses TEXT NOT NULL,
		phases TEXT NOT NULL,
		uses_sprints INTEGER NOT NULL DEFAULT 0,
		tasks TEXT NOT NULL,
		from_project TEXT,
		created_by INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		phase_id INTEGER REFERENCES project_phases(id) ON DELETE SET NULL,
		sprint_id INTEGER REFERENCES sprints(id) ON DELETE SET NULL,
		title TEXT NOT NULL,
		description TEXT NOT NULL,
		assignee_id INTEGER REFERENCES users(id),
		start TEXT,
		due TEXT,
		priority TEXT NOT NULL DEFAULT 'medium',
		status TEXT NOT NULL,
		blocked_reason TEXT,
		subtasks_done INTEGER NOT NULL DEFAULT 0,
		subtasks_total INTEGER NOT NULL DEFAULT 0,
		created_by INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS task_dependencies (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL REFERENCES tasks(id),
		depends_on_task_id INTEGER NOT NULL REFERENCES tasks(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(task_id, depends_on_task_id)
	)`,
	`CREATE TABLE IF NOT EXISTS task_approvals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL REFERENCES tasks(id),
		state TEXT NOT NULL DEFAULT 'pending',
		from_status TEXT NOT NULL,
		to_status TEXT NOT NULL,
		requested_by INTEGER NOT NULL REFERENCES users(id),
		requested_at TEXT NOT NULL,
		decided_by INTEGER REFERENCES users(id),
		decided_at TEXT,
		note TEXT,
		is_current INTEGER NOT NULL DEFAULT 1,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS task_comments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL REFERENCES tasks(id),
		author_id INTEGER NOT NULL REFERENCES users(id),
		body TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS task_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id INTEGER NOT NULL REFERENCES tasks(id),
		actor_id INTEGER NOT NULL REFERENCES users(id),
		kind TEXT NOT NULL,
		"from" TEXT,
		"to" TEXT,
		note TEXT,
		at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS bugs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		project_id INTEGER NOT NULL REFERENCES projects(id),
		phase_id INTEGER REFERENCES project_phases(id) ON DELETE SET NULL,
		task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
		title TEXT NOT NULL,
		description TEXT NOT NULL,
		severity TEXT NOT NULL,
		priority TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'reported',
		reported_by INTEGER NOT NULL REFERENCES users(id),
		assignee_id INTEGER REFERENCES users(id),
		due TEXT,
		closed_at TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS bug_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bug_id INTEGER NOT NULL REFERENCES bugs(id),
		actor_id INTEGER NOT NULL REFERENCES users(id),
		kind TEXT NOT NULL,
		"from" TEXT,
		"to" TEXT,
		note TEXT,
		at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS attachments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		owner_type TEXT NOT NULL,
		owner_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		size INTEGER NOT NULL,
		mime TEXT NOT NULL,
		voice TEXT,
		disk_path TEXT NOT NULL,
		uploaded_by INTEGER NOT NULL REFERENCES users(id),
		at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS announcements (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		important INTEGER NOT NULL DEFAULT 0,
		author_id INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS announcement_reads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		announcement_id INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id),
		at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(announcement_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS direct_conversations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_low_id INTEGER NOT NULL REFERENCES users(id),
		user_high_id INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_low_id, user_high_id)
	)`,
	`CREATE TABLE IF NOT EXISTS chat_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		created_by INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS chat_group_members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		group_id INTEGER NOT NULL REFERENCES chat_groups(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id),
		position INTEGER NOT NULL DEFAULT 0,
		UNIQUE(group_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_key TEXT NOT NULL,
		author_id INTEGER NOT NULL REFERENCES users(id),
		body TEXT NOT NULL,
		reply_to_id INTEGER REFERENCES messages(id),
		mentions TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS chat_reads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_key TEXT NOT NULL,
		user_id INTEGER NOT NULL REFERENCES users(id),
		last_message_id INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(chat_key, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id),
		kind TEXT NOT NULL,
		actor_id INTEGER REFERENCES users(id),
		project_id INTEGER,
		task_id INTEGER,
		announcement_id INTEGER,
		bug_id INTEGER,
		chat_key TEXT,
		team_id INTEGER REFERENCES teams(id) ON DELETE SET NULL,
		data TEXT NOT NULL DEFAULT '{}',
		dedup_key TEXT UNIQUE,
		at TEXT NOT NULL,
		read INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS notification_preferences (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id),
		kind TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT,
		updated_at TEXT,
		UNIQUE(user_id, kind)
	)`,
	`CREATE TABLE IF NOT EXISTS push_subscriptions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		session_id TEXT,
		endpoint_hash TEXT NOT NULL UNIQUE,
		endpoint TEXT NOT NULL,
		p256dh TEXT NOT NULL,
		auth TEXT NOT NULL,
		device TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS performance_criteria (
		id INTEGER PRIMARY KEY,
		items TEXT NOT NULL DEFAULT '[]',
		approved_by INTEGER REFERENCES users(id),
		approved_at TEXT,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS performance_reviews (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id),
		reviewer_id INTEGER NOT NULL REFERENCES users(id),
		period_from TEXT NOT NULL,
		period_to TEXT NOT NULL,
		scores TEXT NOT NULL DEFAULT '[]',
		criteria TEXT NOT NULL DEFAULT '[]',
		total INTEGER NOT NULL,
		note TEXT NOT NULL,
		perf TEXT NOT NULL DEFAULT '{}',
		state TEXT NOT NULL DEFAULT 'draft',
		finalized_at TEXT,
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS performance_review_scores (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		review_id INTEGER NOT NULL REFERENCES performance_reviews(id) ON DELETE CASCADE,
		criterion_id TEXT NOT NULL,
		criterion_name TEXT NOT NULL,
		criterion_name_en TEXT NOT NULL,
		kind TEXT NOT NULL,
		metric TEXT,
		weight INTEGER NOT NULL,
		stars INTEGER,
		score INTEGER NOT NULL,
		UNIQUE(review_id, criterion_id)
	)`,
	`CREATE TABLE IF NOT EXISTS monitor_notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		about_type TEXT NOT NULL,
		about_id INTEGER,
		about_label TEXT,
		about_label_en TEXT,
		tone TEXT,
		subject TEXT,
		people TEXT,
		author_id INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		sent_at TEXT,
		ceo_state TEXT,
		ceo_reply TEXT,
		replied_at TEXT,
		facts TEXT,
		triage TEXT,
		forward_note TEXT,
		monitor_reply TEXT,
		triaged_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
		triaged_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS monitor_note_shares (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		note_id INTEGER NOT NULL REFERENCES monitor_notes(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		label_en TEXT,
		shared_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		seen_at TEXT,
		UNIQUE(note_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS ratings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subject_type TEXT NOT NULL,
		subject_id INTEGER NOT NULL,
		rater_id INTEGER NOT NULL REFERENCES users(id),
		score INTEGER NOT NULL,
		reason_type TEXT NOT NULL,
		reason TEXT NOT NULL,
		project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
		project_name TEXT,
		project_name_en TEXT,
		period_from TEXT,
		period_to TEXT,
		private INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		edited_at TEXT,
		edited_by INTEGER REFERENCES users(id) ON DELETE SET NULL
	)`,
	`CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		actor_id INTEGER REFERENCES users(id),
		action TEXT NOT NULL,
		target_type TEXT NOT NULL,
		target_id INTEGER,
		target_label TEXT NOT NULL,
		changes TEXT,
		at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		ip TEXT,
		user_agent TEXT,
		session_id TEXT,
		request_id TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS advanced_settings (
		id INTEGER PRIMARY KEY,
		rules TEXT NOT NULL,
		updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS advanced_settings_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		by_id INTEGER NOT NULL REFERENCES users(id),
		by_name TEXT NOT NULL,
		by_name_en TEXT,
		how TEXT,
		changes TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS project_github_settings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER NOT NULL UNIQUE REFERENCES projects(id),
		enabled INTEGER NOT NULL DEFAULT 0,
		repos TEXT NOT NULL DEFAULT '[]',
		created_at TEXT,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS github_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		external_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		repo TEXT NOT NULL,
		ref TEXT NOT NULL,
		title TEXT NOT NULL,
		state TEXT,
		author TEXT NOT NULL,
		at TEXT NOT NULL,
		url TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(repo, external_id)
	)`,
	`CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		delivery_id TEXT NOT NULL UNIQUE,
		event TEXT NOT NULL,
		received_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS celebrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		team_id INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		message TEXT,
		created_by INTEGER NOT NULL REFERENCES users(id),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS celebration_recipients (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		celebration_id INTEGER NOT NULL REFERENCES celebrations(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		seen_at TEXT,
		UNIQUE(celebration_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS trial_teams (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		goal TEXT,
		started_on TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		ended_on TEXT,
		created_by INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS trial_members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		team_id INTEGER NOT NULL REFERENCES trial_teams(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		specialty TEXT NOT NULL,
		phone TEXT,
		email TEXT,
		joined_on TEXT NOT NULL,
		decision TEXT NOT NULL DEFAULT 'pending',
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS trial_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		member_id INTEGER NOT NULL REFERENCES trial_members(id) ON DELETE CASCADE,
		score INTEGER,
		text TEXT,
		by_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
}
