package trials

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
)

type Service struct {
	db *dbx.DB
}

func New(db *dbx.DB) *Service {
	return &Service{db: db}
}

type teamRow struct {
	id        int64
	name      string
	goal      *string
	startedOn string
	status    string
	endedOn   *string
	createdBy int64
	createdAt *time.Time
}

func (t teamRow) toJSON() map[string]any {
	return store.CleanMap(map[string]any{
		"id":         t.id,
		"name":       t.name,
		"goal":       t.goal,
		"started_on": t.startedOn,
		"status":     t.status,
		"ended_on":   t.endedOn,
		"created_by": t.createdBy,
		"created_at": store.IsoTime(t.createdAt),
	})
}

type memberRow struct {
	id        int64
	teamID    int64
	name      string
	specialty string
	phone     *string
	email     *string
	joinedOn  string
	decision  string
	createdAt *time.Time
}

func (m memberRow) toJSON() map[string]any {
	return store.CleanMap(map[string]any{
		"id":         m.id,
		"team_id":    m.teamID,
		"name":       m.name,
		"specialty":  m.specialty,
		"phone":      m.phone,
		"email":      m.email,
		"joined_on":  m.joinedOn,
		"decision":   m.decision,
		"created_at": store.IsoTime(m.createdAt),
	})
}

type entryRow struct {
	id       int64
	memberID int64
	score    *int
	text     *string
	byID     int64
	at       *time.Time
}

func (e entryRow) toJSON() map[string]any {
	return store.CleanMap(map[string]any{
		"id":        e.id,
		"member_id": e.memberID,
		"score":     e.score,
		"text":      e.text,
		"by_id":     e.byID,
		"at":        store.IsoTime(e.at),
	})
}

func (s *Service) HandleIndex(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.trials.forbidden"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, goal, started_on, status, ended_on, created_by, created_at
		FROM trial_teams
		WHERE created_by = ?
		ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at DESC, id DESC
	`, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var teams []teamRow
	var teamIDs []int64
	for rows.Next() {
		if t, serr := scanTeam(rows); serr == nil {
			teams = append(teams, t)
			teamIDs = append(teamIDs, t.id)
		}
	}
	_ = rows.Close()

	membersByTeam := map[int64][]memberRow{}
	entriesByMember := map[int64][]entryRow{}
	if len(teamIDs) > 0 {
		membersByTeam, entriesByMember = s.loadMembersAndEntries(r.Context(), teamIDs)
	}

	out := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		mems := membersByTeam[t.id]
		var scores []int
		entryCount := 0
		accepted := 0
		for _, m := range mems {
			if m.decision == "accepted" {
				accepted++
			}
			for _, e := range entriesByMember[m.id] {
				entryCount++
				if e.score != nil {
					scores = append(scores, *e.score)
				}
			}
		}
		row := t.toJSON()
		row["members"] = len(mems)
		if len(scores) > 0 {
			sum := 0
			for _, sv := range scores {
				sum += sv
			}
			row["average"] = math.Round((float64(sum)/float64(len(scores)))*10) / 10
		}
		row["entries"] = entryCount
		row["accepted"] = accepted
		out = append(out, row)
	}
	httpx.Success(w, out)
}

func (s *Service) HandleCreateTeam(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.trials.forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name, err := checkTeamName(input["name"])
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	goal, err := checkGoal(input)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	startedOn, err := checkPastOrToday(input["started_on"], "started_on", true)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO trial_teams (name, goal, started_on, status, ended_on, created_by, created_at, updated_at)
		VALUES (?, ?, ?, 'running', NULL, ?, ?, ?)
	`, name, goal, *startedOn, u.ID, nowStr, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	id, _ := res.LastInsertId()
	t, _ := s.loadTeamFor(r.Context(), u, id)
	httpx.Success(w, t.toJSON(), http.StatusCreated)
}

