package ratings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/audit"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

var ReasonTypes = []string{"quality", "deadlines", "teamwork", "attitude", "incident", "other"}

var ScoreWordsAr = map[int]string{
	1: "ضعيف", 2: "مقبول", 3: "جيد", 4: "جيد جداً", 5: "ممتاز",
}

type Service struct {
	db            *dbx.DB
	cfg           config.Config
	audit         *audit.Service
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, cfg config.Config, auditSvc *audit.Service, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, cfg: cfg, audit: auditSvc, notifications: notifSvc, advanced: advSvc}
}

type ratingRow struct {
	id            int64
	subjectType   string
	subjectID     int64
	raterID       int64
	score         int
	reasonType    string
	reason        string
	projectID     *int64
	projectName   *string
	projectNameEn *string
	periodFrom    *string
	periodTo      *string
	private       bool
	createdAt     *time.Time
	editedAt      *time.Time
	editedBy      *int64
}

func (r ratingRow) toJSON() map[string]any {
	var period map[string]any
	if r.periodFrom != nil && r.periodTo != nil {
		period = map[string]any{"from": *r.periodFrom, "to": *r.periodTo}
	}
	return store.CleanMap(map[string]any{
		"id":              r.id,
		"subject_type":    r.subjectType,
		"subject_id":      r.subjectID,
		"rater_id":        r.raterID,
		"score":           r.score,
		"reason_type":     r.reasonType,
		"reason":          r.reason,
		"project_id":      r.projectID,
		"project_name":    r.projectName,
		"project_name_en": r.projectNameEn,
		"period":          period,
		"private":         r.private,
		"created_at":      store.IsoTime(r.createdAt),
		"edited_at":       store.IsoTime(r.editedAt),
		"edited_by":       r.editedBy,
	})
}

func (s *Service) HandleTeamSummaries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	var (
		teams          []store.Team
		users          []*store.User
		projects       []*access.Project
		allTeamRatings map[int64][]ratingRow
	)
	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); teams = s.loadTeams(ctx) }()
		go func() { defer wg.Done(); users, _ = store.LoadAllUsers(ctx, s.db, "") }()
		go func() { defer wg.Done(); projects, _ = access.LoadAllProjects(ctx, s.db) }()
		go func() { defer wg.Done(); allTeamRatings = s.loadRatingsGrouped(ctx, "team") }()
		wg.Wait()
	} else {
		teams = s.loadTeams(ctx)
		users, _ = store.LoadAllUsers(ctx, s.db, "")
		projects, _ = access.LoadAllProjects(ctx, s.db)
		allTeamRatings = s.loadRatingsGrouped(ctx, "team")
	}
	var activeUsers []*store.User
	for _, usr := range users {
		if usr.Active {
			activeUsers = append(activeUsers, usr)
		}
	}
	mayGive := s.advanced.RoleMay(ctx, u, "give_ratings")

	rows := []map[string]any{}
	for _, t := range teams {
		if !access.CanSeeTeam(u, t.ID) {
			continue
		}
		memberCount := 0
		for _, au := range activeUsers {
			if au.TeamID != nil && *au.TeamID == t.ID {
				memberCount++
			}
		}
		sees := access.CanSeeTeamRatings(u, t.ID, activeUsers, projects)
		rates := access.CanRateTeam(u, t.ID, activeUsers, projects)
		var summary any = nil
		if sees {
			var vis []ratingRow
			for _, rt := range allTeamRatings[t.ID] {
				if u.IsCEO() || !rt.private || rt.raterID == u.ID {
					vis = append(vis, rt)
				}
			}
			summary = summarize(vis)
		}
		rows = append(rows, map[string]any{
			"team":         teamJSON(t),
			"member_count": memberCount,
			"ratings":      summary,
			"can_rate":     rates && mayGive,
		})
	}
	httpx.Success(w, rows)
}

