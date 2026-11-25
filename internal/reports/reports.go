package reports

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"followapi/internal/access"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
)

const BehindBy = 0.15

type Service struct {
	db *dbx.DB
}

func New(db *dbx.DB) *Service {
	return &Service{db: db}
}

func (s *Service) HandleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanSeeReports(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.reports.forbidden"))
		return
	}
	today := support.BaghdadToday()

	type teamLite struct {
		id     int64
		name   string
		nameEn sql.NullString
	}
	var (
		allProjects []*access.Project
		projErr     error
		users       []*store.User
		teamsList   []teamLite
	)
	loadTeams := func() {
		tRows, _ := s.db.QueryContext(ctx, `SELECT id, name, name_en FROM teams ORDER BY id ASC`)
		if tRows == nil {
			return
		}
		defer tRows.Close()
		for tRows.Next() {
			var tl teamLite
			if tRows.Scan(&tl.id, &tl.name, &tl.nameEn) == nil {
				teamsList = append(teamsList, tl)
			}
		}
	}
	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); allProjects, projErr = access.LoadAllProjects(ctx, s.db) }()
		go func() { defer wg.Done(); users, _ = store.LoadAllUsers(ctx, s.db, "") }()
		go func() { defer wg.Done(); loadTeams() }()
		wg.Wait()
	} else {
		allProjects, projErr = access.LoadAllProjects(ctx, s.db)
		users, _ = store.LoadAllUsers(ctx, s.db, "")
		loadTeams()
	}
	if projErr != nil {
		httpx.HandleErr(w, r, projErr)
		return
	}
	var visible []*access.Project
	var visIDs []int64
	projMap := map[int64]*access.Project{}
	for _, p := range allProjects {
		if access.CanSeeProject(u, p) {
			visible = append(visible, p)
			visIDs = append(visIDs, p.ID)
			projMap[p.ID] = p
		}
	}

	type taskLite struct {
		projectID  int64
		sprintID   *int64
		assigneeID *int64
		due        *string
		status     string
	}
	type sprintLite struct {
		id, pid int64
		name    string
		sRaw, eRaw, cRaw any
		resRaw  sql.NullString
	}
	type bugLite struct {
		sev, st string
		dueRaw  any
	}
	var (
		allTasks   []taskLite
		taskErr    error
		rawSprints []sprintLite
		rawBugs    []bugLite
	)
	if len(visIDs) > 0 {
		ph, args := dbx.InInt64(visIDs)
		loadTasksFn := func() {
			rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT project_id, sprint_id, assignee_id, due, status FROM tasks WHERE project_id IN (%s)`, ph), args...)
			if err != nil {
				taskErr = err
				return
			}
			defer rows.Close()
			for rows.Next() {
				var t taskLite
				var spID, assID sql.NullInt64
				var dueRaw any
				if rows.Scan(&t.projectID, &spID, &assID, &dueRaw, &t.status) == nil {
					if spID.Valid {
						v := spID.Int64
						t.sprintID = &v
					}
					if assID.Valid {
						v := assID.Int64
						t.assigneeID = &v
					}
					t.due = dbx.ParseDateOnly(dueRaw)
					allTasks = append(allTasks, t)
				}
			}
		}
		loadSprintsFn := func() {
			spRows, _ := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, project_id, name, start, `+"`end`"+`, closed_at, result FROM sprints WHERE project_id IN (%s) ORDER BY start ASC`, ph), args...)
			if spRows == nil {
				return
			}
			defer spRows.Close()
			for spRows.Next() {
				var sp sprintLite
				if spRows.Scan(&sp.id, &sp.pid, &sp.name, &sp.sRaw, &sp.eRaw, &sp.cRaw, &sp.resRaw) == nil {
					rawSprints = append(rawSprints, sp)
				}
			}
		}
		loadBugsFn := func() {
			bRows, _ := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT severity, status, due FROM bugs WHERE project_id IN (%s)`, ph), args...)
			if bRows == nil {
				return
			}
			defer bRows.Close()
			for bRows.Next() {
				var b bugLite
				if bRows.Scan(&b.sev, &b.st, &b.dueRaw) == nil {
					rawBugs = append(rawBugs, b)
				}
			}
		}
		if !s.db.IsSQLite() {
			var wg sync.WaitGroup
			wg.Add(3)
			go func() { defer wg.Done(); loadTasksFn() }()
			go func() { defer wg.Done(); loadSprintsFn() }()
			go func() { defer wg.Done(); loadBugsFn() }()
			wg.Wait()
		} else {
			loadTasksFn()
			loadSprintsFn()
			loadBugsFn()
		}
		if taskErr != nil {
			httpx.HandleErr(w, r, taskErr)
			return
		}
	}

	usersMap := map[int64]*store.User{}
	for _, usr := range users {
		usersMap[usr.ID] = usr
	}

	projectRows := make([]map[string]any, 0, len(visible))
	for _, p := range visible {
		doneSet := map[string]bool{}
		blockedSet := map[string]bool{}
		for _, st := range p.Statuses {
			if st.Tone == "done" {
				doneSet[st.Key] = true
			} else if st.Tone == "blocked" {
				blockedSet[st.Key] = true
			}
		}
		var total, done, late, blocked int
		for _, t := range allTasks {
			if t.projectID != p.ID {
				continue
			}
			total++
			if doneSet[t.status] {
				done++
			}
			if blockedSet[t.status] {
				blocked++
			}
			if t.due != nil && *t.due < today && !doneSet[t.status] {
				late++
			}
		}
		ratio := 0.0
		if total > 0 {
			ratio = float64(done) / float64(total)
		}
		planned := support.PlannedRatio(p.Start, p.End, today)
		projectRows = append(projectRows, store.CleanMap(map[string]any{
			"id":            p.ID,
			"code":          p.Code,
			"name":          p.Name,
			"name_en":       p.NameEn,
			"state":         p.State,
			"pm_id":         p.PMID,
			"start":         p.Start,
			"end":           p.End,
			"total":         total,
			"done":          done,
			"late":          late,
			"blocked":       blocked,
			"ratio":         ratio,
			"planned_ratio": planned,
			"overdue":       p.State != "done" && p.End < today,
			"behind":        p.State == "active" && (ratio+BehindBy) < planned,
		}))
	}

	type personStat struct {
		UserID   int64
		TeamID   *int64
		Open     int
		Done     int
		Late     int
		Blocked  int
		InReview int
	}
	byPerson := map[int64]*personStat{}
	for _, t := range allTasks {
		if t.assigneeID == nil {
			continue
		}
		p := projMap[t.projectID]
		if p == nil {
			continue
		}
		uid := *t.assigneeID
		ps := byPerson[uid]
		if ps == nil {
			var tid *int64
			if usr := usersMap[uid]; usr != nil {
				tid = usr.TeamID
			}
			ps = &personStat{UserID: uid, TeamID: tid}
			byPerson[uid] = ps
		}
		st := p.FindStatus(t.status)
		tone := ""
		if st != nil {
			tone = st.Tone
		}
		if tone == "done" {
			ps.Done++
		} else {
			ps.Open++
		}
		if t.due != nil && *t.due < today && tone != "done" {
			ps.Late++
		}
		if tone == "blocked" {
			ps.Blocked++
		}
		if tone == "review" {
			ps.InReview++
		}
	}

	personStats := make([]*personStat, 0, len(byPerson))
	for _, ps := range byPerson {
		personStats = append(personStats, ps)
	}
	sort.SliceStable(personStats, func(i, j int) bool {
		if personStats[i].Late != personStats[j].Late {
			return personStats[i].Late > personStats[j].Late
		}
		if personStats[i].Open != personStats[j].Open {
			return personStats[i].Open > personStats[j].Open
		}
		return personStats[i].UserID < personStats[j].UserID
	})
	peopleRows := make([]map[string]any, 0, len(personStats))
	for _, ps := range personStats {
		peopleRows = append(peopleRows, store.CleanMap(map[string]any{
			"user_id":   ps.UserID,
			"team_id":   ps.TeamID,
			"open":      ps.Open,
			"done":      ps.Done,
			"late":      ps.Late,
			"blocked":   ps.Blocked,
			"in_review": ps.InReview,
		}))
	}

	teamRows := []map[string]any{}
	for _, tl := range teamsList {
		var done, open, late int
		for _, ps := range personStats {
			if ps.TeamID != nil && *ps.TeamID == tl.id {
				done += ps.Done
				open += ps.Open
				late += ps.Late
			}
		}
		if open+done == 0 {
			continue
		}
		membersCnt := 0
		for _, usr := range users {
			if usr.Active && usr.TeamID != nil && *usr.TeamID == tl.id {
				membersCnt++
			}
		}
		ratio := 0.0
		if done+open > 0 {
			ratio = float64(done) / float64(done+open)
		}
		var nePtr *string
		if tl.nameEn.Valid && tl.nameEn.String != "" {
			v := tl.nameEn.String
			nePtr = &v
		}
		teamRows = append(teamRows, store.CleanMap(map[string]any{
			"team_id": tl.id,
			"name":    tl.name,
			"name_en": nePtr,
			"members": membersCnt,
			"open":    open,
			"done":    done,
			"late":    late,
			"ratio":   ratio,
		}))
	}

	sprintRows := []map[string]any{}
	for _, sp := range rawSprints {
		closed := dbx.ParseTime(sp.cRaw) != nil
		var planned, done int
		if sp.resRaw.Valid && sp.resRaw.String != "" {
			var resMap map[string]int
			_ = json.Unmarshal([]byte(sp.resRaw.String), &resMap)
			planned = resMap["planned"]
			done = resMap["done"]
		} else {
			p := projMap[sp.pid]
			for _, t := range allTasks {
				if t.sprintID != nil && *t.sprintID == sp.id {
					planned++
					if p != nil {
						if st := p.FindStatus(t.status); st != nil && st.Tone == "done" {
							done++
						}
					}
				}
			}
		}
		sprintRows = append(sprintRows, map[string]any{
			"project_id": sp.pid,
			"sprint_id":  sp.id,
			"name":       sp.name,
			"start":      derefStr(dbx.ParseDateOnly(sp.sRaw)),
			"end":        derefStr(dbx.ParseDateOnly(sp.eRaw)),
			"closed":     closed,
			"planned":    planned,
			"done":       done,
			"left":       planned - done,
		})
	}

	bugCounts := map[string]int{"open": 0, "late": 0, "closed": 0, "critical_open": 0}
	for _, b := range rawBugs {
		if b.st == "closed" {
			bugCounts["closed"]++
		} else {
			bugCounts["open"]++
			if d := dbx.ParseDateOnly(b.dueRaw); d != nil && *d < today {
				bugCounts["late"]++
			}
			if b.sev == "critical" {
				bugCounts["critical_open"]++
			}
		}
	}

	httpx.Success(w, map[string]any{
		"today":    today,
		"projects": projectRows,
		"people":   peopleRows,
		"teams":    teamRows,
		"sprints":  sprintRows,
		"bugs":     bugCounts,
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
