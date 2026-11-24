package performance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/audit"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

type Service struct {
	db            *dbx.DB
	audit         *audit.Service
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, auditSvc *audit.Service, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, audit: auditSvc, notifications: notifSvc, advanced: advSvc}
}

type TaskLite struct {
	ID            int64
	Code          string
	ProjectID     int64
	Title         string
	AssigneeID    *int64
	Due           *string
	Status        string
	BlockedReason *string
	CreatedAt     *time.Time
	UpdatedAt     *time.Time
	DecidedAt     *time.Time
	ApprovalState *string
}

func LoadTasksForProjects(ctx context.Context, db *dbx.DB, projectIDs []int64, assigneeID *int64) ([]TaskLite, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	ph, args := dbx.InInt64(projectIDs)
	q := fmt.Sprintf(`
		SELECT t.id, t.code, t.project_id, t.title, t.assignee_id, t.due, t.status, t.blocked_reason, t.created_at, t.updated_at,
		       a.state, a.decided_at
		FROM tasks t
		LEFT JOIN task_approvals a ON a.id = (
			SELECT MAX(a2.id) FROM task_approvals a2 WHERE a2.task_id = t.id AND a2.is_current = 1
		)
		WHERE t.project_id IN (%s)
	`, ph)
	if assigneeID != nil {
		q += ` AND t.assignee_id = ?`
		args = append(args, *assigneeID)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskLite
	for rows.Next() {
		var tl TaskLite
		var assID sql.NullInt64
		var br, appSt sql.NullString
		var dueRaw, crRaw, upRaw, decRaw any
		if err := rows.Scan(&tl.ID, &tl.Code, &tl.ProjectID, &tl.Title, &assID, &dueRaw, &tl.Status, &br, &crRaw, &upRaw, &appSt, &decRaw); err != nil {
			return nil, err
		}
		if assID.Valid {
			v := assID.Int64
			tl.AssigneeID = &v
		}
		if br.Valid && br.String != "" {
			v := br.String
			tl.BlockedReason = &v
		}
		if appSt.Valid && appSt.String != "" {
			v := appSt.String
			tl.ApprovalState = &v
		}
		tl.Due = dbx.ParseDateOnly(dueRaw)
		tl.CreatedAt = dbx.ParseTime(crRaw)
		tl.UpdatedAt = dbx.ParseTime(upRaw)
		tl.DecidedAt = dbx.ParseTime(decRaw)
		out = append(out, tl)
	}
	return out, rows.Err()
}

func TaskFinishedDay(p *access.Project, t TaskLite) *string {
	if !store.UserHasRole(t.Status, p.DoneKeys()) {
		return nil
	}
	dt := t.UpdatedAt
	if t.ApprovalState != nil && *t.ApprovalState == "approved" && t.DecidedAt != nil {
		dt = t.DecidedAt
	}
	if dt == nil {
		return nil
	}
	d := support.ToBaghdadDate(*dt)
	return &d
}

func ComputePersonPerf(userID int64, teamID *int64, tasks []TaskLite, projects []*access.Project, periodFrom, periodTo, today string) map[string]any {
	projByID := make(map[int64]*access.Project, len(projects))
	for _, p := range projects {
		projByID[p.ID] = p
	}
	end := periodTo
	if today < end {
		end = today
	}

	var assigned, done, late, blocked, withDue, onTime int
	for _, t := range tasks {
		if t.AssigneeID == nil || *t.AssigneeID != userID {
			continue
		}
		p := projByID[t.ProjectID]
		if p == nil {
			continue
		}
		finished := TaskFinishedDay(p, t)
		createdDay := ""
		if t.CreatedAt != nil {
			createdDay = support.ToBaghdadDate(*t.CreatedAt)
		}
		if createdDay > periodTo || (finished != nil && *finished < periodFrom) {
			continue
		}
		assigned++
		if finished == nil {
			if st := p.FindStatus(t.Status); st != nil && st.Tone == "blocked" {
				blocked++
			}
		}
		if finished != nil && *finished <= periodTo {
			done++
			if t.Due != nil {
				withDue++
				if *finished <= *t.Due {
					onTime++
				} else {
					late++
				}
			}
		} else if t.Due != nil && *t.Due < end {
			late++
		}
	}

	completion := 0.0
	if assigned > 0 {
		completion = float64(done) / float64(assigned)
	}
	var onTimeVal any = nil
	if withDue > 0 {
		onTimeVal = float64(onTime) / float64(withDue)
	}
	return store.CleanMap(map[string]any{
		"user_id":    userID,
		"team_id":    teamID,
		"assigned":   assigned,
		"done":       done,
		"late":       late,
		"blocked":    blocked,
		"completion": completion,
		"on_time":    onTimeVal,
	}, "on_time")
}

func (s *Service) HandleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanSeePeoplePerf(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.performance.forbidden"))
		return
	}
	pFrom, pTo, err := checkPeriod(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.HandleErr(w, r, err)
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
	for _, p := range allProjects {
		if access.CanSeeProject(u, p) {
			visible = append(visible, p)
			visIDs = append(visIDs, p.ID)
		}
	}
	tasks, _ := LoadTasksForProjects(ctx, s.db, visIDs, nil)

	people := []map[string]any{}
	for _, subj := range users {
		if subj.Active && subj.ID != u.ID && subj.Role != "ceo" && subj.Role != "admin" && access.CanSeePerson(u, subj, allProjects) {
			people = append(people, ComputePersonPerf(subj.ID, subj.TeamID, tasks, visible, pFrom, pTo, today))
		}
	}

	teamsOut := []map[string]any{}
	for _, tl := range teamsList {
		row := buildTeamPerf(tl.id, tl.name, nullStrPtr(tl.nameEn), users, people, visible, tasks, pTo)
		if intVal(row["assigned"]) > 0 {
			teamsOut = append(teamsOut, row)
		}
	}

	httpx.Success(w, map[string]any{
		"people": people,
		"teams":  teamsOut,
	})
}

func (s *Service) HandlePerson(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	sid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	pFrom, pTo, err := checkPeriod(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	allProjects, err := access.LoadAllProjects(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	subject, _ := store.FindUserByID(r.Context(), s.db, "", sid)
	if subject == nil || !access.CanSeePerson(u, subject, allProjects) {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.performance.person_not_found"))
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
	var tasks []TaskLite
	reviews := []map[string]any{}
	loadReviews := func() {
		rRows, err := s.db.QueryContext(r.Context(), `
			SELECT id, user_id, reviewer_id, period_from, period_to, scores, criteria, total, note, perf, state, finalized_at, created_at
			FROM performance_reviews WHERE user_id = ? ORDER BY period_to DESC, id DESC
		`, subject.ID)
		if err != nil {
			return
		}
		defer rRows.Close()
		for rRows.Next() {
			rev, ok := scanReview(rRows)
			if ok {
				reviewerID, _ := rev["reviewer_id"].(int64)
				state, _ := rev["state"].(string)
				if access.CanSeeReview(u, reviewerID, state, subject, allProjects) {
					reviews = append(reviews, rev)
				}
			}
		}
	}
	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tasks, _ = LoadTasksForProjects(r.Context(), s.db, visIDs, &subject.ID) }()
		go func() { defer wg.Done(); loadReviews() }()
		wg.Wait()
	} else {
		tasks, _ = LoadTasksForProjects(r.Context(), s.db, visIDs, &subject.ID)
		loadReviews()
	}
	today := support.BaghdadToday()
	perf := ComputePersonPerf(subject.ID, subject.TeamID, tasks, visible, pFrom, pTo, today)

	blocked := []map[string]any{}
	for _, t := range tasks {
		p := projMap[t.ProjectID]
		if p == nil {
			continue
		}
		if st := p.FindStatus(t.Status); st != nil && st.Tone == "blocked" {
			blocked = append(blocked, store.CleanMap(map[string]any{
				"task_id":    t.ID,
				"code":       t.Code,
				"title":      t.Title,
				"reason":     t.BlockedReason,
				"project_id": t.ProjectID,
			}))
		}
	}

	canRev := access.CanReview(u, subject, allProjects) && s.advanced.RoleMay(r.Context(), u, "write_reviews")
	httpx.Success(w, map[string]any{
		"perf":       perf,
		"blocked":    blocked,
		"reviews":    reviews,
		"can_review": canRev,
	})
}

func (s *Service) HandleCriteria(w http.ResponseWriter, r *http.Request) {
	httpx.Success(w, s.formatCriteria(r.Context()))
}

func (s *Service) HandleSaveCriteria(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.performance.criteria_forbidden"))
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rawItems, _ := body["items"].([]any)
	clean := make([]map[string]any, 0, len(rawItems))
	var toParts []string
	for i, ri := range rawItems {
		m, _ := ri.(map[string]any)
		kind := "manual"
		if strVal(m["kind"]) == "auto" {
			kind = "auto"
		}
		id := strVal(m["id"])
		if id == "" {
			id = fmt.Sprintf("c%s%d", strconv.FormatInt(time.Now().UnixMilli(), 36), i)
		}
		wVal, hasW := toIntStrict(m["weight"])
		if !hasW {
			wVal = 0
		}
		nm := strings.TrimSpace(strVal(m["name"]))
		nmEn := strings.TrimSpace(strVal(m["name_en"]))
		item := map[string]any{
			"id":      id,
			"name":    nm,
			"name_en": nmEn,
			"kind":    kind,
			"weight":  wVal,
		}
		if kind == "auto" && strVal(m["metric"]) != "" {
			item["metric"] = strVal(m["metric"])
		}
		clean = append(clean, item)
		toParts = append(toParts, fmt.Sprintf("%s %d", nm, wVal))
	}

	if prob := criteriaProblem(clean); prob != "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("items", "api.performance.criteria_"+prob))
		return
	}

	cur := s.formatCriteria(r.Context())
	var fromParts []string
	if oldItems, ok := cur["items"].([]any); ok {
		for _, oi := range oldItems {
			if om, ok := oi.(map[string]any); ok {
				w, _ := toIntStrict(om["weight"])
				fromParts = append(fromParts, fmt.Sprintf("%s %d", strVal(om["name"]), w))
			}
		}
	}

	fromStr := strings.Join(fromParts, "، ")
	toStr := strings.Join(toParts, "، ")
	cleanJSON, _ := json.Marshal(clean)
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE performance_criteria SET items = ?, approved_by = NULL, approved_at = NULL, updated_at = ? WHERE id = 1
	`, string(cleanJSON), nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(r.Context(), &u.ID, "criteria_changed", "criteria", nil, "معايير التقييم", []audit.Change{
		{Field: "items", From: &fromStr, To: &toStr},
	})
	httpx.Success(w, s.formatCriteria(r.Context()))
}

func (s *Service) HandleApproveCriteria(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.performance.criteria_approve_forbidden"))
		return
	}
	cur := s.formatCriteria(r.Context())
	var items []map[string]any
	if rawArr, ok := cur["items"].([]any); ok {
		for _, x := range rawArr {
			if m, ok := x.(map[string]any); ok {
				items = append(items, m)
			}
		}
	}
	if criteriaProblem(items) != "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("items", "api.performance.criteria_fix_before_approve"))
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err := s.db.ExecContext(r.Context(), `UPDATE performance_criteria SET approved_by = ?, approved_at = ?, updated_at = ? WHERE id = 1`, u.ID, nowStr, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(r.Context(), &u.ID, "criteria_approved", "criteria", nil, "معايير التقييم", nil)
	httpx.Success(w, s.formatCriteria(r.Context()))
}

func (s *Service) HandleCreateReview(w http.ResponseWriter, r *http.Request) {
	s.saveReviewHandler(w, r, nil)
}

func (s *Service) HandleUpdateReview(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.saveReviewHandler(w, r, &id)
}

func (s *Service) saveReviewHandler(w http.ResponseWriter, r *http.Request, reviewID *int64) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "reviews"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "write_reviews"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	allProjects, err := access.LoadAllProjects(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	subjectID := toInt64Val(input["user_id"])
	subject, _ := store.FindUserByID(r.Context(), s.db, "", subjectID)
	if subject == nil || !access.CanReview(u, subject, allProjects) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.performance.review_forbidden"))
		return
	}

	perMap, _ := input["period"].(map[string]any)
	pFrom, pTo, err := checkPeriod(strVal(perMap["from"]), strVal(perMap["to"]))
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	today := support.BaghdadToday()
	if pTo > today {
		httpx.HandleErr(w, r, httpx.ErrValidation("period", "api.performance.period_not_ended"))
		return
	}
	note := strings.TrimSpace(strVal(input["note"]))
	if support.RuneLen(note) > 3000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.performance.note_max"))
		return
	}

	criteria := s.rawCriteriaItems(r.Context())
	starsMap, _ := input["stars"].(map[string]any)
	for _, c := range criteria {
		if strVal(c["kind"]) != "auto" {
			cid := strVal(c["id"])
			if rawVal, exists := starsMap[cid]; exists && rawVal != nil {
				stVal, ok := toIntStrict(rawVal)
				if !ok || stVal < 1 || stVal > 5 {
					httpx.HandleErr(w, r, httpx.ErrValidation("stars."+cid, "api.performance.stars_range"))
					return
				}
			}
		}
	}

	var visible []*access.Project
	var visIDs []int64
	for _, p := range allProjects {
		if access.CanSeeProject(u, p) {
			visible = append(visible, p)
			visIDs = append(visIDs, p.ID)
		}
	}
	tasks, _ := LoadTasksForProjects(r.Context(), s.db, visIDs, &subject.ID)
	perf := ComputePersonPerf(subject.ID, subject.TeamID, tasks, visible, pFrom, pTo, today)
	scores, total := scoreReview(criteria, perf, starsMap)

	scoresJSON, _ := json.Marshal(scores)
	criteriaJSON, _ := json.Marshal(criteria)
	perfJSON, _ := json.Marshal(perf)
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var outID int64

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if reviewID != nil {
			var revID int64
			var reviewerID int64
			var state string
			qerr := tx.QueryRowContext(r.Context(), `SELECT id, reviewer_id, state FROM performance_reviews WHERE id = ?`+s.db.ForUpdate(), *reviewID).
				Scan(&revID, &reviewerID, &state)
			if qerr != nil || reviewerID != u.ID {
				return httpx.ErrNotFound("api.performance.review_not_found")
			}
			if state == "final" {
				return httpx.ErrConflict("api.performance.review_final_immutable")
			}
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE performance_reviews SET period_from = ?, period_to = ?, scores = ?, criteria = ?, total = ?, note = ?, perf = ?, updated_at = ? WHERE id = ?
			`, pFrom, pTo, string(scoresJSON), string(criteriaJSON), total, note, string(perfJSON), nowStr, revID); err != nil {
				return err
			}
			outID = revID
		} else {
			res, err := tx.ExecContext(r.Context(), `
				INSERT INTO performance_reviews (user_id, reviewer_id, period_from, period_to, scores, criteria, total, note, perf, state, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)
			`, subject.ID, u.ID, pFrom, pTo, string(scoresJSON), string(criteriaJSON), total, note, string(perfJSON), nowStr, nowStr)
			if err != nil {
				return err
			}
			outID, _ = res.LastInsertId()
		}

		_, _ = tx.ExecContext(r.Context(), `DELETE FROM performance_review_scores WHERE review_id = ?`, outID)
		for _, c := range criteria {
			cid := strVal(c["id"])
			wVal, _ := toIntStrict(c["weight"])
			var metric *string
			if m := strVal(c["metric"]); m != "" {
				metric = &m
			}
			var starsPtr *int
			if strVal(c["kind"]) != "auto" {
				if v, ok := toIntStrict(starsMap[cid]); ok {
					starsPtr = &v
				}
			}
			_, err := tx.ExecContext(r.Context(), `
				INSERT INTO performance_review_scores (review_id, criterion_id, criterion_name, criterion_name_en, kind, metric, weight, stars, score)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, outID, cid, strVal(c["name"]), strVal(c["name_en"]), strVal(c["kind"]), metric, wVal, starsPtr, scores[cid])
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	row := s.db.QueryRowContext(r.Context(), `
		SELECT id, user_id, reviewer_id, period_from, period_to, scores, criteria, total, note, perf, state, finalized_at, created_at
		FROM performance_reviews WHERE id = ?
	`, outID)
	rev, _ := scanReview(row)
	status := http.StatusCreated
	if reviewID != nil {
		status = http.StatusOK
	}
	httpx.Success(w, rev, status)
}

func (s *Service) HandleFinalizeReview(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "reviews"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "write_reviews"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var subjID int64
	var total int
	var pFrom, pTo string

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		var reviewerID int64
		var state, scoresRaw, critRaw string
		var pfRaw, ptRaw any
		qerr := tx.QueryRowContext(r.Context(), `
			SELECT user_id, reviewer_id, period_from, period_to, scores, criteria, total, state
			FROM performance_reviews WHERE id = ?`+s.db.ForUpdate(), id).
			Scan(&subjID, &reviewerID, &pfRaw, &ptRaw, &scoresRaw, &critRaw, &total, &state)
		if qerr != nil || reviewerID != u.ID {
			return httpx.ErrNotFound("api.performance.review_not_found")
		}
		if state == "final" {
			return httpx.ErrConflict("api.performance.review_already_final")
		}
		var appRaw any
		_ = tx.QueryRowContext(r.Context(), `SELECT approved_at FROM performance_criteria WHERE id = 1`).Scan(&appRaw)
		if dbx.ParseTime(appRaw) == nil {
			return httpx.ErrConflict("api.performance.criteria_not_approved")
		}
		var scores map[string]int
		var criteria []map[string]any
		_ = json.Unmarshal([]byte(scoresRaw), &scores)
		_ = json.Unmarshal([]byte(critRaw), &criteria)
		for _, c := range criteria {
			if strVal(c["kind"]) != "auto" && scores[strVal(c["id"])] == 0 {
				return httpx.ErrValidation("stars", "api.performance.stars_missing")
			}
		}
		if dp := dbx.ParseDateOnly(pfRaw); dp != nil {
			pFrom = *dp
		}
		if dp := dbx.ParseDateOnly(ptRaw); dp != nil {
			pTo = *dp
		}
		_, err := tx.ExecContext(r.Context(), `UPDATE performance_reviews SET state = 'final', finalized_at = ?, updated_at = ? WHERE id = ?`, nowStr, nowStr, id)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	subjName := "-"
	_ = s.db.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, subjID).Scan(&subjName)
	totStr := strconv.Itoa(total)
	s.audit.Log(r.Context(), &u.ID, "review_finalized", "review", &id, subjName, []audit.Change{{Field: "total", To: &totStr}})
	s.notifications.Notify(r.Context(), []int64{subjID}, "review_final", notifications.About{
		ActorID: &u.ID,
		Data:    map[string]any{"note": pFrom + " → " + pTo},
	})

	row := s.db.QueryRowContext(r.Context(), `
		SELECT id, user_id, reviewer_id, period_from, period_to, scores, criteria, total, note, perf, state, finalized_at, created_at
		FROM performance_reviews WHERE id = ?
	`, id)
	rev, _ := scanReview(row)
	httpx.Success(w, rev)
}

// Helpers

func buildTeamPerf(teamID int64, name string, nameEn *string, users []*store.User, people []map[string]any, projects []*access.Project, tasks []TaskLite, periodTo string) map[string]any {
	memberSet := map[int64]bool{}
	membersCount := 0
	for _, u := range users {
		if u.TeamID != nil && *u.TeamID == teamID {
			memberSet[u.ID] = true
			if u.Active {
				membersCount++
			}
		}
	}
	var assigned, done, late, blocked int
	for _, row := range people {
		if tid, ok := row["team_id"].(*int64); ok && tid != nil && *tid == teamID {
			assigned += intVal(row["assigned"])
			done += intVal(row["done"])
			late += intVal(row["late"])
			blocked += intVal(row["blocked"])
		}
	}
	projHit := map[int64]bool{}
	for _, t := range tasks {
		if t.AssigneeID != nil && memberSet[*t.AssigneeID] {
			projHit[t.ProjectID] = true
		}
	}
	planned := 0.0
	cnt := 0
	for _, p := range projects {
		if projHit[p.ID] {
			planned += support.PlannedRatio(p.Start, p.End, periodTo)
			cnt++
		}
	}
	if cnt > 0 {
		planned = planned / float64(cnt)
	}
	comp := 0.0
	if assigned > 0 {
		comp = float64(done) / float64(assigned)
	}
	return store.CleanMap(map[string]any{
		"team_id":    teamID,
		"name":       name,
		"name_en":    nameEn,
		"members":    membersCount,
		"assigned":   assigned,
		"done":       done,
		"left":       assigned - done,
		"late":       late,
		"blocked":    blocked,
		"completion": comp,
		"planned":    planned,
	})
}

func (s *Service) formatCriteria(ctx context.Context) map[string]any {
	var itemsRaw string
	var appBy sql.NullInt64
	var appAtRaw, upAtRaw any
	err := s.db.QueryRowContext(ctx, `SELECT items, approved_by, approved_at, updated_at FROM performance_criteria WHERE id = 1`).
		Scan(&itemsRaw, &appBy, &appAtRaw, &upAtRaw)
	if err != nil {
		return map[string]any{"items": []any{}}
	}
	items := access.DecodeJSONArray(itemsRaw)
	var byPtr *int64
	if appBy.Valid {
		v := appBy.Int64
		byPtr = &v
	}
	return store.CleanMap(map[string]any{
		"items":       items,
		"approved_by": byPtr,
		"approved_at": store.IsoTime(dbx.ParseTime(appAtRaw)),
		"updated_at":  store.IsoTime(dbx.ParseTime(upAtRaw)),
	})
}

func (s *Service) rawCriteriaItems(ctx context.Context) []map[string]any {
	var itemsRaw string
	if s.db.QueryRowContext(ctx, `SELECT items FROM performance_criteria WHERE id = 1`).Scan(&itemsRaw) != nil {
		return nil
	}
	var out []map[string]any
	_ = json.Unmarshal([]byte(itemsRaw), &out)
	return out
}

func scoreReview(criteria []map[string]any, perf map[string]any, stars map[string]any) (map[string]int, int) {
	scores := map[string]int{}
	weightedSum := 0.0
	for _, c := range criteria {
		cid := strVal(c["id"])
		var score int
		if strVal(c["kind"]) == "auto" && strVal(c["metric"]) != "" {
			score = metricScore(strVal(c["metric"]), perf)
		} else {
			sv, _ := toIntStrict(stars[cid])
			score = sv * 20
		}
		scores[cid] = score
		w, _ := toIntStrict(c["weight"])
		weightedSum += float64(score*w) / 100.0
	}
	return scores, int(math.Round(weightedSum))
}

func metricScore(metric string, perf map[string]any) int {
	comp, _ := perf["completion"].(float64)
	switch metric {
	case "completion":
		return int(math.Round(comp * 100.0))
	case "on_time":
		if ot, ok := perf["on_time"].(float64); ok {
			return int(math.Round(ot * 100.0))
		}
		return int(math.Round(comp * 100.0))
	default:
		assigned := intVal(perf["assigned"])
		late := intVal(perf["late"])
		if assigned > 0 {
			return int(math.Round((1.0 - float64(late)/float64(assigned)) * 100.0))
		}
		return 100
	}
}

func criteriaProblem(items []map[string]any) string {
	if len(items) == 0 {
		return "empty"
	}
	sum := 0
	hasManual := false
	for _, c := range items {
		if strings.TrimSpace(strVal(c["name"])) == "" || strings.TrimSpace(strVal(c["name_en"])) == "" {
			return "name"
		}
		w, ok := toIntStrict(c["weight"])
		if !ok || w <= 0 {
			return "weight"
		}
		sum += w
		kind := strVal(c["kind"])
		if kind == "" {
			kind = "manual"
		}
		if kind == "manual" {
			hasManual = true
		}
		if kind == "auto" {
			m := strVal(c["metric"])
			if m != "completion" && m != "on_time" && m != "not_late" {
				return "metric"
			}
		}
	}
	if sum != 100 {
		return "sum"
	}
	if !hasManual {
		return "only_auto"
	}
	return ""
}

func checkPeriod(from, to string) (string, string, error) {
	if !support.IsRealDay(from) || !support.IsRealDay(to) {
		return "", "", httpx.ErrValidation("period", "api.performance.period_invalid")
	}
	if from > to {
		return "", "", httpx.ErrValidation("period", "api.performance.period_order")
	}
	return from, to, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanReview(sc scanner) (map[string]any, bool) {
	var id, userID, reviewerID int64
	var pfRaw, ptRaw, finRaw, crRaw any
	var scoresRaw, criteriaRaw, note, perfRaw, state string
	var total int
	if sc.Scan(&id, &userID, &reviewerID, &pfRaw, &ptRaw, &scoresRaw, &criteriaRaw, &total, &note, &perfRaw, &state, &finRaw, &crRaw) != nil {
		return nil, false
	}
	scores := map[string]any{}
	_ = json.Unmarshal([]byte(scoresRaw), &scores)
	criteria := []any{}
	_ = json.Unmarshal([]byte(criteriaRaw), &criteria)
	var perf any
	if strings.TrimSpace(perfRaw) != "" {
		var pm map[string]any
		if json.Unmarshal([]byte(perfRaw), &pm) == nil {
			perf = pm
		}
	}
	var pf, pt string
	if p := dbx.ParseDateOnly(pfRaw); p != nil {
		pf = *p
	}
	if p := dbx.ParseDateOnly(ptRaw); p != nil {
		pt = *p
	}
	return store.CleanMap(map[string]any{
		"id":           id,
		"user_id":      userID,
		"reviewer_id":  reviewerID,
		"period":       map[string]any{"from": pf, "to": pt},
		"scores":       scores,
		"criteria":     criteria,
		"total":        total,
		"note":         note,
		"perf":         perf,
		"state":        state,
		"created_at":   store.IsoTime(dbx.ParseTime(crRaw)),
		"finalized_at": store.IsoTime(dbx.ParseTime(finRaw)),
	}), true
}

func nullStrPtr(s sql.NullString) *string {
	if s.Valid && s.String != "" {
		v := s.String
		return &v
	}
	return nil
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intVal(v any) int {
	if n, ok := toIntStrict(v); ok {
		return n
	}
	return 0
}

func toIntStrict(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if float64(int(t)) == t {
			return int(t), true
		}
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func toInt64Val(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	}
	return 0
}
