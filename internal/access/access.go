package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
)

type ProjectStatus struct {
	ID       int64
	Key      string
	Name     string
	NameEn   string
	Tone     string
	Position int
	IsBase   bool
}

type ProjectPhase struct {
	ID       int64
	Name     string
	Position int
}

type Project struct {
	ID          int64
	Code        string
	Name        string
	NameEn      *string
	Description string
	PMID        int64
	Start       string
	End         string
	State       string
	Priority    string
	UsesSprints bool
	MemberIDs   []int64
	Statuses    []ProjectStatus
	Phases      []ProjectPhase
}

var BaseStatusKeys = []string{"new", "todo", "in_progress", "in_review", "testing", "completed", "blocked"}
var StatusTones = []string{"new", "todo", "progress", "review", "testing", "done", "blocked"}

var DefaultStatuses = []ProjectStatus{
	{Key: "new", Name: "جديدة", NameEn: "New", Tone: "new", IsBase: true},
	{Key: "todo", Name: "قيد الانتظار", NameEn: "To do", Tone: "todo", IsBase: true},
	{Key: "in_progress", Name: "قيد التنفيذ", NameEn: "In progress", Tone: "progress", IsBase: true},
	{Key: "in_review", Name: "قيد المراجعة", NameEn: "In review", Tone: "review", IsBase: true},
	{Key: "testing", Name: "قيد الاختبار", NameEn: "Testing", Tone: "testing", IsBase: true},
	{Key: "completed", Name: "مكتملة", NameEn: "Completed", Tone: "done", IsBase: true},
	{Key: "blocked", Name: "متوقفة", NameEn: "Blocked", Tone: "blocked", IsBase: true},
}

func (p *Project) IsMember(userID int64) bool {
	if p.PMID == userID {
		return true
	}
	for _, id := range p.MemberIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func (p *Project) FindStatus(key string) *ProjectStatus {
	for i := range p.Statuses {
		if p.Statuses[i].Key == key {
			return &p.Statuses[i]
		}
	}
	return nil
}

func (p *Project) DoneKeys() []string {
	var out []string
	for _, s := range p.Statuses {
		if s.Tone == "done" {
			out = append(out, s.Key)
		}
	}
	return out
}

func (p *Project) HasPhase(id int64) bool {
	for _, ph := range p.Phases {
		if ph.ID == id {
			return true
		}
	}
	return false
}

func (p *Project) ToJSON() map[string]any {
	statuses := make([]map[string]any, 0, len(p.Statuses))
	for _, s := range p.Statuses {
		statuses = append(statuses, map[string]any{
			"key":     s.Key,
			"name":    s.Name,
			"name_en": s.NameEn,
			"tone":    s.Tone,
		})
	}
	phases := make([]map[string]any, 0, len(p.Phases))
	for _, ph := range p.Phases {
		phases = append(phases, map[string]any{
			"id":   ph.ID,
			"name": ph.Name,
		})
	}
	memberIDs := p.MemberIDs
	if memberIDs == nil {
		memberIDs = []int64{}
	}
	return store.CleanMap(map[string]any{
		"id":           p.ID,
		"code":         p.Code,
		"name":         p.Name,
		"name_en":      p.NameEn,
		"description":  p.Description,
		"pm_id":        p.PMID,
		"member_ids":   memberIDs,
		"start":        p.Start,
		"end":          p.End,
		"state":        p.State,
		"priority":     p.Priority,
		"uses_sprints": p.UsesSprints,
		"statuses":     statuses,
		"phases":       phases,
	})
}

var (
	projCacheMu      sync.RWMutex
	projCacheList    []*Project
	projCacheByID    map[int64]*Project
	projCacheExpires time.Time
)

func InvalidateProjectsCache() {
	projCacheMu.Lock()
	projCacheList = nil
	projCacheByID = nil
	projCacheExpires = time.Time{}
	projCacheMu.Unlock()
}

func cloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	cp := *p
	if p.NameEn != nil {
		v := *p.NameEn
		cp.NameEn = &v
	}
	cp.MemberIDs = append([]int64(nil), p.MemberIDs...)
	cp.Statuses = append([]ProjectStatus(nil), p.Statuses...)
	cp.Phases = append([]ProjectPhase(nil), p.Phases...)
	return &cp
}