func (s *Service) HandleDetail(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, err := s.loadTeamFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	membersByTeam, entriesByMember := s.loadMembersAndEntries(r.Context(), []int64{t.id})
	mems := membersByTeam[t.id]

	var allEntries []entryRow
	memberOut := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		mine := entriesByMember[m.id]
		var scores []int
		notes := 0
		var lastAt *time.Time
		for _, e := range mine {
			allEntries = append(allEntries, e)
			if e.score != nil {
				scores = append(scores, *e.score)
			}
			if e.text != nil && *e.text != "" {
				notes++
			}
			if e.at != nil && (lastAt == nil || e.at.After(*lastAt)) {
				cp := *e.at
				lastAt = &cp
			}
		}
		bm := m.toJSON()
		if len(scores) > 0 {
			sum := 0
			for _, sv := range scores {
				sum += sv
			}
			bm["average"] = math.Round((float64(sum)/float64(len(scores)))*10) / 10
		}
		bm["scores"] = len(scores)
		bm["notes"] = notes
		if lastAt != nil {
			bm["last_at"] = store.IsoTime(lastAt)
		}
		memberOut = append(memberOut, store.CleanMap(bm))
	}

	sort.SliceStable(allEntries, func(i, j int) bool {
		ti := int64(0)
		tj := int64(0)
		if allEntries[i].at != nil {
			ti = allEntries[i].at.Unix()
		}
		if allEntries[j].at != nil {
			tj = allEntries[j].at.Unix()
		}
		if ti != tj {
			return tj < ti
		}
		return allEntries[j].id < allEntries[i].id
	})

	entriesOut := make([]map[string]any, 0, len(allEntries))
	for _, e := range allEntries {
		entriesOut = append(entriesOut, e.toJSON())
	}

	httpx.Success(w, map[string]any{
		"team":    t.toJSON(),
		"members": memberOut,
		"entries": entriesOut,
	})
}