func (s *Service) HandleTeamDetail(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	tid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t := s.findTeam(r.Context(), tid)
	if t == nil || !access.CanSeeTeam(u, t.ID) {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.ratings.team_not_found"))
		return
	}
	users, _ := store.LoadAllUsers(r.Context(), s.db, s.cfg.AppURL)
	var activeUsers, teamMembers []*store.User
	for _, usr := range users {
		if usr.Active {
			activeUsers = append(activeUsers, usr)
			if usr.TeamID != nil && *usr.TeamID == t.ID {
				teamMembers = append(teamMembers, usr)
			}
		}
	}
	sort.SliceStable(teamMembers, func(i, j int) bool {
		aLead := t.LeadID != nil && teamMembers[i].ID == *t.LeadID
		bLead := t.LeadID != nil && teamMembers[j].ID == *t.LeadID
		if aLead != bLead {
			return aLead
		}
		return teamMembers[i].Name < teamMembers[j].Name
	})

	projects, _ := access.LoadAllProjects(r.Context(), s.db)
	canSeeRatings := access.CanSeeTeamRatings(u, t.ID, activeUsers, projects)

	var tasksSummary any = nil
	if u.IsCEO() || u.Role == "pm" || u.Role == "monitor" || u.Role == "lead" {
		counts := map[string]int{"open": 0, "late": 0, "blocked": 0, "done": 0}
		var memberIDs []int64
		for _, m := range teamMembers {
			memberIDs = append(memberIDs, m.ID)
		}
		visProj := map[int64]*access.Project{}
		var visIDs []int64
		for _, p := range projects {
			if access.CanSeeProject(u, p) {
				visProj[p.ID] = p
				visIDs = append(visIDs, p.ID)
			}
		}
		if len(memberIDs) > 0 && len(visIDs) > 0 {
			day := support.BaghdadToday()
			phP, argsP := dbx.InInt64(visIDs)
			phM, argsM := dbx.InInt64(memberIDs)
			allArgs := append(argsP, argsM...)
			rows, _ := s.db.QueryContext(r.Context(), fmt.Sprintf(`SELECT project_id, status, due FROM tasks WHERE project_id IN (%s) AND assignee_id IN (%s)`, phP, phM), allArgs...)
			if rows != nil {
				for rows.Next() {
					var pid int64
					var st string
					var dueRaw any
					if rows.Scan(&pid, &st, &dueRaw) == nil {
						p := visProj[pid]
						if p == nil {
							continue
						}
						stObj := p.FindStatus(st)
						tone := ""
						if stObj != nil {
							tone = stObj.Tone
						}
						if tone == "done" {
							counts["done"]++
							continue
						}
						counts["open"]++
						if tone == "blocked" {
							counts["blocked"]++
						}
						if d := dbx.ParseDateOnly(dueRaw); d != nil && *d < day {
							counts["late"]++
						}
					}
				}
				_ = rows.Close()
			}
		}
		tasksSummary = counts
	}

	memOut := make([]map[string]any, 0, len(teamMembers))
	for _, m := range teamMembers {
		memOut = append(memOut, store.UserJSON(m, u))
	}
	httpx.Success(w, map[string]any{
		"team":            teamJSON(*t),
		"members":         memOut,
		"can_see_ratings": canSeeRatings,
		"tasks":           tasksSummary,
	})
}

func (s *Service) HandlePeopleSummaries(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	users, _ := store.LoadAllUsers(r.Context(), s.db, "")
	projects, _ := access.LoadAllProjects(r.Context(), s.db)
	allRatings := s.loadRatingsGrouped(r.Context(), "person")
	mayGive := s.advanced.RoleMay(r.Context(), u, "give_ratings")

	rows := []map[string]any{}
	for _, person := range users {
		if !person.Active || person.Role == "ceo" || person.Role == "admin" {
			continue
		}
		if !access.CanSeePersonRatings(u, person, projects) {
			continue
		}
		var vis []ratingRow
		for _, rt := range allRatings[person.ID] {
			if u.IsCEO() || !rt.private || rt.raterID == u.ID {
				vis = append(vis, rt)
			}
		}
		rows = append(rows, map[string]any{
			"user_id":  person.ID,
			"ratings":  summarize(vis),
			"can_rate": access.CanRatePerson(u, person, projects) && mayGive,
		})
	}
	httpx.Success(w, rows)
}

