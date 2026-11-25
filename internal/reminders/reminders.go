package reminders

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/dbx"
	"followapi/internal/notifications"
	"followapi/internal/push"
	"followapi/internal/store"
	"followapi/internal/support"
)

type Service struct {
	db       *dbx.DB
	notif    *notifications.Service
	advanced *advanced.Service
	push     *push.Service
}

func New(db *dbx.DB, notifSvc *notifications.Service, advSvc *advanced.Service, pushSvc *push.Service) *Service {
	return &Service{db: db, notif: notifSvc, advanced: advSvc, push: pushSvc}
}

// Send runs the daily reminder sweep for Baghdad date `dayOverride` (defaults to today).
// Idempotent via notifications.dedup_key. Also prunes stale push subscriptions.
func (s *Service) Send(ctx context.Context, dayOverride string) (map[string]int64, error) {
	day := dayOverride
	if day == "" {
		day = support.BaghdadToday()
	}
	baseDate, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, fmt.Errorf("invalid reminder date %q: %w", day, err)
	}

	settings, _ := store.LoadAppSetting(ctx, s.db)
	dueDays := settings.DueSoonDays
	if dueDays < 1 {
		dueDays = 1
	}
	dueSoonLimit := baseDate.AddDate(0, 0, dueDays).Format("2006-01-02")

	projects, err := access.LoadAllProjects(ctx, s.db)
	if err != nil {
		return nil, err
	}
	projByID := map[int64]*access.Project{}
	for _, p := range projects {
		projByID[p.ID] = p
	}

	stats := map[string]int64{
		"task_due_soon":  0,
		"task_overdue":   0,
		"bug_late":       0,
		"birthday_soon":  0,
		"birthday_today": 0,
		"pruned_pushes":  0,
	}

	// 1. Task due soon & overdue
	type taskDue struct {
		tid, pid   int64
		code       string
		title      string
		status     string
		assignee   sql.NullInt64
		due        string
	}
	var taskItems []taskDue
	trows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, code, title, assignee_id, due, status
		FROM tasks
		WHERE due IS NOT NULL AND due != ''
	`)
	if err == nil {
		for trows.Next() {
			var it taskDue
			var dueRaw any
			if trows.Scan(&it.tid, &it.pid, &it.code, &it.title, &it.assignee, &dueRaw, &it.status) == nil {
				if duePtr := dbx.ParseDateOnly(dueRaw); duePtr != nil {
					it.due = *duePtr
					taskItems = append(taskItems, it)
				}
			}
		}
		_ = trows.Close()
	}
	for _, it := range taskItems {
		p := projByID[it.pid]
		if p == nil || p.State != "active" || isDoneStatus(p, it.status) {
			continue
		}
		if !it.assignee.Valid || it.assignee.Int64 <= 0 {
			continue
		}
		recs := []int64{it.assignee.Int64}
		var kind string
		if it.due < day {
			kind = "task_overdue"
		} else if it.due <= dueSoonLimit {
			kind = "task_due_soon"
		} else {
			continue
		}
		data := notifications.AboutTask(it.code, it.title, p.Name, p.NameEn)
		data["due"] = it.due
		for _, uid := range recs {
			dk := fmt.Sprintf("%s:%d:%s:u%d", kind, it.tid, it.due, uid)
			n := s.notif.Notify(ctx, []int64{uid}, kind, notifications.About{
				ProjectID: &it.pid,
				TaskID:    &it.tid,
				Data:      data,
				DedupKey:  &dk,
			})
			stats[kind] += int64(n)
		}
	}

	// 2. Late bugs (open statuses with due < day)
	type bugLate struct {
		bid, pid, reportedBy int64
		code, title          string
		assignee             sql.NullInt64
		due                  string
	}
	var bugItems []bugLate
	brows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, code, title, reported_by, assignee_id, due, status
		FROM bugs
		WHERE due IS NOT NULL AND due != '' AND status NOT IN ('closed','duplicate','rejected')
	`)
	if err == nil {
		for brows.Next() {
			var it bugLate
			var status string
			var dueRaw any
			if brows.Scan(&it.bid, &it.pid, &it.code, &it.title, &it.reportedBy, &it.assignee, &dueRaw, &status) == nil {
				if duePtr := dbx.ParseDateOnly(dueRaw); duePtr != nil && *duePtr < day {
					it.due = *duePtr
					bugItems = append(bugItems, it)
				}
			}
		}
		_ = brows.Close()
	}
	for _, it := range bugItems {
		p := projByID[it.pid]
		if p == nil || p.State != "active" {
			continue
		}
		recs := uniquePositive(p.PMID)
		if it.assignee.Valid {
			recs = uniquePositive(append(recs, it.assignee.Int64)...)
		}
		data := notifications.AboutProject(p.Name, p.NameEn)
		data["bug_code"] = it.code
		data["bug_title"] = it.title
		data["due"] = it.due
		for _, uid := range recs {
			dk := fmt.Sprintf("bug_late:%d:%s:u%d", it.bid, it.due, uid)
			n := s.notif.Notify(ctx, []int64{uid}, "bug_late", notifications.About{
				ProjectID: &it.pid,
				BugID:     &it.bid,
				Data:      data,
				DedupKey:  &dk,
			})
			stats["bug_late"] += int64(n)
		}
	}

	// 3. Birthdays (birthday_today & birthday_soon 7 days out) — sent to active monitors & CEO only, without age
	users, err := store.LoadAllUsers(ctx, s.db, "")
	if err == nil {
		var watcherIDs []int64
		for _, cand := range users {
			if cand.Active && (cand.Role == "monitor" || cand.Role == "ceo") {
				watcherIDs = append(watcherIDs, cand.ID)
			}
		}
		for _, cand := range users {
			if !cand.Active || cand.BirthDate == nil || *cand.BirthDate == "" {
				continue
			}
			bKind, bDate, ok := support.ReminderForDay(*cand.BirthDate, day)
			if !ok {
				continue
			}
			for _, recID := range watcherIDs {
				if recID == cand.ID {
					continue
				}
				dk := fmt.Sprintf("%s:%d:%s:u%d", bKind, cand.ID, bDate, recID)
				data := map[string]any{
					"person_id":   cand.ID,
					"person_name": cand.Name,
					"due":         bDate,
				}
				if cand.NameEn != nil && *cand.NameEn != "" {
					data["person_name_en"] = *cand.NameEn
				}
				if cand.TeamID != nil {
					data["team_id"] = *cand.TeamID
				}
				n := s.notif.Notify(ctx, []int64{recID}, bKind, notifications.About{
					TeamID:   cand.TeamID,
					Data:     data,
					DedupKey: &dk,
				})
				stats[bKind] += int64(n)
			}
		}
	}

	if s.push != nil {
		stats["pruned_pushes"] = s.push.PruneStale(ctx)
	}

	return stats, nil
}

func isDoneStatus(p *access.Project, status string) bool {
	for _, k := range p.DoneKeys() {
		if k == status {
			return true
		}
	}
	return false
}

func uniquePositive(ids ...int64) []int64 {
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
