package bugs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

var Severities = []string{"critical", "high", "medium", "low"}
var Priorities = []string{"high", "medium", "low"}

var nextTransitions = map[string][]string{
	"reported": {"assigned"},
	"assigned": {"fixing"},
	"fixing":   {"testing"},
	"testing":  {"closed", "reopened"},
	"reopened": {"fixing"},
	"closed":   {"reopened"},
}

type Service struct {
	db            *dbx.DB
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, notifications: notifSvc, advanced: advSvc}
}

type bugRow struct {
	id          int64
	code        string
	projectID   int64
	phaseID     *int64
	taskID      *int64
	title       string
	description string
	severity    string
	priority    string
	status      string
	reportedBy  int64
	assigneeID  *int64
	due         *string
	createdAt   *time.Time
	updatedAt   *time.Time
	closedAt    *time.Time
}

func (b *bugRow) toJSON() map[string]any {
	return store.CleanMap(map[string]any{
		"id":          b.id,
		"code":        b.code,
		"project_id":  b.projectID,
		"phase_id":    b.phaseID,
		"task_id":     b.taskID,
		"title":       b.title,
		"description": b.description,
		"severity":    b.severity,
		"priority":    b.priority,
		"status":      b.status,
		"reported_by": b.reportedBy,
		"assignee_id": b.assigneeID,
		"due":         b.due,
		"created_at":  store.IsoTime(b.createdAt),
		"updated_at":  store.IsoTime(b.updatedAt),
		"closed_at":   store.IsoTime(b.closedAt),
	})
}

func (s *Service) HandleIndex(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "bugs"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	projs, err := access.LoadAllProjects(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var visIDs []int64
	for _, p := range projs {
		if access.CanSeeProject(u, p) {
			visIDs = append(visIDs, p.ID)
		}
	}
	if len(visIDs) == 0 {
		httpx.Success(w, []any{})
		return
	}
	ph, args := dbx.InInt64(visIDs)
	list, err := s.loadBugs(r.Context(), fmt.Sprintf(`WHERE project_id IN (%s) ORDER BY id ASC`, ph), args...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, b.toJSON())
	}
	httpx.Success(w, out)
}

