package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

var Priorities = []string{"high", "medium", "low"}

type Service struct {
	db            *dbx.DB
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, notifications: notifSvc, advanced: advSvc}
}

type taskRow struct {
	id            int64
	code          string
	projectID     int64
	phaseID       *int64
	sprintID      *int64
	title         string
	description   string
	assigneeID    *int64
	start         *string
	due           *string
	priority      string
	status        string
	blockedReason *string
	subtasksDone  int
	subtasksTotal int
	createdBy     *int64
	createdAt     *time.Time
	updatedAt     *time.Time
	dependsOn     []int64
	approval      map[string]any
}

func (t *taskRow) toJSON() map[string]any {
	m := map[string]any{
		"id":             t.id,
		"code":           t.code,
		"project_id":     t.projectID,
		"phase_id":       t.phaseID,
		"sprint_id":      t.sprintID,
		"title":          t.title,
		"description":    t.description,
		"assignee_id":    t.assigneeID,
		"start":          t.start,
		"due":            t.due,
		"priority":       t.priority,
		"status":         t.status,
		"blocked_reason": t.blockedReason,
		"approval":       t.approval,
		"subtasks_done":  t.subtasksDone,
		"subtasks_total": t.subtasksTotal,
		"created_by":     t.createdBy,
		"created_at":     store.IsoTime(t.createdAt),
		"updated_at":     store.IsoTime(t.updatedAt),
	}
	if len(t.dependsOn) > 0 {
		m["depends_on"] = t.dependsOn
	}
	return store.CleanMap(m)
}

func (s *Service) HandleListForProject(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	pid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(r.Context(), s.db, u, pid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	list, err := s.loadTasks(r.Context(), `WHERE project_id = ? ORDER BY id ASC`, pid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, t.toJSON())
	}
	httpx.Success(w, out)
}

func (s *Service) HandleMyTasks(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	allProjs, err := access.LoadAllProjects(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var visIDs []int64
	for _, p := range allProjs {
		if access.CanSeeProject(u, p) {
			visIDs = append(visIDs, p.ID)
		}
	}
	if len(visIDs) == 0 {
		httpx.Success(w, []any{})
		return
	}
	ph, args := dbx.InInt64(visIDs)
	allArgs := append([]any{u.ID}, args...)
	list, err := s.loadTasks(r.Context(), fmt.Sprintf(`WHERE assignee_id = ? AND project_id IN (%s) ORDER BY id ASC`, ph), allArgs...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, t.toJSON())
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
	t, _, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, t.toJSON())
}

func (s *Service) HandleCreate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
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
	if !access.CanManageTasks(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.create_forbidden"))
		return
	}
	fields, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := s.checkFields(r.Context(), p, fields, true); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	statusKey := ""
	if len(p.Statuses) > 0 {
		statusKey = p.Statuses[0].Key
	}
	if sk := strVal(fields["status"]); sk != "" {
		statusKey = sk
	}
	target := p.FindStatus(statusKey)
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("status", "api.tasks.status_unknown"))
		return
	}
	if target.Tone == "blocked" || target.Tone == "done" {
		httpx.HandleErr(w, r, httpx.ErrValidation("status", "api.tasks.status_start_invalid"))
		return
	}

	var taskID int64
	var taskCode, taskTitle string
	var assigneeID *int64
	if aid := optInt64(fields["assignee_id"]); aid != nil {
		assigneeID = aid
	}
	var sprintID *int64
	if p.UsesSprints {
		sprintID = optInt64(fields["sprint_id"])
	}
	phaseID := optInt64(fields["phase_id"])
	due := optStr(fields["due"])
	start := optStr(fields["start"])
	priority := strVal(fields["priority"])
	if priority == "" {
		priority = "medium"
	}
	taskTitle = strings.TrimSpace(strVal(fields["title"]))
	desc := strings.TrimSpace(strVal(fields["description"]))
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	taskPrefix := s.advanced.CodePrefix(r.Context(), "task")

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		code, err := dbx.NextCode(r.Context(), s.db, tx, "task", taskPrefix)
		if err != nil {
			return err
		}
		taskCode = code
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO tasks (code, project_id, phase_id, sprint_id, title, description, assignee_id, start, due, priority, status, subtasks_done, subtasks_total, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)
		`, taskCode, p.ID, phaseID, sprintID, taskTitle, desc, assigneeID, start, due, priority, statusKey, u.ID, nowStr, nowStr)
		if err != nil {
			return err
		}
		taskID, _ = res.LastInsertId()
		_, err = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, at) VALUES (?, ?, 'created', ?)`, taskID, u.ID, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if assigneeID != nil {
		s.notifications.Notify(r.Context(), []int64{*assigneeID}, "task_assigned", notifications.About{
			ActorID:   &u.ID,
			ProjectID: &p.ID,
			TaskID:    &taskID,
			Data:      notifications.AboutTask(taskCode, taskTitle, p.Name, p.NameEn),
		})
	}

	created, _, _ := s.taskForOrFail(r.Context(), u, taskID)
	httpx.Success(w, created.toJSON(), http.StatusCreated)
}