func (s *Service) HandleList(subjectType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := httpx.UserFrom(r.Context())
		sid, err := httpx.URLInt64(r, "id")
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		sc, err := s.subjectAccess(r.Context(), u, subjectType, sid)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		if !sc.sees {
			httpx.HandleErr(w, r, httpx.ErrForbidden("api.ratings.forbidden_read"))
			return
		}
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT id, subject_type, subject_id, rater_id, score, reason_type, reason, project_id, project_name, project_name_en, period_from, period_to, private, created_at, edited_at, edited_by
			FROM ratings WHERE subject_type = ? AND subject_id = ? ORDER BY created_at DESC, id DESC
		`, subjectType, sid)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			if rt, serr := scanRating(rows); serr == nil {
				if u.IsCEO() || !rt.private || rt.raterID == u.ID {
					items = append(items, rt.toJSON())
				}
			}
		}
		httpx.Success(w, map[string]any{
			"items":    items,
			"can_rate": sc.rates && s.advanced.RoleMay(r.Context(), u, "give_ratings"),
		})
	}
}

func (s *Service) HandleCreate(subjectType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := httpx.UserFrom(r.Context())
		sid, err := httpx.URLInt64(r, "id")
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		sc, err := s.subjectAccess(r.Context(), u, subjectType, sid)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		if !sc.rates {
			httpx.HandleErr(w, r, httpx.ErrForbidden("api.ratings.forbidden_rate"))
			return
		}
		if err := s.advanced.RequireAction(r.Context(), u, "give_ratings"); err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		input, err := httpx.DecodeMap(r)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		prep, err := s.validatePrepare(r.Context(), u, input)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		now := time.Now().UTC()
		nowStr := dbx.FormatDBTime(now)
		privInt := 0
		if prep.private {
			privInt = 1
		}
		res, err := s.db.ExecContext(r.Context(), `
			INSERT INTO ratings (subject_type, subject_id, rater_id, score, reason_type, reason, project_id, project_name, project_name_en, period_from, period_to, private, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, subjectType, sid, u.ID, prep.score, prep.reasonType, prep.reason, prep.projectID, prep.projectName, prep.projectNameEn, prep.periodFrom, prep.periodTo, privInt, nowStr)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		rid, _ := res.LastInsertId()
		rt := ratingRow{
			id: rid, subjectType: subjectType, subjectID: sid, raterID: u.ID,
			score: prep.score, reasonType: prep.reasonType, reason: prep.reason,
			projectID: prep.projectID, projectName: prep.projectName, projectNameEn: prep.projectNameEn,
			periodFrom: prep.periodFrom, periodTo: prep.periodTo, private: prep.private, createdAt: &now,
		}
		s.tell(r.Context(), rt, sc, u.ID)
		httpx.Success(w, rt.toJSON(), http.StatusCreated)
	}
}