func LoadAllProjects(ctx context.Context, db *dbx.DB) ([]*Project, error) {
	if !db.IsSQLite() {
		projCacheMu.RLock()
		if projCacheList != nil && time.Now().Before(projCacheExpires) {
			out := make([]*Project, len(projCacheList))
			for i, p := range projCacheList {
				out[i] = cloneProject(p)
			}
			projCacheMu.RUnlock()
			return out, nil
		}
		projCacheMu.RUnlock()
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, code, name, name_en, description, pm_id, start, `+"`end`"+`, state, priority, uses_sprints
		FROM projects ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	var list []*Project
	byID := map[int64]*Project{}
	for rows.Next() {
		p, serr := scanProjectRow(rows)
		if serr != nil {
			_ = rows.Close()
			return nil, serr
		}
		list = append(list, p)
		byID[p.ID] = p
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return list, err
	}
	if len(list) > 0 {
		if err := hydrateProjectsMeta(ctx, db, byID); err != nil {
			return nil, err
		}
	}
	if !db.IsSQLite() {
		cachedList := make([]*Project, len(list))
		cachedMap := make(map[int64]*Project, len(list))
		for i, p := range list {
			cp := cloneProject(p)
			cachedList[i] = cp
			cachedMap[cp.ID] = cp
		}
		projCacheMu.Lock()
		projCacheList = cachedList
		projCacheByID = cachedMap
		projCacheExpires = time.Now().Add(15 * time.Second)
		projCacheMu.Unlock()
	}
	return list, nil
}

func LoadProject(ctx context.Context, db *dbx.DB, id int64) (*Project, error) {
	if !db.IsSQLite() {
		projCacheMu.RLock()
		if projCacheByID != nil && time.Now().Before(projCacheExpires) {
			p := cloneProject(projCacheByID[id])
			projCacheMu.RUnlock()
			return p, nil
		}
		projCacheMu.RUnlock()
		list, err := LoadAllProjects(ctx, db)
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			if p.ID == id {
				return p, nil
			}
		}
		return nil, nil
	}

	row := db.QueryRowContext(ctx, `
		SELECT id, code, name, name_en, description, pm_id, start, `+"`end`"+`, state, priority, uses_sprints
		FROM projects WHERE id = ?
	`, id)
	var p Project
	var nameEn sql.NullString
	var startRaw, endRaw any
	var usesInt int
	err := row.Scan(&p.ID, &p.Code, &p.Name, &nameEn, &p.Description, &p.PMID, &startRaw, &endRaw, &p.State, &p.Priority, &usesInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if nameEn.Valid && nameEn.String != "" {
		v := nameEn.String
		p.NameEn = &v
	}
	if s := dbx.ParseDateOnly(startRaw); s != nil {
		p.Start = *s
	}
	if e := dbx.ParseDateOnly(endRaw); e != nil {
		p.End = *e
	}
	p.UsesSprints = usesInt != 0
	byID := map[int64]*Project{p.ID: &p}
	if err := hydrateProjectsMeta(ctx, db, byID); err != nil {
		return nil, err
	}
	return &p, nil
}

func scanProjectRow(rows *sql.Rows) (*Project, error) {
	var p Project
	var nameEn sql.NullString
	var startRaw, endRaw any
	var usesInt int
	if err := rows.Scan(&p.ID, &p.Code, &p.Name, &nameEn, &p.Description, &p.PMID, &startRaw, &endRaw, &p.State, &p.Priority, &usesInt); err != nil {
		return nil, err
	}
	if nameEn.Valid && nameEn.String != "" {
		v := nameEn.String
		p.NameEn = &v
	}
	if s := dbx.ParseDateOnly(startRaw); s != nil {
		p.Start = *s
	}
	if e := dbx.ParseDateOnly(endRaw); e != nil {
		p.End = *e
	}
	p.UsesSprints = usesInt != 0
	return &p, nil
}

func hydrateProjectsMeta(ctx context.Context, db *dbx.DB, byID map[int64]*Project) error {
	if len(byID) == 0 {
		return nil
	}
	var singlePID int64
	var args []any
	where := ""
	if len(byID) == 1 {
		for pid := range byID {
			singlePID = pid
		}
		where = " WHERE project_id = ?"
		args = []any{singlePID}
	}

	type memberPair struct{ pid, uid int64 }
	type statusPair struct {
		pid int64
		st  ProjectStatus
	}
	type phasePair struct {
		pid int64
		ph  ProjectPhase
	}

	loadMembers := func() ([]memberPair, error) {
		rows, err := db.QueryContext(ctx, `SELECT project_id, user_id FROM project_members`+where+` ORDER BY id ASC`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []memberPair
		for rows.Next() {
			var pid, uid int64
			if rows.Scan(&pid, &uid) == nil {
				out = append(out, memberPair{pid, uid})
			}
		}
		return out, rows.Err()
	}

	loadStatuses := func() ([]statusPair, error) {
		rows, err := db.QueryContext(ctx, `SELECT id, project_id, `+"`key`"+`, name, name_en, tone, position, is_base FROM project_statuses`+where+` ORDER BY position ASC, id ASC`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []statusPair
		for rows.Next() {
			var st ProjectStatus
			var pid int64
			var isBaseInt int
			if rows.Scan(&st.ID, &pid, &st.Key, &st.Name, &st.NameEn, &st.Tone, &st.Position, &isBaseInt) == nil {
				st.IsBase = isBaseInt != 0
				out = append(out, statusPair{pid, st})
			}
		}
		return out, rows.Err()
	}

	loadPhases := func() ([]phasePair, error) {
		rows, err := db.QueryContext(ctx, `SELECT id, project_id, name, position FROM project_phases`+where+` ORDER BY position ASC, id ASC`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []phasePair
		for rows.Next() {
			var ph ProjectPhase
			var pid int64
			if rows.Scan(&ph.ID, &pid, &ph.Name, &ph.Position) == nil {
				out = append(out, phasePair{pid, ph})
			}
		}
		return out, rows.Err()
	}

	var (
		members  []memberPair
		statuses []statusPair
		phases   []phasePair
		errM, errS, errP error
	)
	if !db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			members, errM = loadMembers()
		}()
		go func() {
			defer wg.Done()
			statuses, errS = loadStatuses()
		}()
		go func() {
			defer wg.Done()
			phases, errP = loadPhases()
		}()
		wg.Wait()
	} else {
		members, errM = loadMembers()
		if errM == nil {
			statuses, errS = loadStatuses()
		}
		if errM == nil && errS == nil {
			phases, errP = loadPhases()
		}
	}
	if errM != nil {
		return errM
	}
	if errS != nil {
		return errS
	}
	if errP != nil {
		return errP
	}
	for _, m := range members {
		if p := byID[m.pid]; p != nil {
			p.MemberIDs = append(p.MemberIDs, m.uid)
		}
	}
	for _, s := range statuses {
		if p := byID[s.pid]; p != nil {
			p.Statuses = append(p.Statuses, s.st)
		}
	}
	for _, ph := range phases {
		if p := byID[ph.pid]; p != nil {
			p.Phases = append(p.Phases, ph.ph)
		}
	}
	return nil
}

func CanSeeProject(u *store.User, p *Project) bool {
	if u.IsCEO() || u.Role == "monitor" || u.Role == "admin" {
		return true
	}
	return p.IsMember(u.ID)
}

func ProjectForOrFail(ctx context.Context, db *dbx.DB, u *store.User, projectID int64) (*Project, error) {
	p, err := LoadProject(ctx, db, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil || !CanSeeProject(u, p) {
		return nil, httpx.ErrNotFound("api.projects.not_found")
	}
	return p, nil
}

func CanCreateProject(u *store.User) bool {
	return u.IsCEO() || u.Role == "pm" || u.Role == "monitor"
}

func CanManageProjectSetup(u *store.User, p *Project) bool {
	return u.IsCEO() || p.PMID == u.ID
}

func CanTakePart(u *store.User, p *Project) bool {
	if u.Role == "admin" {
		return false
	}
	return u.IsCEO() || u.Role == "monitor" || p.IsMember(u.ID)
}

func CanManageTasks(u *store.User, p *Project) bool {
	return u.IsCEO() || p.PMID == u.ID || (u.Role == "lead" && p.IsMember(u.ID))
}

func CanApprove(u *store.User, p *Project) bool {
	return u.IsCEO() || CanManageTasks(u, p)
}

func CanMoveTask(u *store.User, p *Project, assigneeID *int64) bool {
	if CanManageTasks(u, p) {
		return true
	}
	return assigneeID != nil && *assigneeID == u.ID && p.IsMember(u.ID)
}

func CanManageBugs(u *store.User, p *Project) bool {
	return CanManageTasks(u, p) || u.Role == "monitor"
}

func BugNextStatuses(u *store.User, p *Project, status string, reportedBy int64, assigneeID *int64) []string {
	type edge struct {
		to  string
		who string
	}
	graph := map[string][]edge{
		"reported": {{to: "assigned", who: "managers"}},
		"assigned": {{to: "fixing", who: "developer"}},
		"fixing":   {{to: "testing", who: "developer"}},
		"testing":  {{to: "closed", who: "checker"}, {to: "reopened", who: "checker"}},
		"reopened": {{to: "fixing", who: "developer"}},
		"closed":   {{to: "reopened", who: "checker"}},
	}
	isMgr := CanManageBugs(u, p)
	var out []string
	for _, e := range graph[status] {
		if isMgr ||
			(e.who == "developer" && assigneeID != nil && *assigneeID == u.ID) ||
			(e.who == "checker" && reportedBy == u.ID) {
			out = append(out, e.to)
		}
	}
	return out
}

func CanSeeReports(u *store.User) bool {
	return u.IsCEO() || u.Role == "pm" || u.Role == "monitor" || u.Role == "lead"
}

func CanPostAnnouncements(u *store.User) bool {
	return u.IsCEO() || u.Role == "admin" || u.Role == "monitor"
}

func CanManageAnnouncement(u *store.User, authorID int64) bool {
	return u.IsCEO() || u.ID == authorID || u.Role == "admin"
}

func CanSeePeoplePerf(u *store.User) bool {
	return u.IsCEO() || u.Role == "pm" || u.Role == "monitor" || u.Role == "lead"
}

func CanReview(u *store.User, subject *store.User, projects []*Project) bool {
	if u.ID == subject.ID || subject.Role == "ceo" || subject.Role == "admin" {
		return false
	}
	if u.IsCEO() {
		return true
	}
	if u.Role == "pm" {
		for _, p := range projects {
			if p.PMID == u.ID && p.IsMember(subject.ID) {
				return true
			}
		}
		return false
	}
	if u.Role == "lead" {
		return u.TeamID != nil && subject.TeamID != nil && *u.TeamID == *subject.TeamID && subject.Role == "employee"
	}
	return false
}

func CanSeePerson(u *store.User, subject *store.User, projects []*Project) bool {
	return u.IsCEO() || u.ID == subject.ID || u.Role == "monitor" || CanReview(u, subject, projects)
}

func CanSeeReview(u *store.User, reviewerID int64, state string, subject *store.User, projects []*Project) bool {
	if u.IsCEO() || reviewerID == u.ID {
		return true
	}
	if state != "final" {
		return false
	}
	return CanSeePerson(u, subject, projects)
}

func CanAdminister(u *store.User) bool {
	return u.IsCEO() || u.Role == "admin"
}

func CanListAdminUsers(u *store.User) bool {
	return CanAdminister(u) || u.Role == "monitor"
}

func CanStartDirect(u *store.User, other *store.User) bool {
	return u.IsCEO() || !other.IsCEO() || u.Role == "monitor"
}

func CanCreateGroupChat(u *store.User) bool {
	return u.IsCEO() || u.Role == "monitor" || u.Role == "pm" || u.Role == "lead"
}

func CanSeeTeam(u *store.User, teamID int64) bool {
	return u.IsCEO() || u.Role == "monitor" || u.Role == "pm" || u.Role == "admin" || (u.TeamID != nil && *u.TeamID == teamID)
}

func WorksForPM(pmID int64, teamID int64, activeUsers []*store.User, projects []*Project) bool {
	teamUsers := map[int64]bool{}
	for _, au := range activeUsers {
		if au.TeamID != nil && *au.TeamID == teamID {
			teamUsers[au.ID] = true
		}
	}
	if len(teamUsers) == 0 {
		return false
	}
	for _, p := range projects {
		if p.PMID != pmID {
			continue
		}
		if teamUsers[p.PMID] {
			return true
		}
		for _, mid := range p.MemberIDs {
			if teamUsers[mid] {
				return true
			}
		}
	}
	return false
}

func CanRateTeam(u *store.User, teamID int64, activeUsers []*store.User, projects []*Project) bool {
	if u.IsCEO() || u.Role == "monitor" {
		return true
	}
	return u.Role == "pm" && WorksForPM(u.ID, teamID, activeUsers, projects)
}

func CanSeeTeamRatings(u *store.User, teamID int64, activeUsers []*store.User, projects []*Project) bool {
	return u.IsCEO() || CanRateTeam(u, teamID, activeUsers, projects) ||
		(u.Role != "admin" && u.TeamID != nil && *u.TeamID == teamID)
}

func CanRatePerson(u *store.User, person *store.User, projects []*Project) bool {
	if u.ID == person.ID || (person.Role != "pm" && person.Role != "monitor" && person.Role != "lead" && person.Role != "employee") {
		return false
	}
	if u.IsCEO() || u.Role == "monitor" {
		return true
	}
	if u.Role == "pm" {
		for _, p := range projects {
			if p.PMID == u.ID && p.IsMember(person.ID) {
				return true
			}
		}
	}
	return false
}

func CanSeePersonRatings(u *store.User, person *store.User, projects []*Project) bool {
	return u.IsCEO() || CanRatePerson(u, person, projects) || u.ID == person.ID
}

// Shared JSON unmarshalling helper for small arrays
func DecodeJSONArray(raw string) []any {
	out := []any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}