func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, p, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanManageTasks(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.edit_forbidden"))
		return
	}
	patch, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if _, hasA := patch["assignee_id"]; hasA {
		nextA := optInt64(patch["assignee_id"])
		if !eqInt64Ptr(nextA, t.assigneeID) {
			if err := s.advanced.RequireAction(r.Context(), u, "reassign_task"); err != nil {
				httpx.HandleErr(w, r, err)
				return
			}
		}
	}
	if _, hasDue := patch["due"]; hasDue {
		nextD := optStr(patch["due"])
		if !eqStrPtr(nextD, t.due) {
			if err := s.advanced.RequireAction(r.Context(), u, "change_due"); err != nil {
				httpx.HandleErr(w, r, err)
				return
			}
		}
	}

	chk := map[string]any{}
	for k, v := range patch {
		chk[k] = v
	}
	if _, ok := chk["start"]; !ok && t.start != nil {
		chk["start"] = *t.start
	}
	if _, ok := chk["due"]; !ok && t.due != nil {
		chk["due"] = *t.due
	}
	if err := s.checkFields(r.Context(), p, chk, false); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var dueNote *string
	if dn := strings.TrimSpace(strVal(patch["due_note"])); dn != "" {
		if support.RuneLen(dn) > 300 {
			httpx.HandleErr(w, r, httpx.ErrValidation("due_note", "api.tasks.note_max"))
			return
		}
		dueNote = &dn
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var notifyAssigneeID *int64
	var notifyDue bool
	var notifyDueVal *string

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		type fieldSpec struct {
			key  string
			kind string
		}
		specs := []fieldSpec{
			{"title", "title"},
			{"description", "description"},
			{"assignee_id", "assignee"},
			{"priority", "priority"},
			{"due", "due"},
			{"phase_id", "phase"},
		}
		for _, sp := range specs {
			if _, ok := patch[sp.key]; !ok {
				continue
			}
			switch sp.key {
			case "title":
				after := strings.TrimSpace(strVal(patch["title"]))
				if after != t.title {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'title', ?, ?, ?)`,
						t.id, u.ID, t.title, after, nowStr)
					t.title = after
				}
			case "description":
				after := strVal(patch["description"])
				if after != t.description {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, at) VALUES (?, ?, 'description', ?)`,
						t.id, u.ID, nowStr)
					t.description = after
				}
			case "assignee_id":
				after := optInt64(patch["assignee_id"])
				if !eqInt64Ptr(after, t.assigneeID) {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'assignee', ?, ?, ?)`,
						t.id, u.ID, int64PtrStr(t.assigneeID), int64PtrStr(after), nowStr)
					t.assigneeID = after
					if after != nil {
						notifyAssigneeID = after
					}
				}
			case "priority":
				after := strVal(patch["priority"])
				if after != "" && after != t.priority {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'priority', ?, ?, ?)`,
						t.id, u.ID, t.priority, after, nowStr)
					t.priority = after
				}
			case "due":
				after := optStr(patch["due"])
				if !eqStrPtr(after, t.due) {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'due', ?, ?, ?, ?)`,
						t.id, u.ID, t.due, after, dueNote, nowStr)
					t.due = after
					notifyDue = true
					notifyDueVal = after
				}
			case "phase_id":
				after := optInt64(patch["phase_id"])
				if !eqInt64Ptr(after, t.phaseID) {
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'phase', ?, ?, ?)`,
						t.id, u.ID, int64PtrStr(t.phaseID), int64PtrStr(after), nowStr)
					t.phaseID = after
				}
			}
		}

		if _, ok := patch["start"]; ok {
			t.start = optStr(patch["start"])
		}

		if _, ok := patch["sprint_id"]; ok && p.UsesSprints {
			nextSp := optInt64(patch["sprint_id"])
			if !eqInt64Ptr(nextSp, t.sprintID) {
				var oldName, newName *string
				if t.sprintID != nil {
					var on string
					var closedRaw any
					if tx.QueryRowContext(r.Context(), `SELECT name, closed_at FROM sprints WHERE id = ?`, *t.sprintID).Scan(&on, &closedRaw) == nil {
						if dbx.ParseTime(closedRaw) != nil {
							return httpx.ErrValidation("sprint_id", "api.tasks.task_in_closed_sprint")
						}
						oldName = &on
					}
				}
				if nextSp != nil {
					var nn string
					if tx.QueryRowContext(r.Context(), `SELECT name FROM sprints WHERE id = ?`, *nextSp).Scan(&nn) == nil {
						newName = &nn
					}
				}
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'sprint', ?, ?, ?)`,
					t.id, u.ID, oldName, newName, nowStr)
				t.sprintID = nextSp
			}
		}

		_, err := tx.ExecContext(r.Context(), `
			UPDATE tasks SET title = ?, description = ?, assignee_id = ?, priority = ?, start = ?, due = ?, phase_id = ?, sprint_id = ?, updated_at = ?
			WHERE id = ?
		`, t.title, t.description, t.assigneeID, t.priority, t.start, t.due, t.phaseID, t.sprintID, nowStr, t.id)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if notifyAssigneeID != nil {
		s.notifications.Notify(r.Context(), []int64{*notifyAssigneeID}, "task_assigned", notifications.About{
			ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: notifications.AboutTask(t.code, t.title, p.Name, p.NameEn),
		})
	}
	if notifyDue && t.assigneeID != nil {
		d := notifications.AboutTask(t.code, t.title, p.Name, p.NameEn)
		if notifyDueVal != nil {
			d["due"] = *notifyDueVal
		} else {
			d["due"] = nil
		}
		if dueNote != nil {
			d["note"] = *dueNote
		}
		s.notifications.Notify(r.Context(), []int64{*t.assigneeID}, "task_due_changed", notifications.About{
			ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: d,
		})
	}

	updated, _, _ := s.taskForOrFail(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleMove(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, p, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanMoveTask(u, p, t.assigneeID) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.move_forbidden"))
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	toStatus := strVal(body["status"])
	var reason *string
	if br := strings.TrimSpace(strVal(body["blocked_reason"])); br != "" {
		if support.RuneLen(br) > 300 {
			httpx.HandleErr(w, r, httpx.ErrValidation("blocked_reason", "api.tasks.reason_max"))
			return
		}
		reason = &br
	}
	target := p.FindStatus(toStatus)
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("status", "api.tasks.status_unknown"))
		return
	}
	if t.status == toStatus {
		httpx.Success(w, map[string]any{"task": t.toJSON(), "outcome": "moved"})
		return
	}
	if target.Tone == "blocked" && reason == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("blocked_reason", "api.tasks.blocked_reason_required"))
		return
	}
	canApprove := access.CanApprove(u, p)
	if target.Tone == "done" && !canApprove {
		if t.approval != nil && strVal(t.approval["state"]) == "pending" && strVal(t.approval["to"]) == toStatus {
			httpx.Success(w, map[string]any{"task": t.toJSON(), "outcome": "moved"})
			return
		}
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	outcome := "moved"
	var pendingReqID *int64
	var sendStatusNotif bool
	var freedWaiters []waiterNotif

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		var curStatus string
		if err := tx.QueryRowContext(r.Context(), `SELECT status FROM tasks WHERE id = ?`+s.db.ForUpdate(), t.id).Scan(&curStatus); err != nil {
			return err
		}
		if target.Tone == "done" {
			if err := s.mustNotWaitTx(r.Context(), tx, p, t.id); err != nil {
				return err
			}
		}

		if target.Tone == "done" && !canApprove {
			_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET is_current = 0, updated_at = ? WHERE task_id = ? AND is_current = 1`, nowStr, t.id)
			_, err := tx.ExecContext(r.Context(), `
				INSERT INTO task_approvals (task_id, state, from_status, to_status, requested_by, requested_at, is_current, created_at, updated_at)
				VALUES (?, 'pending', ?, ?, ?, ?, 1, ?, ?)
			`, t.id, curStatus, toStatus, u.ID, nowStr, nowStr, nowStr)
			if err != nil {
				return err
			}
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'approval_requested', ?, ?, ?)`,
				t.id, u.ID, curStatus, toStatus, nowStr)
			_, _ = tx.ExecContext(r.Context(), `UPDATE tasks SET updated_at = ? WHERE id = ?`, nowStr, t.id)
			outcome = "approval_requested"
			return nil
		}

		fromSt := p.FindStatus(curStatus)
		leavingBlocked := fromSt != nil && fromSt.Tone == "blocked"
		var nextReason *string
		if target.Tone == "blocked" {
			nextReason = reason
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'blocked', ?, ?, ?, ?)`,
				t.id, u.ID, curStatus, toStatus, reason, nowStr)
		} else {
			evKind := "status"
			if leavingBlocked {
				evKind = "unblocked"
			}
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, ?, ?, ?, ?)`,
				t.id, u.ID, evKind, curStatus, toStatus, nowStr)
		}

		var appID int64
		var appState string
		var appReqBy int64
		appErr := tx.QueryRowContext(r.Context(), `SELECT id, state, requested_by FROM task_approvals WHERE task_id = ? AND is_current = 1 ORDER BY id DESC LIMIT 1`, t.id).
			Scan(&appID, &appState, &appReqBy)

		if target.Tone == "done" {
			if appErr == nil && appState == "pending" {
				pendingReqID = &appReqBy
				_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET state = 'approved', to_status = ?, decided_by = ?, decided_at = ?, updated_at = ? WHERE id = ?`,
					toStatus, u.ID, nowStr, nowStr, appID)
			} else {
				_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET is_current = 0, updated_at = ? WHERE task_id = ? AND is_current = 1`, nowStr, t.id)
				_, _ = tx.ExecContext(r.Context(), `
					INSERT INTO task_approvals (task_id, state, from_status, to_status, requested_by, requested_at, decided_by, decided_at, is_current, created_at, updated_at)
					VALUES (?, 'approved', ?, ?, ?, ?, ?, ?, 1, ?, ?)
				`, t.id, curStatus, toStatus, u.ID, nowStr, u.ID, nowStr, nowStr, nowStr)
			}
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'approved', ?, ?, ?)`,
				t.id, u.ID, curStatus, toStatus, nowStr)
		} else if appErr == nil && appState == "pending" {
			_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET state = 'cancelled', is_current = 0, updated_at = ? WHERE id = ?`, nowStr, appID)
		}

		if _, err := tx.ExecContext(r.Context(), `UPDATE tasks SET status = ?, blocked_reason = ?, updated_at = ? WHERE id = ?`,
			toStatus, nextReason, nowStr, t.id); err != nil {
			return err
		}
		sendStatusNotif = true
		if target.Tone == "done" {
			freedWaiters = s.collectFreeWaitersTx(r.Context(), tx, p, t.id)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if outcome == "approval_requested" {
		approvers := s.approverRecipientIDs(r.Context(), p)
		s.notifications.Notify(r.Context(), approvers, "approval_requested", notifications.About{
			ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: notifications.AboutTask(t.code, t.title, p.Name, p.NameEn),
		})
	} else if sendStatusNotif {
		var recips []int64
		if t.assigneeID != nil {
			recips = append(recips, *t.assigneeID)
		}
		recips = append(recips, p.PMID)
		d := notifications.AboutTask(t.code, t.title, p.Name, p.NameEn)
		d["status"] = target.Name
		d["status_en"] = target.NameEn
		if reason != nil {
			d["note"] = *reason
		} else {
			d["note"] = nil
		}
		s.notifications.Notify(r.Context(), recips, "task_status", notifications.About{
			ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: d,
		})
		for _, wItem := range freedWaiters {
			wd := notifications.AboutTask(wItem.code, wItem.title, p.Name, p.NameEn)
			wd["note"] = t.code
			s.notifications.Notify(r.Context(), []int64{wItem.assigneeID}, "dependency_done", notifications.About{
				ActorID: &u.ID, ProjectID: &p.ID, TaskID: &wItem.taskID, Data: wd,
			})
		}
		if pendingReqID != nil && *pendingReqID != u.ID {
			ad := notifications.AboutTask(t.code, t.title, p.Name, p.NameEn)
			ad["decision"] = "approved"
			s.notifications.Notify(r.Context(), []int64{*pendingReqID}, "approval_decided", notifications.About{
				ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: ad,
			})
		}
	}

	updated, _, _ := s.taskForOrFail(r.Context(), u, id)
	httpx.Success(w, map[string]any{"task": updated.toJSON(), "outcome": outcome})
}

func (s *Service) HandleDecideApproval(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, p, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanApprove(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.approve_forbidden"))
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	approve, _ := body["approve"].(bool)
	var cleanNote *string
	if nt := strings.TrimSpace(strVal(body["note"])); nt != "" {
		if support.RuneLen(nt) > 300 {
			httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.tasks.note_max"))
			return
		}
		cleanNote = &nt
	}
	if !approve && cleanNote == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.tasks.reject_note_required"))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var requestedBy int64
	var freedWaiters []waiterNotif

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		var curStatus string
		if err := tx.QueryRowContext(r.Context(), `SELECT status FROM tasks WHERE id = ?`+s.db.ForUpdate(), t.id).Scan(&curStatus); err != nil {
			return err
		}
		var appID int64
		var appState, fromSt, toSt string
		if err := tx.QueryRowContext(r.Context(), `SELECT id, state, from_status, to_status, requested_by FROM task_approvals WHERE task_id = ? AND is_current = 1 ORDER BY id DESC LIMIT 1`, t.id).
			Scan(&appID, &appState, &fromSt, &toSt, &requestedBy); err != nil || appState != "pending" {
			return httpx.ErrConflict("api.tasks.no_pending_approval")
		}
		if approve {
			if err := s.mustNotWaitTx(r.Context(), tx, p, t.id); err != nil {
				return err
			}
			_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET state = 'approved', decided_by = ?, decided_at = ?, note = ?, updated_at = ? WHERE id = ?`,
				u.ID, nowStr, cleanNote, nowStr, appID)
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'approved', ?, ?, ?, ?)`,
				t.id, u.ID, curStatus, toSt, cleanNote, nowStr)
			_, _ = tx.ExecContext(r.Context(), `UPDATE tasks SET status = ?, blocked_reason = NULL, updated_at = ? WHERE id = ?`, toSt, nowStr, t.id)
			freedWaiters = s.collectFreeWaitersTx(r.Context(), tx, p, t.id)
		} else {
			_, _ = tx.ExecContext(r.Context(), `UPDATE task_approvals SET state = 'rejected', decided_by = ?, decided_at = ?, note = ?, updated_at = ? WHERE id = ?`,
				u.ID, nowStr, cleanNote, nowStr, appID)
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'rejected', ?, ?, ?, ?)`,
				t.id, u.ID, toSt, fromSt, cleanNote, nowStr)
			_, _ = tx.ExecContext(r.Context(), `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, fromSt, nowStr, t.id)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if approve {
		for _, wItem := range freedWaiters {
			wd := notifications.AboutTask(wItem.code, wItem.title, p.Name, p.NameEn)
			wd["note"] = t.code
			s.notifications.Notify(r.Context(), []int64{wItem.assigneeID}, "dependency_done", notifications.About{
				ActorID: &u.ID, ProjectID: &p.ID, TaskID: &wItem.taskID, Data: wd,
			})
		}
	}
	dec := "rejected"
	if approve {
		dec = "approved"
	}
	ad := notifications.AboutTask(t.code, t.title, p.Name, p.NameEn)
	ad["decision"] = dec
	if cleanNote != nil {
		ad["note"] = *cleanNote
	} else {
		ad["note"] = nil
	}
	s.notifications.Notify(r.Context(), []int64{requestedBy}, "approval_decided", notifications.About{
		ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: ad,
	})

	updated, _, _ := s.taskForOrFail(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleSetDependencies(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, p, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanManageTasks(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.dependencies_forbidden"))
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	next := uniqueInt64(toInt64Slice(body["depends_on"]))
	if len(next) > 10 {
		httpx.HandleErr(w, r, httpx.ErrValidation("depends_on", "api.tasks.dependencies_max", map[string]any{"max": 10}))
		return
	}
	for _, d := range next {
		if d == t.id {
			httpx.HandleErr(w, r, httpx.ErrValidation("depends_on", "api.tasks.dependencies_self"))
			return
		}
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.Context(), `SELECT id, code FROM tasks WHERE project_id = ?`+s.db.ForUpdate(), p.ID)
		if err != nil {
			return err
		}
		projCodes := map[int64]string{}
		for rows.Next() {
			var tid int64
			var code string
			if rows.Scan(&tid, &code) == nil {
				projCodes[tid] = code
			}
		}
		_ = rows.Close()

		for _, depID := range next {
			if _, ok := projCodes[depID]; !ok {
				return httpx.ErrValidation("depends_on", "api.tasks.dependencies_same_project")
			}
		}

		depRows, err := tx.QueryContext(r.Context(), `SELECT td.task_id, td.depends_on_task_id FROM task_dependencies td INNER JOIN tasks tt ON tt.id = td.task_id WHERE tt.project_id = ?`, p.ID)
		if err != nil {
			return err
		}
		adj := map[int64][]int64{}
		for depRows.Next() {
			var tid, did int64
			if depRows.Scan(&tid, &did) == nil {
				adj[tid] = append(adj[tid], did)
			}
		}
		_ = depRows.Close()

		// Loop detection
		seen := map[int64]bool{}
		stack := append([]int64{}, next...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == t.id {
				return httpx.ErrValidation("depends_on", "api.tasks.dependencies_loop")
			}
			if seen[cur] {
				continue
			}
			seen[cur] = true
			stack = append(stack, adj[cur]...)
		}

		before := adj[t.id]
		for _, depID := range next {
			if !containsInt64(before, depID) {
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`to`"+`, at) VALUES (?, ?, 'dependency_added', ?, ?)`,
					t.id, u.ID, projCodes[depID], nowStr)
			}
		}
		for _, depID := range before {
			if !containsInt64(next, depID) {
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, at) VALUES (?, ?, 'dependency_removed', ?, ?)`,
					t.id, u.ID, projCodes[depID], nowStr)
			}
		}

		if _, err := tx.ExecContext(r.Context(), `DELETE FROM task_dependencies WHERE task_id = ?`, t.id); err != nil {
			return err
		}
		for _, depID := range next {
			_, _ = tx.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO task_dependencies (task_id, depends_on_task_id, created_at) VALUES (?, ?, ?)`,
				t.id, depID, nowStr)
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET updated_at = ? WHERE id = ?`, nowStr, t.id)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	updated, _, _ := s.taskForOrFail(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleComments(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, _, err := s.taskForOrFail(r.Context(), u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, task_id, author_id, body, created_at FROM task_comments WHERE task_id = ? ORDER BY id ASC`, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, tid, aid int64
		var body string
		var createdRaw any
		if rows.Scan(&cid, &tid, &aid, &body, &createdRaw) == nil {
			out = append(out, map[string]any{
				"id":         cid,
				"task_id":    tid,
				"author_id":  aid,
				"body":       body,
				"created_at": store.IsoTime(dbx.ParseTime(createdRaw)),
			})
		}
	}
	httpx.Success(w, out)
}

func (s *Service) HandleAddComment(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, p, err := s.taskForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanTakePart(u, p) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.tasks.comment_forbidden"))
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	text := strings.TrimSpace(strVal(body["body"]))
	if text == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.tasks.comment_required"))
		return
	}
	if support.RuneLen(text) > 2000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.tasks.comment_max", map[string]any{"max": 2000}))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var commentID int64
	var priorAuthors []int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		rows, _ := tx.QueryContext(r.Context(), `SELECT author_id FROM task_comments WHERE task_id = ?`, t.id)
		if rows != nil {
			for rows.Next() {
				var aid int64
				if rows.Scan(&aid) == nil {
					priorAuthors = append(priorAuthors, aid)
				}
			}
			_ = rows.Close()
		}
		res, err := tx.ExecContext(r.Context(), `INSERT INTO task_comments (task_id, author_id, body, created_at) VALUES (?, ?, ?, ?)`,
			t.id, u.ID, text, nowStr)
		if err != nil {
			return err
		}
		commentID, _ = res.LastInsertId()
		_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, at) VALUES (?, ?, 'comment', ?)`, t.id, u.ID, nowStr)
		_, err = tx.ExecContext(r.Context(), `UPDATE tasks SET updated_at = ? WHERE id = ?`, nowStr, t.id)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var recips []int64
	if t.assigneeID != nil {
		recips = append(recips, *t.assigneeID)
	}
	recips = append(recips, p.PMID)
	recips = append(recips, priorAuthors...)
	cd := notifications.AboutTask(t.code, t.title, p.Name, p.NameEn)
	cd["note"] = support.TruncateRunes(text, 120)
	s.notifications.Notify(r.Context(), recips, "task_comment", notifications.About{
		ActorID: &u.ID, ProjectID: &p.ID, TaskID: &t.id, Data: cd,
	})

	createdAt := dbx.ParseTimeString(nowStr)
	httpx.Success(w, map[string]any{
		"id":         commentID,
		"task_id":    t.id,
		"author_id":  u.ID,
		"body":       text,
		"created_at": store.IsoTime(createdAt),
	}, http.StatusCreated)
}

func (s *Service) HandleHistory(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, _, err := s.taskForOrFail(r.Context(), u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at FROM task_events WHERE task_id = ? ORDER BY id ASC`, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var eid, tid, aid int64
		var kind string
		var from, to, note sql.NullString
		var atRaw any
		if rows.Scan(&eid, &tid, &aid, &kind, &from, &to, &note, &atRaw) == nil {
			m := map[string]any{"id": eid, "task_id": tid, "actor_id": aid, "kind": kind, "at": store.IsoTime(dbx.ParseTime(atRaw))}
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

// Helpers

func (s *Service) taskForOrFail(ctx context.Context, u *store.User, id int64) (*taskRow, *access.Project, error) {
	list, err := s.loadTasks(ctx, `WHERE id = ?`, id)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return nil, nil, httpx.ErrNotFound("api.tasks.not_found")
	}
	t := list[0]
	p, err := access.LoadProject(ctx, s.db, t.projectID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil || !access.CanSeeProject(u, p) {
		return nil, nil, httpx.ErrNotFound("api.tasks.not_found")
	}
	return t, p, nil
}

func (s *Service) loadTasks(ctx context.Context, whereSQL string, args ...any) ([]*taskRow, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, code, project_id, phase_id, sprint_id, title, description, assignee_id, start, due, priority, status, blocked_reason, subtasks_done, subtasks_total, created_by, created_at, updated_at
		FROM tasks %s
	`, whereSQL), args...)
	if err != nil {
		return nil, err
	}
	var list []*taskRow
	var ids []int64
	byID := map[int64]*taskRow{}
	for rows.Next() {
		var t taskRow
		var phID, spID, assID, cbID sql.NullInt64
		var br sql.NullString
		var stRaw, dueRaw, crRaw, upRaw any
		if err := rows.Scan(&t.id, &t.code, &t.projectID, &phID, &spID, &t.title, &t.description, &assID, &stRaw, &dueRaw, &t.priority, &t.status, &br, &t.subtasksDone, &t.subtasksTotal, &cbID, &crRaw, &upRaw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if phID.Valid {
			v := phID.Int64
			t.phaseID = &v
		}
		if spID.Valid {
			v := spID.Int64
			t.sprintID = &v
		}
		if assID.Valid {
			v := assID.Int64
			t.assigneeID = &v
		}
		if br.Valid && br.String != "" {
			v := br.String
			t.blockedReason = &v
		}
		if cbID.Valid {
			v := cbID.Int64
			t.createdBy = &v
		}
		t.start = dbx.ParseDateOnly(stRaw)
		t.due = dbx.ParseDateOnly(dueRaw)
		t.createdAt = dbx.ParseTime(crRaw)
		t.updatedAt = dbx.ParseTime(upRaw)
		list = append(list, &t)
		ids = append(ids, t.id)
		byID[t.id] = &t
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return list, rows.Err()
	}

	ph, inArgs := dbx.InInt64(ids)
	loadDeps := func() {
		dRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT task_id, depends_on_task_id FROM task_dependencies WHERE task_id IN (%s) ORDER BY id ASC`, ph), inArgs...)
		if err != nil {
			return
		}
		defer dRows.Close()
		for dRows.Next() {
			var tid, did int64
			if dRows.Scan(&tid, &did) == nil {
				if t := byID[tid]; t != nil {
					t.dependsOn = append(t.dependsOn, did)
				}
			}
		}
	}
	loadApprovals := func() {
		aRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT task_id, state, from_status, to_status, requested_by, requested_at, decided_by, decided_at, note
			FROM task_approvals
			WHERE task_id IN (%s) AND is_current = 1
			ORDER BY id DESC
		`, ph), inArgs...)
		if err != nil {
			return
		}
		defer aRows.Close()
		for aRows.Next() {
			var tid, reqBy int64
			var st, fromSt, toSt string
			var reqAtRaw, decAtRaw any
			var decBy sql.NullInt64
			var note sql.NullString
			if aRows.Scan(&tid, &st, &fromSt, &toSt, &reqBy, &reqAtRaw, &decBy, &decAtRaw, &note) == nil {
				if t := byID[tid]; t != nil && t.approval == nil && st != "cancelled" {
					am := map[string]any{
						"state":        st,
						"from":         fromSt,
						"to":           toSt,
						"requested_by": reqBy,
						"requested_at": store.IsoTime(dbx.ParseTime(reqAtRaw)),
					}
					if decBy.Valid {
						am["decided_by"] = decBy.Int64
					}
					if dt := dbx.ParseTime(decAtRaw); dt != nil {
						am["decided_at"] = store.IsoTime(dt)
					}
					if note.Valid && note.String != "" {
						am["note"] = note.String
					}
					t.approval = am
				}
			}
		}
	}

	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); loadDeps() }()
		go func() { defer wg.Done(); loadApprovals() }()
		wg.Wait()
	} else {
		loadDeps()
		loadApprovals()
	}

	return list, nil
}