func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rt, sc, err := s.ratingForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	canChange := u.IsCEO() || (rt.raterID == u.ID && rt.createdAt != nil && rt.createdAt.Add(24*time.Hour).After(now))
	if rt.raterID != u.ID && !canChange {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.ratings.edit_writer_only"))
		return
	}
	if !canChange {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.ratings.edit_window_expired"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	prep, err := s.validatePrepare(r.Context(), u, input)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	beforeScore, beforeType, beforeReason, beforePriv := rt.score, rt.reasonType, rt.reason, rt.private
	var editedBy *int64
	if rt.raterID != u.ID {
		editedBy = &u.ID
	}
	nowStr := dbx.FormatDBTime(now)
	privInt := 0
	if prep.private {
		privInt = 1
	}
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE ratings SET score = ?, reason_type = ?, reason = ?, project_id = ?, project_name = ?, project_name_en = ?, period_from = ?, period_to = ?, private = ?, edited_at = ?, edited_by = ?
		WHERE id = ?
	`, prep.score, prep.reasonType, prep.reason, prep.projectID, prep.projectName, prep.projectNameEn, prep.periodFrom, prep.periodTo, privInt, nowStr, editedBy, rt.id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if rt.raterID != u.ID {
		var changes []audit.Change
		if beforeScore != prep.score {
			f, t := scoreText(beforeScore), scoreText(prep.score)
			changes = append(changes, audit.Change{Field: "score", From: &f, To: &t})
		}
		if beforeType != prep.reasonType {
			changes = append(changes, audit.Change{Field: "reason_type", From: &beforeType, To: &prep.reasonType})
		}
		r1, r2 := support.TruncateRunes(beforeReason, 200), support.TruncateRunes(prep.reason, 200)
		if r1 != r2 {
			changes = append(changes, audit.Change{Field: "reason", From: &r1, To: &r2})
		}
		if beforePriv != prep.private {
			f := strconv.FormatBool(beforePriv)
			t := strconv.FormatBool(prep.private)
			changes = append(changes, audit.Change{Field: "private", From: &f, To: &t})
		}
		tType := "user"
		if rt.subjectType == "team" {
			tType = "team"
		}
		s.audit.Log(r.Context(), &u.ID, "rating_changed", tType, &rt.subjectID, sc.label, changes)
	}

	rt.score = prep.score
	rt.reasonType = prep.reasonType
	rt.reason = prep.reason
	rt.projectID = prep.projectID
	rt.projectName = prep.projectName
	rt.projectNameEn = prep.projectNameEn
	rt.periodFrom = prep.periodFrom
	rt.periodTo = prep.periodTo
	rt.private = prep.private
	rt.editedAt = &now
	rt.editedBy = editedBy
	if beforePriv && !rt.private {
		s.tell(r.Context(), rt, sc, u.ID)
	}
	httpx.Success(w, rt.toJSON())
}

func (s *Service) HandleDelete(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rt, sc, err := s.ratingForOrFail(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	canChange := u.IsCEO() || (rt.raterID == u.ID && rt.createdAt != nil && rt.createdAt.Add(24*time.Hour).After(now))
	if rt.raterID != u.ID && !canChange {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.ratings.delete_writer_only"))
		return
	}
	if !canChange {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.ratings.delete_window_expired"))
		return
	}
	raterName := "-"
	_ = s.db.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, rt.raterID).Scan(&raterName)
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM ratings WHERE id = ?`, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	scText := scoreText(rt.score)
	rTrunc := support.TruncateRunes(rt.reason, 200)
	tType := "user"
	if rt.subjectType == "team" {
		tType = "team"
	}
	s.audit.Log(r.Context(), &u.ID, "rating_deleted", tType, &rt.subjectID, sc.label, []audit.Change{
		{Field: "score", From: &scText},
		{Field: "reason_type", From: &rt.reasonType},
		{Field: "reason", From: &rTrunc},
		{Field: "rater", From: &raterName},
	})
	httpx.Success(w, nil)
}

// Helpers

type subjectCtx struct {
	team   *store.Team
	person *store.User
	label  string
	sees   bool
	rates  bool
}

func (s *Service) subjectAccess(ctx context.Context, u *store.User, subjectType string, sid int64) (subjectCtx, error) {
	projects, _ := access.LoadAllProjects(ctx, s.db)
	if subjectType == "team" {
		t := s.findTeam(ctx, sid)
		if t == nil || !access.CanSeeTeam(u, t.ID) {
			return subjectCtx{}, httpx.ErrNotFound("api.ratings.team_not_found")
		}
		users, _ := store.LoadAllUsers(ctx, s.db, "")
		var active []*store.User
		for _, usr := range users {
			if usr.Active {
				active = append(active, usr)
			}
		}
		return subjectCtx{
			team: t, label: t.Name,
			sees:  access.CanSeeTeamRatings(u, t.ID, active, projects),
			rates: access.CanRateTeam(u, t.ID, active, projects),
		}, nil
	}
	person, _ := store.FindUserByID(ctx, s.db, "", sid)
	if person == nil {
		return subjectCtx{}, httpx.ErrNotFound("api.not_found")
	}
	return subjectCtx{
		person: person, label: person.Name,
		sees:  access.CanSeePersonRatings(u, person, projects),
		rates: access.CanRatePerson(u, person, projects) && person.Active,
	}, nil
}