func (s *Service) HandleListForProject(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "bugs"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	pid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(r.Context(), s.db, u, pid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	list, err := s.loadBugs(r.Context(), `WHERE project_id = ? ORDER BY id ASC`, pid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, b := range list {
		out = append(out, b.toJSON())
	}
	httpx.Success(w, out)
}

func (s *Service) HandleShow(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	b, _, err := s.bugForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, b.toJSON())
}

func (s *Service) HandleReport(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "bugs"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	pid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := access.ProjectForOrFail(r.Context(), s.db, u, pid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanTakePart(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.bugs.report_forbidden"))
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "report_bugs"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	fields, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if strVal(fields["severity"]) == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("severity", "api.bugs.severity_invalid"))
		return
	}
	if !access.CanManageBugs(u, p) {
		delete(fields, "assignee_id")
		delete(fields, "due")
	}
	if err := s.checkFields(r.Context(), p, fields, true); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	assigneeID := optInt64(fields["assignee_id"])
	status := "reported"
	if assigneeID != nil {
		status = "assigned"
	}
	title := strings.TrimSpace(strVal(fields["title"]))
	desc := strings.TrimSpace(strVal(fields["description"]))
	sev := strVal(fields["severity"])
	pri := strVal(fields["priority"])
	if pri == "" {
		pri = "medium"
	}
	phaseID := optInt64(fields["phase_id"])
	taskID := optInt64(fields["task_id"])
	due := optStr(fields["due"])
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	bugPrefix := s.advanced.CodePrefix(r.Context(), "bug")

	var bugID int64
	var code string
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		c, err := dbx.NextCode(r.Context(), s.db, tx, "bug", bugPrefix)
		if err != nil {
			return err
		}
		code = c
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO bugs (code, project_id, phase_id, task_id, title, description, severity, priority, status, reported_by, assignee_id, due, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, code, p.ID, phaseID, taskID, title, desc, sev, pri, status, u.ID, assigneeID, due, nowStr, nowStr)
		if err != nil {
			return err
		}
		bugID, _ = res.LastInsertId()
		_, err = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, at) VALUES (?, ?, 'created', ?)`, bugID, u.ID, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	bData := notifications.AboutProject(p.Name, p.NameEn)
	bData["bug_code"] = code
	bData["bug_title"] = title
	bData["severity"] = sev
	about := notifications.About{
		ActorID:   &u.ID,
		ProjectID: &p.ID,
		BugID:     &bugID,
		Data:      bData,
	}
	s.notifications.Notify(r.Context(), s.managerIDs(r.Context(), p), "bug_reported", about)
	if assigneeID != nil {
		s.notifications.Notify(r.Context(), []int64{*assigneeID}, "bug_assigned", about)
	}

	created, _, _ := s.bugForOrFail(r.Context(), u, bugID)
	httpx.Success(w, created.toJSON(), http.StatusCreated)
}

func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	b, p, err := s.bugForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanManageBugs(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.bugs.edit_forbidden"))
		return
	}
	patch, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := s.checkFields(r.Context(), p, patch, false); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var notifyAssignee *int64

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if sv := strVal(patch["severity"]); sv != "" && sv != b.severity {
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'severity', ?, ?, ?)`,
				b.id, u.ID, b.severity, sv, nowStr)
			b.severity = sv
		}
		if pr := strVal(patch["priority"]); pr != "" && pr != b.priority {
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'priority', ?, ?, ?)`,
				b.id, u.ID, b.priority, pr, nowStr)
			b.priority = pr
		}
		if hasKey(patch, "due") {
			due := optStr(patch["due"])
			if !eqStrPtr(due, b.due) {
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'due', ?, ?, ?)`,
					b.id, u.ID, b.due, due, nowStr)
				b.due = due
			}
		}
		if hasKey(patch, "assignee_id") {
			nextA := optInt64(patch["assignee_id"])
			if !eqInt64Ptr(nextA, b.assigneeID) {
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'assignee', ?, ?, ?)`,
					b.id, u.ID, int64PtrStr(b.assigneeID), int64PtrStr(nextA), nowStr)
				b.assigneeID = nextA
				if nextA != nil {
					notifyAssignee = nextA
					if b.status == "reported" {
						_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'status', 'reported', 'assigned', ?)`,
							b.id, u.ID, nowStr)
						b.status = "assigned"
					}
				}
			}
		}

		var edited []string
		if hasKey(patch, "title") {
			t := strings.TrimSpace(strVal(patch["title"]))
			if t != b.title {
				b.title = t
				edited = append(edited, "title")
			}
		}
		if hasKey(patch, "description") {
			d := strVal(patch["description"])
			if d != b.description {
				b.description = d
				edited = append(edited, "description")
			}
		}
		if len(edited) > 0 {
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`to`"+`, at) VALUES (?, ?, 'edited', ?, ?)`,
				b.id, u.ID, strings.Join(edited, ","), nowStr)
		}
		if hasKey(patch, "phase_id") {
			b.phaseID = optInt64(patch["phase_id"])
		}
		if hasKey(patch, "task_id") {
			b.taskID = optInt64(patch["task_id"])
		}

		_, err := tx.ExecContext(r.Context(), `
			UPDATE bugs SET severity = ?, priority = ?, due = ?, assignee_id = ?, status = ?, title = ?, description = ?, phase_id = ?, task_id = ?, updated_at = ?
			WHERE id = ?
		`, b.severity, b.priority, b.due, b.assigneeID, b.status, b.title, b.description, b.phaseID, b.taskID, nowStr, b.id)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if notifyAssignee != nil {
		s.notifications.Notify(r.Context(), []int64{*notifyAssignee}, "bug_assigned", notifications.About{
			ActorID: &u.ID, ProjectID: &p.ID, BugID: &b.id, Data: map[string]any{"bug_code": b.code, "bug_title": b.title},
		})
	}

	updated, _, _ := s.bugForOrFail(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleMove(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	b, p, err := s.bugForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	toStatus := strVal(body["status"])
	if !store.UserHasRole(toStatus, nextTransitions[b.status]) {
		httpx.HandleErr(w, r, httpx.ErrValidation("status", "api.bugs.status_invalid_transition"))
		return
	}
	allowed := access.BugNextStatuses(u, p, b.status, b.reportedBy, b.assigneeID)
	if !store.UserHasRole(toStatus, allowed) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.bugs.move_forbidden"))
		return
	}
	if toStatus == "assigned" && b.assigneeID == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("assignee_id", "api.bugs.assignee_required"))
		return
	}
	var reason *string
	if nt := strings.TrimSpace(strVal(body["note"])); nt != "" {
		if support.RuneLen(nt) > 300 {
			httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.bugs.note_max"))
			return
		}
		reason = &nt
	}
	if toStatus == "reopened" && reason == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.bugs.reopen_note_required"))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var closedAt *string
	if toStatus == "closed" {
		closedAt = &nowStr
	}

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE bugs SET status = ?, closed_at = ?, updated_at = ? WHERE id = ?`, toStatus, closedAt, nowStr, b.id); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'status', ?, ?, ?, ?)`,
			b.id, u.ID, b.status, toStatus, reason, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	recips := []int64{b.reportedBy, p.PMID}
	if b.assigneeID != nil {
		recips = append(recips, *b.assigneeID)
	}
	data := notifications.AboutProject(p.Name, p.NameEn)
	data["bug_code"] = b.code
	data["bug_title"] = b.title
	data["bug_status"] = toStatus
	if reason != nil {
		data["note"] = *reason
	}
	s.notifications.Notify(r.Context(), recips, "bug_status", notifications.About{
		ActorID: &u.ID, ProjectID: &p.ID, BugID: &b.id, Data: data,
	})

	updated, _, _ := s.bugForOrFail(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleHistory(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, _, err := s.bugForOrFail(r.Context(), u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at FROM bug_events WHERE bug_id = ? ORDER BY id ASC`, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var eid, bid, aid int64
		var kind string
		var from, to, note sql.NullString
		var atRaw any
		if rows.Scan(&eid, &bid, &aid, &kind, &from, &to, &note, &atRaw) == nil {
			m := map[string]any{"id": eid, "bug_id": bid, "actor_id": aid, "kind": kind, "at": store.IsoTime(dbx.ParseTime(atRaw))}
			if from.Valid {
				m["from"] = from.String
			}
			if to.Valid {
				m["to"] = to.String
			}
			if note.Valid {
				m["note"] = note.String
			}
			out = append(out, m)
		}
	}
	httpx.Success(w, out)
}

func (s *Service) bugForOrFail(ctx context.Context, u *store.User, id int64) (*bugRow, *access.Project, error) {
	list, err := s.loadBugs(ctx, `WHERE id = ?`, id)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return nil, nil, httpx.ErrNotFound("api.bugs.not_found")
	}
	b := list[0]
	p, err := access.LoadProject(ctx, s.db, b.projectID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil || !access.CanSeeProject(u, p) {
		return nil, nil, httpx.ErrNotFound("api.bugs.not_found")
	}
	return b, p, nil
}

func (s *Service) loadBugs(ctx context.Context, whereSQL string, args ...any) ([]*bugRow, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, code, project_id, phase_id, task_id, title, description, severity, priority, status, reported_by, assignee_id, due, created_at, updated_at, closed_at
		FROM bugs %s
	`, whereSQL), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*bugRow
	for rows.Next() {
		var b bugRow
		var phID, tID, assID sql.NullInt64
		var dueRaw, crRaw, upRaw, clRaw any
		if err := rows.Scan(&b.id, &b.code, &b.projectID, &phID, &tID, &b.title, &b.description, &b.severity, &b.priority, &b.status, &b.reportedBy, &assID, &dueRaw, &crRaw, &upRaw, &clRaw); err != nil {
			return nil, err
		}
		if phID.Valid {
			v := phID.Int64
			b.phaseID = &v
		}
		if tID.Valid {
			v := tID.Int64
			b.taskID = &v
		}
		if assID.Valid {
			v := assID.Int64
			b.assigneeID = &v
		}
		b.due = dbx.ParseDateOnly(dueRaw)
		b.createdAt = dbx.ParseTime(crRaw)
		b.updatedAt = dbx.ParseTime(upRaw)
		b.closedAt = dbx.ParseTime(clRaw)
		list = append(list, &b)
	}
	return list, rows.Err()
}

func (s *Service) checkFields(ctx context.Context, p *access.Project, fields map[string]any, isCreate bool) error {
	if isCreate || hasKey(fields, "title") {
		title := strings.TrimSpace(strVal(fields["title"]))
		if title == "" {
			return httpx.ErrValidation("title", "api.bugs.title_required")
		}
		if support.RuneLen(title) > 160 {
			return httpx.ErrValidation("title", "api.bugs.title_max")
		}
	}
	if hasKey(fields, "description") && support.RuneLen(strVal(fields["description"])) > 5000 {
		return httpx.ErrValidation("description", "api.bugs.description_max")
	}
	if hasKey(fields, "severity") && !store.UserHasRole(strVal(fields["severity"]), Severities) {
		return httpx.ErrValidation("severity", "api.bugs.severity_invalid")
	}
	if hasKey(fields, "priority") && !store.UserHasRole(strVal(fields["priority"]), Priorities) {
		return httpx.ErrValidation("priority", "api.bugs.priority_invalid")
	}
	if aid := optInt64(fields["assignee_id"]); aid != nil && !p.IsMember(*aid) {
		return httpx.ErrValidation("assignee_id", "api.bugs.assignee_not_member")
	}
	if hasKey(fields, "due") {
		if d := optStr(fields["due"]); d != nil && !support.IsRealDay(*d) {
			return httpx.ErrValidation("due", "api.bugs.due_invalid")
		}
	}
	if phID := optInt64(fields["phase_id"]); phID != nil && !p.HasPhase(*phID) {
		return httpx.ErrValidation("phase_id", "api.bugs.phase_invalid")
	}
	if tID := optInt64(fields["task_id"]); tID != nil {
		var ok int
		_ = s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ? AND project_id = ? LIMIT 1`, *tID, p.ID).Scan(&ok)
		if ok != 1 {
			return httpx.ErrValidation("task_id", "api.bugs.task_invalid")
		}
	}
	return nil
}

func (s *Service) managerIDs(ctx context.Context, p *access.Project) []int64 {
	var recips []int64
	recips = append(recips, p.PMID)
	rows, err := s.db.QueryContext(ctx, `SELECT id, role FROM users WHERE active = 1`)
	if err == nil {
		for rows.Next() {
			var uid int64
			var role string
			if rows.Scan(&uid, &role) == nil {
				if role == "ceo" || role == "monitor" || (role == "lead" && p.IsMember(uid)) {
					recips = append(recips, uid)
				}
			}
		}
		_ = rows.Close()
	}
	seen := map[int64]bool{}
	var uniq []int64
	for _, id := range recips {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return uniq
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }
func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func optStr(v any) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" {
		return nil
	}
	return &s
}
func optInt64(v any) *int64 {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case int64:
		if t > 0 {
			return &t
		}
	case int:
		n := int64(t)
		if n > 0 {
			return &n
		}
	case float64:
		n := int64(t)
		if n > 0 {
			return &n
		}
	case json.Number:
		if n, err := t.Int64(); err == nil && n > 0 {
			return &n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil && n > 0 {
			return &n
		}
	}
	return nil
}
func eqInt64Ptr(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
func eqStrPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
func int64PtrStr(p *int64) *string {
	if p == nil {
		return nil
	}
	s := strconv.FormatInt(*p, 10)
	return &s
}