func (s *Service) checkFields(ctx context.Context, p *access.Project, fields map[string]any, isCreate bool) error {
	if isCreate || hasKey(fields, "title") {
		title := strings.TrimSpace(strVal(fields["title"]))
		if title == "" {
			return httpx.ErrValidation("title", "api.tasks.title_required")
		}
		if support.RuneLen(title) > 160 {
			return httpx.ErrValidation("title", "api.tasks.title_max", map[string]any{"max": 160})
		}
	}
	if hasKey(fields, "description") && support.RuneLen(strVal(fields["description"])) > 5000 {
		return httpx.ErrValidation("description", "api.tasks.description_max")
	}
	if hasKey(fields, "priority") && !store.UserHasRole(strVal(fields["priority"]), Priorities) {
		return httpx.ErrValidation("priority", "api.tasks.priority_invalid")
	}
	if aid := optInt64(fields["assignee_id"]); aid != nil {
		if !p.IsMember(*aid) {
			return httpx.ErrValidation("assignee_id", "api.tasks.assignee_not_member")
		}
	}
	start := optStr(fields["start"])
	due := optStr(fields["due"])
	if start != nil && due != nil && *due < *start {
		return httpx.ErrValidation("due", "api.tasks.due_before_start")
	}
	if spID := optInt64(fields["sprint_id"]); spID != nil {
		var pid int64
		var closedRaw any
		err := s.db.QueryRowContext(ctx, `SELECT project_id, closed_at FROM sprints WHERE id = ?`, *spID).Scan(&pid, &closedRaw)
		if err != nil || pid != p.ID {
			return httpx.ErrValidation("sprint_id", "api.tasks.sprint_not_in_project")
		}
		if dbx.ParseTime(closedRaw) != nil {
			return httpx.ErrValidation("sprint_id", "api.tasks.sprint_closed")
		}
	}
	if phID := optInt64(fields["phase_id"]); phID != nil && !p.HasPhase(*phID) {
		return httpx.ErrValidation("phase_id", "api.tasks.phase_not_in_project")
	}
	return nil
}