func (s *Service) ratingForOrFail(ctx context.Context, u *store.User, id int64) (ratingRow, subjectCtx, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, subject_type, subject_id, rater_id, score, reason_type, reason, project_id, project_name, project_name_en, period_from, period_to, private, created_at, edited_at, edited_by
		FROM ratings WHERE id = ?
	`, id)
	rt, err := scanRating(row)
	if err != nil {
		return rt, subjectCtx{}, httpx.ErrNotFound("api.ratings.not_found")
	}
	sc, err := s.subjectAccess(ctx, u, rt.subjectType, rt.subjectID)
	if err != nil || !sc.sees || (!u.IsCEO() && rt.private && rt.raterID != u.ID) {
		return rt, subjectCtx{}, httpx.ErrNotFound("api.ratings.not_found")
	}
	return rt, sc, nil
}

type preparedRating struct {
	score         int
	reasonType    string
	reason        string
	projectID     *int64
	projectName   *string
	projectNameEn *string
	periodFrom    *string
	periodTo      *string
	private       bool
}

func (s *Service) validatePrepare(ctx context.Context, u *store.User, input map[string]any) (preparedRating, error) {
	var out preparedRating
	errs := map[string]string{}
	sc, okSc := toIntStrict(input["score"])
	if !okSc || sc < 1 || sc > 5 {
		errs["score"] = "api.ratings.score_invalid"
	} else {
		out.score = sc
	}
	rt := strVal(input["reason_type"])
	if !store.UserHasRole(rt, ReasonTypes) {
		errs["reason_type"] = "api.ratings.reason_type_invalid"
	} else {
		out.reasonType = rt
	}
	rs := strings.TrimSpace(strVal(input["reason"]))
	if rs == "" {
		errs["reason"] = "api.ratings.reason_required"
	} else if support.RuneLen(rs) > 1000 {
		errs["reason"] = "api.ratings.reason_max"
	} else {
		out.reason = rs
	}
	if pRaw, hasP := input["period"]; hasP && pRaw != nil {
		pm, okP := pRaw.(map[string]any)
		pf, pt := strVal(pm["from"]), strVal(pm["to"])
		if !okP || !support.IsRealDay(pf) || !support.IsRealDay(pt) {
			errs["period"] = "api.ratings.period_invalid"
		} else if pf > pt {
			errs["period"] = "api.ratings.period_order"
		} else {
			out.periodFrom = &pf
			out.periodTo = &pt
		}
	}
	if pidRaw, hasPID := input["project_id"]; hasPID && pidRaw != nil {
		pidInt, okPID := toIntStrict(pidRaw)
		if !okPID || pidInt <= 0 {
			errs["project_id"] = "api.ratings.project_invalid"
		} else {
			p, _ := access.LoadProject(ctx, s.db, int64(pidInt))
			if p == nil || !access.CanSeeProject(u, p) {
				errs["project_id"] = "api.ratings.project_invalid"
			} else {
				out.projectID = &p.ID
				out.projectName = &p.Name
				out.projectNameEn = p.NameEn
			}
		}
	}
	if pv, hasPriv := input["private"]; hasPriv && pv != nil {
		b, okB := pv.(bool)
		if !okB {
			errs["private"] = "api.ratings.private_invalid"
		} else {
			out.private = b
		}
	}
	if len(errs) > 0 {
		for _, firstKey := range errs {
			return out, httpx.ErrValidationMap(firstKey, errs)
		}
	}
	return out, nil
}

func (s *Service) tell(ctx context.Context, rt ratingRow, sc subjectCtx, actorID int64) {
	if rt.private {
		return
	}
	d := map[string]any{
		"score": strconv.Itoa(rt.score),
		"note":  support.TruncateRunes(rt.reason, 140),
	}
	if sc.team != nil && sc.team.LeadID != nil {
		td := map[string]any{"score": d["score"], "note": d["note"], "team_name": sc.team.Name}
		if sc.team.NameEn != nil && *sc.team.NameEn != "" {
			td["team_name_en"] = *sc.team.NameEn
		}
		s.notifications.Notify(ctx, []int64{*sc.team.LeadID}, "team_rated", notifications.About{
			ActorID: &actorID, TeamID: &sc.team.ID, Data: td,
		})
	} else if sc.person != nil {
		s.notifications.Notify(ctx, []int64{sc.person.ID}, "person_rated", notifications.About{
			ActorID: &actorID, Data: d,
		})
	}
}

func summarize(list []ratingRow) map[string]any {
	since := time.Now().UTC().AddDate(0, 0, -90)
	var recent, before []ratingRow
	byType := map[string]int{}
	for _, t := range ReasonTypes {
		byType[t] = 0
	}
	byScore := []int{0, 0, 0, 0, 0}
	for _, r := range list {
		if r.createdAt != nil && !r.createdAt.Before(since) {
			recent = append(recent, r)
		} else {
			before = append(before, r)
		}
		if _, ok := byType[r.reasonType]; ok {
			byType[r.reasonType]++
		}
		if r.score >= 1 && r.score <= 5 {
			byScore[r.score-1]++
		}
	}
	avgOf := func(sub []ratingRow) map[string]any {
		cnt := len(sub)
		var avg any = nil
		if cnt > 0 {
			sum := 0
			for _, x := range sub {
				sum += x.score
			}
			avg = math.Round((float64(sum)/float64(cnt))*10) / 10
		}
		return map[string]any{"count": cnt, "average": avg}
	}
	out := avgOf(list)
	out["recent"] = avgOf(recent)
	out["before"] = avgOf(before)
	out["by_type"] = byType
	out["by_score"] = byScore
	if len(list) > 0 {
		newest := list[0]
		out["last"] = map[string]any{
			"score":       newest.score,
			"reason_type": newest.reasonType,
			"created_at":  store.IsoTime(newest.createdAt),
		}
	}
	return out
}

func (s *Service) loadTeams(ctx context.Context) []store.Team {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, name_en, lead_id FROM teams ORDER BY id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []store.Team
	for rows.Next() {
		var t store.Team
		var ne sql.NullString
		var lid sql.NullInt64
		if rows.Scan(&t.ID, &t.Name, &ne, &lid) == nil {
			if ne.Valid && ne.String != "" {
				v := ne.String
				t.NameEn = &v
			}
			if lid.Valid {
				v := lid.Int64
				t.LeadID = &v
			}
			out = append(out, t)
		}
	}
	return out
}

func (s *Service) findTeam(ctx context.Context, id int64) *store.Team {
	var t store.Team
	var ne sql.NullString
	var lid sql.NullInt64
	if s.db.QueryRowContext(ctx, `SELECT id, name, name_en, lead_id FROM teams WHERE id = ?`, id).Scan(&t.ID, &t.Name, &ne, &lid) != nil {
		return nil
	}
	if ne.Valid && ne.String != "" {
		v := ne.String
		t.NameEn = &v
	}
	if lid.Valid {
		v := lid.Int64
		t.LeadID = &v
	}
	return &t
}

func (s *Service) loadRatingsGrouped(ctx context.Context, subjectType string) map[int64][]ratingRow {
	out := map[int64][]ratingRow{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, subject_type, subject_id, rater_id, score, reason_type, reason, project_id, project_name, project_name_en, period_from, period_to, private, created_at, edited_at, edited_by
		FROM ratings WHERE subject_type = ? ORDER BY created_at DESC, id DESC
	`, subjectType)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		if r, serr := scanRating(rows); serr == nil {
			out[r.subjectID] = append(out[r.subjectID], r)
		}
	}
	return out
}