func (s *Service) HandleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, err := s.loadTeamFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	patch, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, ok := patch["name"]; ok {
		nm, err := checkTeamName(patch["name"])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		t.name = nm
	}
	if _, ok := patch["goal"]; ok {
		g, err := checkGoal(patch)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		t.goal = g
	}
	if _, ok := patch["started_on"]; ok {
		so, err := checkPastOrToday(patch["started_on"], "started_on", true)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		t.startedOn = *so
	}
	if _, ok := patch["status"]; ok {
		st := strVal(patch["status"])
		if st != "running" && st != "ended" {
			httpx.HandleErr(w, r, httpx.ErrValidation("status", "api.trials.status_invalid"))
			return
		}
		if st != t.status {
			t.status = st
			if st == "ended" {
				td := support.BaghdadToday()
				t.endedOn = &td
			} else {
				t.endedOn = nil
			}
		}
	}
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE trial_teams SET name = ?, goal = ?, started_on = ?, status = ?, ended_on = ?, updated_at = ? WHERE id = ?
	`, t.name, t.goal, t.startedOn, t.status, t.endedOn, dbx.FormatDBTime(time.Now().UTC()), t.id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	updated, _ := s.loadTeamFor(r.Context(), u, id)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleAddMember(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	tid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	t, err := s.loadTeamFor(r.Context(), u, tid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var cnt int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM trial_members WHERE team_id = ?`, t.id).Scan(&cnt)
	if cnt >= 50 {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.trials.members_limit", map[string]any{"max": 50}))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name, err := checkMemberName(input["name"])
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	spec, err := checkSpecialty(input["specialty"])
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	phone, err := checkPhone(input)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	email, err := checkEmail(input)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	joinedPtr, err := checkPastOrToday(input["joined_on"], "joined_on", false)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	joinedOn := support.BaghdadToday()
	if joinedPtr != nil {
		joinedOn = *joinedPtr
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO trial_members (team_id, name, specialty, phone, email, joined_on, decision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)
	`, t.id, name, spec, phone, email, joinedOn, nowStr, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	mid, _ := res.LastInsertId()
	m, _ := s.loadMemberFor(r.Context(), u, mid)
	httpx.Success(w, m.toJSON(), http.StatusCreated)
}

func (s *Service) HandleUpdateMember(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	mid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	m, err := s.loadMemberFor(r.Context(), u, mid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	patch, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, ok := patch["name"]; ok {
		nm, err := checkMemberName(patch["name"])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		m.name = nm
	}
	if _, ok := patch["specialty"]; ok {
		sp, err := checkSpecialty(patch["specialty"])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		m.specialty = sp
	}
	if _, ok := patch["phone"]; ok {
		ph, err := checkPhone(patch)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		m.phone = ph
	}
	if _, ok := patch["email"]; ok {
		em, err := checkEmail(patch)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		m.email = em
	}
	if j, ok := patch["joined_on"]; ok && j != nil && strVal(j) != "" {
		jp, err := checkPastOrToday(j, "joined_on", true)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		m.joinedOn = *jp
	}
	if _, ok := patch["decision"]; ok {
		dec := strVal(patch["decision"])
		if dec != "pending" && dec != "accepted" && dec != "rejected" {
			httpx.HandleErr(w, r, httpx.ErrValidation("decision", "api.trials.decision_invalid"))
			return
		}
		m.decision = dec
	}
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE trial_members SET name = ?, specialty = ?, phone = ?, email = ?, joined_on = ?, decision = ?, updated_at = ? WHERE id = ?
	`, m.name, m.specialty, m.phone, m.email, m.joinedOn, m.decision, dbx.FormatDBTime(time.Now().UTC()), m.id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	updated, _ := s.loadMemberFor(r.Context(), u, mid)
	httpx.Success(w, updated.toJSON())
}

func (s *Service) HandleDeleteMember(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	mid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := s.loadMemberFor(r.Context(), u, mid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM trial_entries WHERE member_id = ?`, mid)
		_, err := tx.ExecContext(r.Context(), `DELETE FROM trial_members WHERE id = ?`, mid)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, nil)
}

func (s *Service) HandleAddEntry(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	mid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := s.loadMemberFor(r.Context(), u, mid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var scorePtr *int
	if sv, hasScore := input["score"]; hasScore && sv != nil && fmt.Sprint(sv) != "" {
		sc, ok := toIntStrict(sv)
		if !ok || sc < 1 || sc > 5 {
			httpx.HandleErr(w, r, httpx.ErrValidation("score", "api.trials.score_invalid"))
			return
		}
		scorePtr = &sc
	}
	var textPtr *string
	if tv, hasText := input["text"]; hasText && tv != nil {
		tr := strings.TrimSpace(strVal(tv))
		if support.RuneLen(tr) > 1000 {
			httpx.HandleErr(w, r, httpx.ErrValidation("text", "api.trials.text_max", map[string]any{"max": 1000}))
			return
		}
		if tr != "" {
			textPtr = &tr
		}
	}
	if scorePtr == nil && textPtr == nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("text", "api.trials.entry_empty"))
		return
	}
	now := time.Now().UTC()
	nowStr := dbx.FormatDBTime(now)
	res, err := s.db.ExecContext(r.Context(), `INSERT INTO trial_entries (member_id, score, text, by_id, at) VALUES (?, ?, ?, ?, ?)`,
		mid, scorePtr, textPtr, u.ID, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	eid, _ := res.LastInsertId()
	e := entryRow{id: eid, memberID: mid, score: scorePtr, text: textPtr, byID: u.ID, at: &now}
	httpx.Success(w, e.toJSON(), http.StatusCreated)
}

func (s *Service) HandleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.trials.forbidden"))
		return
	}
	eid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var createdBy int64
	err = s.db.QueryRowContext(r.Context(), `
		SELECT tt.created_by
		FROM trial_entries te
		INNER JOIN trial_members tm ON tm.id = te.member_id
		INNER JOIN trial_teams tt ON tt.id = tm.team_id
		WHERE te.id = ?
	`, eid).Scan(&createdBy)
	if err == sql.ErrNoRows || createdBy != u.ID {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.trials.entry_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `DELETE FROM trial_entries WHERE id = ?`, eid)
	httpx.Success(w, nil)
}

func (s *Service) loadTeamFor(ctx context.Context, u *store.User, id int64) (teamRow, error) {
	if u.Role != "monitor" {
		return teamRow{}, httpx.ErrForbidden("api.trials.forbidden")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, name, goal, started_on, status, ended_on, created_by, created_at FROM trial_teams WHERE id = ?`, id)
	t, err := scanTeam(row)
	if err != nil || t.createdBy != u.ID {
		return teamRow{}, httpx.ErrNotFound("api.trials.not_found")
	}
	return t, nil
}

func (s *Service) loadMemberFor(ctx context.Context, u *store.User, mid int64) (memberRow, error) {
	if u.Role != "monitor" {
		return memberRow{}, httpx.ErrForbidden("api.trials.forbidden")
	}
	var m memberRow
	var ph, em sql.NullString
	var jRaw, crRaw any
	var createdBy int64
	err := s.db.QueryRowContext(ctx, `
		SELECT tm.id, tm.team_id, tm.name, tm.specialty, tm.phone, tm.email, tm.joined_on, tm.decision, tm.created_at, tt.created_by
		FROM trial_members tm
		INNER JOIN trial_teams tt ON tt.id = tm.team_id
		WHERE tm.id = ?
	`, mid).Scan(&m.id, &m.teamID, &m.name, &m.specialty, &ph, &em, &jRaw, &m.decision, &crRaw, &createdBy)
	if err != nil || createdBy != u.ID {
		return m, httpx.ErrNotFound("api.trials.member_not_found")
	}
	if ph.Valid && ph.String != "" {
		v := ph.String
		m.phone = &v
	}
	if em.Valid && em.String != "" {
		v := em.String
		m.email = &v
	}
	if jp := dbx.ParseDateOnly(jRaw); jp != nil {
		m.joinedOn = *jp
	}
	m.createdAt = dbx.ParseTime(crRaw)
	return m, nil
}

func (s *Service) loadMembersAndEntries(ctx context.Context, teamIDs []int64) (map[int64][]memberRow, map[int64][]entryRow) {
	mByT := map[int64][]memberRow{}
	eByM := map[int64][]entryRow{}
	ph, args := dbx.InInt64(teamIDs)
	mRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, team_id, name, specialty, phone, email, joined_on, decision, created_at
		FROM trial_members WHERE team_id IN (%s) ORDER BY id ASC
	`, ph), args...)
	var mIDs []int64
	if err == nil {
		for mRows.Next() {
			var m memberRow
			var phStr, emStr sql.NullString
			var jRaw, crRaw any
			if mRows.Scan(&m.id, &m.teamID, &m.name, &m.specialty, &phStr, &emStr, &jRaw, &m.decision, &crRaw) == nil {
				if phStr.Valid && phStr.String != "" {
					v := phStr.String
					m.phone = &v
				}
				if emStr.Valid && emStr.String != "" {
					v := emStr.String
					m.email = &v
				}
				if jp := dbx.ParseDateOnly(jRaw); jp != nil {
					m.joinedOn = *jp
				}
				m.createdAt = dbx.ParseTime(crRaw)
				mByT[m.teamID] = append(mByT[m.teamID], m)
				mIDs = append(mIDs, m.id)
			}
		}
		_ = mRows.Close()
	}
	if len(mIDs) > 0 {
		mph, margs := dbx.InInt64(mIDs)
		eRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT id, member_id, score, text, by_id, at
			FROM trial_entries WHERE member_id IN (%s) ORDER BY at DESC, id DESC
		`, mph), margs...)
		if err == nil {
			for eRows.Next() {
				var e entryRow
				var sc sql.NullInt64
				var tx sql.NullString
				var atRaw any
				if eRows.Scan(&e.id, &e.memberID, &sc, &tx, &e.byID, &atRaw) == nil {
					if sc.Valid {
						v := int(sc.Int64)
						e.score = &v
					}
					if tx.Valid && tx.String != "" {
						v := tx.String
						e.text = &v
					}
					e.at = dbx.ParseTime(atRaw)
					eByM[e.memberID] = append(eByM[e.memberID], e)
				}
			}
			_ = eRows.Close()
		}
	}
	return mByT, eByM
}