func (s *Service) mustNotWaitTx(ctx context.Context, tx *sql.Tx, p *access.Project, taskID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT t.code, t.status FROM task_dependencies td INNER JOIN tasks t ON t.id = td.depends_on_task_id WHERE td.task_id = ? ORDER BY t.id ASC`, taskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	doneSet := map[string]bool{}
	for _, k := range p.DoneKeys() {
		doneSet[k] = true
	}
	var open []string
	for rows.Next() {
		var code, st string
		if rows.Scan(&code, &st) == nil && !doneSet[st] {
			open = append(open, code)
		}
	}
	if len(open) > 0 {
		sep := ", "
		if httpx.LocaleFrom(ctx) == "ar" {
			sep = "، "
		}
		return &httpx.APIError{Status: 409, Code: "CONFLICT", Key: "api.tasks.waiting_for_dependencies", Params: map[string]any{"codes": strings.Join(open, sep)}}
	}
	return nil
}

type waiterNotif struct {
	taskID     int64
	code       string
	title      string
	assigneeID int64
}

func (s *Service) collectFreeWaitersTx(ctx context.Context, tx *sql.Tx, p *access.Project, doneTaskID int64) []waiterNotif {
	doneSet := map[string]bool{}
	for _, k := range p.DoneKeys() {
		doneSet[k] = true
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.code, t.title, t.assignee_id
		FROM task_dependencies td
		INNER JOIN tasks t ON t.id = td.task_id
		WHERE td.depends_on_task_id = ? AND t.assignee_id IS NOT NULL
	`, doneTaskID)
	if err != nil {
		return nil
	}
	var cands []waiterNotif
	for rows.Next() {
		var w waiterNotif
		if rows.Scan(&w.taskID, &w.code, &w.title, &w.assigneeID) == nil {
			cands = append(cands, w)
		}
	}
	_ = rows.Close()

	var out []waiterNotif
	for _, c := range cands {
		dRows, err := tx.QueryContext(ctx, `SELECT t.status FROM task_dependencies td INNER JOIN tasks t ON t.id = td.depends_on_task_id WHERE td.task_id = ?`, c.taskID)
		if err != nil {
			continue
		}
		hasOpen := false
		for dRows.Next() {
			var st string
			if dRows.Scan(&st) == nil && !doneSet[st] {
				hasOpen = true
			}
		}
		_ = dRows.Close()
		if !hasOpen {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) approverRecipientIDs(ctx context.Context, p *access.Project) []int64 {
	var recips []int64
	recips = append(recips, p.PMID)
	if len(p.MemberIDs) > 0 {
		ph, args := dbx.InInt64(p.MemberIDs)
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM users WHERE active = 1 AND role = 'lead' AND id IN (%s)`, ph), args...)
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					recips = append(recips, id)
				}
			}
			_ = rows.Close()
		}
	}
	return uniqueInt64(recips)
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

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

func toInt64Slice(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []int64
	for _, x := range arr {
		if p := optInt64(x); p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func uniqueInt64(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func containsInt64(arr []int64, target int64) bool {
	for _, v := range arr {
		if v == target {
			return true
		}
	}
	return false
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