type scanner interface{ Scan(dest ...any) error }

func scanRating(sc scanner) (ratingRow, error) {
	var r ratingRow
	var pid, eby sql.NullInt64
	var pn, pne sql.NullString
	var pfRaw, ptRaw, crRaw, edRaw any
	var privInt int
	if err := sc.Scan(&r.id, &r.subjectType, &r.subjectID, &r.raterID, &r.score, &r.reasonType, &r.reason, &pid, &pn, &pne, &pfRaw, &ptRaw, &privInt, &crRaw, &edRaw, &eby); err != nil {
		return r, err
	}
	if pid.Valid {
		v := pid.Int64
		r.projectID = &v
	}
	if pn.Valid && pn.String != "" {
		v := pn.String
		r.projectName = &v
	}
	if pne.Valid && pne.String != "" {
		v := pne.String
		r.projectNameEn = &v
	}
	r.periodFrom = dbx.ParseDateOnly(pfRaw)
	r.periodTo = dbx.ParseDateOnly(ptRaw)
	r.private = privInt != 0
	r.createdAt = dbx.ParseTime(crRaw)
	r.editedAt = dbx.ParseTime(edRaw)
	if eby.Valid {
		v := eby.Int64
		r.editedBy = &v
	}
	return r, nil
}

func teamJSON(t store.Team) map[string]any {
	return store.CleanMap(map[string]any{
		"id":      t.ID,
		"name":    t.Name,
		"name_en": t.NameEn,
		"lead_id": t.LeadID,
	})
}

func scoreText(score int) string {
	return strings.TrimSpace(fmt.Sprintf("%d %s", score, ScoreWordsAr[score]))
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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