type scanner interface{ Scan(dest ...any) error }

func scanTeam(sc scanner) (teamRow, error) {
	var t teamRow
	var goal sql.NullString
	var sRaw, eRaw, crRaw any
	if err := sc.Scan(&t.id, &t.name, &goal, &sRaw, &t.status, &eRaw, &t.createdBy, &crRaw); err != nil {
		return t, err
	}
	if goal.Valid && goal.String != "" {
		v := goal.String
		t.goal = &v
	}
	if sp := dbx.ParseDateOnly(sRaw); sp != nil {
		t.startedOn = *sp
	}
	t.endedOn = dbx.ParseDateOnly(eRaw)
	t.createdAt = dbx.ParseTime(crRaw)
	return t, nil
}

func checkTeamName(raw any) (string, error) {
	nm := strings.TrimSpace(strVal(raw))
	if nm == "" {
		return "", httpx.ErrValidation("name", "api.trials.name_required")
	}
	if support.RuneLen(nm) > 80 {
		return "", httpx.ErrValidation("name", "api.trials.name_max", map[string]any{"max": 80})
	}
	return nm, nil
}

func checkGoal(input map[string]any) (*string, error) {
	v, ok := input["goal"]
	if !ok || v == nil {
		return nil, nil
	}
	tr := strings.TrimSpace(strVal(v))
	if support.RuneLen(tr) > 500 {
		return nil, httpx.ErrValidation("goal", "api.trials.goal_max", map[string]any{"max": 500})
	}
	if tr == "" {
		return nil, nil
	}
	return &tr, nil
}

func checkMemberName(raw any) (string, error) {
	nm := strings.TrimSpace(strVal(raw))
	if nm == "" {
		return "", httpx.ErrValidation("name", "api.trials.member_name_required")
	}
	if support.RuneLen(nm) > 120 {
		return "", httpx.ErrValidation("name", "api.trials.member_name_max", map[string]any{"max": 120})
	}
	return nm, nil
}

func checkSpecialty(raw any) (string, error) {
	sp := strings.TrimSpace(strVal(raw))
	if sp == "" {
		return "", httpx.ErrValidation("specialty", "api.trials.specialty_required")
	}
	if support.RuneLen(sp) > 80 {
		return "", httpx.ErrValidation("specialty", "api.trials.specialty_max", map[string]any{"max": 80})
	}
	return sp, nil
}

func checkPhone(input map[string]any) (*string, error) {
	v, ok := input["phone"]
	if !ok || v == nil {
		return nil, nil
	}
	tr := strings.TrimSpace(strVal(v))
	if tr == "" {
		return nil, nil
	}
	if support.RuneLen(tr) > 30 {
		return nil, httpx.ErrValidation("phone", "api.trials.phone_max", map[string]any{"max": 30})
	}
	return &tr, nil
}

func checkEmail(input map[string]any) (*string, error) {
	v, ok := input["email"]
	if !ok || v == nil {
		return nil, nil
	}
	tr := strings.ToLower(strings.TrimSpace(strVal(v)))
	if tr == "" {
		return nil, nil
	}
	addr, err := mail.ParseAddress(tr)
	if err != nil || addr.Address != tr {
		return nil, httpx.ErrValidation("email", "api.trials.email_invalid")
	}
	return &tr, nil
}

func checkPastOrToday(raw any, field string, required bool) (*string, error) {
	val := strings.TrimSpace(strVal(raw))
	if val == "" {
		if required {
			return nil, httpx.ErrValidation(field, "api.trials.day_invalid")
		}
		return nil, nil
	}
	if !support.IsRealDay(val) || val > support.BaghdadToday() {
		return nil, httpx.ErrValidation(field, "api.trials.day_invalid")
	}
	return &val, nil
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
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}
