package projects

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
	"followapi/internal/audit"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

var ProjectStates = []string{"planned", "active", "paused", "done"}
var ProjectPriorities = []string{"high", "medium", "low"}

type Service struct {
	db            *dbx.DB
	audit         *audit.Service
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, auditSvc *audit.Service, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, audit: auditSvc, notifications: notifSvc, advanced: advSvc}
}

func (s *Service) HandleIndex(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	all, err := access.LoadAllProjects(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var visible []*access.Project
	var visIDs []int64
	for _, p := range all {
		if access.CanSeeProject(u, p) {
			visible = append(visible, p)
			visIDs = append(visIDs, p.ID)
		}
	}
	type taskStat struct {
		status string
		due    *string
	}
	statsByProj := map[int64][]taskStat{}
	if len(visIDs) > 0 {
		ph, args := dbx.InInt64(visIDs)
		rows, err := s.db.QueryContext(r.Context(), fmt.Sprintf(`SELECT project_id, status, due FROM tasks WHERE project_id IN (%s)`, ph), args...)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		for rows.Next() {
			var pid int64
			var st string
			var dueRaw any
			if rows.Scan(&pid, &st, &dueRaw) == nil {
				statsByProj[pid] = append(statsByProj[pid], taskStat{status: st, due: dbx.ParseDateOnly(dueRaw)})
			}
		}
		_ = rows.Close()
	}

	today := support.BaghdadToday()
	out := make([]map[string]any, 0, len(visible))
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
		tasks := statsByProj[p.ID]
		var total, done, blocked, late int
		total = len(tasks)
		for _, t := range tasks {
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
		m := p.ToJSON()
		m["tasks_total"] = total
		m["tasks_done"] = done
		m["tasks_late"] = late
		m["tasks_blocked"] = blocked
		out = append(out, m)
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
	p, err := access.ProjectForOrFail(r.Context(), s.db, u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, p.ToJSON())
}

func (s *Service) HandleCreate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !access.CanCreateProject(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.projects.create_forbidden"))
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "create_project"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	pmID := toInt64Val(input["pm_id"])
	if u.Role == "pm" {
		pmID = u.ID
	}
	input["pm_id"] = pmID
	if strVal(input["start"]) == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("start", "api.projects.start_required"))
		return
	}
	if strVal(input["end"]) == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("end", "api.projects.end_required"))
		return
	}
	if err := s.checkFields(r.Context(), input, nil); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var tplName *string
	var tplStatuses []access.ProjectStatus
	var tplPhases []string
	var tplTasks []map[string]any
	if tid := toInt64Val(input["template_id"]); tid > 0 {
		var name, statusesRaw, phasesRaw, tasksRaw string
		qerr := s.db.QueryRowContext(r.Context(), `SELECT name, statuses, phases, tasks FROM project_templates WHERE id = ?`, tid).
			Scan(&name, &statusesRaw, &phasesRaw, &tasksRaw)
		if qerr != nil {
			httpx.HandleErr(w, r, httpx.ErrValidation("template_id", "api.projects.template_not_found"))
			return
		}
		tplName = &name
		var rawSt []map[string]any
		_ = json.Unmarshal([]byte(statusesRaw), &rawSt)
		for _, rs := range rawSt {
			tplStatuses = append(tplStatuses, access.ProjectStatus{
				Key:    strVal(rs["key"]),
				Name:   strVal(rs["name"]),
				NameEn: strVal(rs["name_en"]),
				Tone:   strVal(rs["tone"]),
			})
		}
		_ = json.Unmarshal([]byte(phasesRaw), &tplPhases)
		_ = json.Unmarshal([]byte(tasksRaw), &tplTasks)
	}

	var projectID int64
	var projName string
	var projNameEn *string
	memberIDs := withPM(toInt64Slice(input["member_ids"]), pmID)
	today := support.BaghdadToday()
	start := strVal(input["start"])
	end := strVal(input["end"])
	state := "active"
	if start > today {
		state = "planned"
	}
	priority := strVal(input["priority"])
	if priority == "" {
		priority = "medium"
	}
	usesSprints := boolVal(input["uses_sprints"])
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	projPrefix := s.advanced.CodePrefix(r.Context(), "project")
	taskPrefix := s.advanced.CodePrefix(r.Context(), "task")

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		code, err := dbx.NextCode(r.Context(), s.db, tx, "project", projPrefix)
		if err != nil {
			return err
		}
		projName = strings.TrimSpace(strVal(input["name"]))
		if ne := strings.TrimSpace(strVal(input["name_en"])); ne != "" {
			projNameEn = &ne
		}
		desc := strings.TrimSpace(strVal(input["description"]))
		usInt := 0
		if usesSprints {
			usInt = 1
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO projects (code, name, name_en, description, pm_id, start, `+"`end`"+`, state, priority, uses_sprints, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, code, projName, projNameEn, desc, pmID, start, end, state, priority, usInt, nowStr, nowStr)
		if err != nil {
			return err
		}
		projectID, _ = res.LastInsertId()

		for _, mid := range memberIDs {
			_, _ = tx.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO project_members (project_id, user_id, created_at) VALUES (?, ?, ?)`, projectID, mid, nowStr)
		}

		statuses := access.DefaultStatuses
		if len(tplStatuses) > 0 {
			statuses = tplStatuses
		}
		for pos, st := range statuses {
			isBase := 0
			if store.UserHasRole(st.Key, access.BaseStatusKeys) {
				isBase = 1
			}
			_, err := tx.ExecContext(r.Context(), `
				INSERT INTO project_statuses (project_id, `+"`key`"+`, name, name_en, tone, position, is_base, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, projectID, st.Key, st.Name, st.NameEn, st.Tone, pos, isBase, nowStr, nowStr)
			if err != nil {
				return err
			}
		}

		if tplName != nil {
			phaseMap := map[string]int64{}
			for pos, phName := range tplPhases {
				pRes, err := tx.ExecContext(r.Context(), `
					INSERT INTO project_phases (project_id, name, position, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
				`, projectID, phName, pos, nowStr, nowStr)
				if err != nil {
					return err
				}
				pid, _ := pRes.LastInsertId()
				phaseMap[phName] = pid
			}
			firstKey := statuses[0].Key
			for _, tt := range tplTasks {
				tCode, err := dbx.NextCode(r.Context(), s.db, tx, "task", taskPrefix)
				if err != nil {
					return err
				}
				var phID *int64
				if pn := strVal(tt["phase"]); pn != "" {
					if idVal, ok := phaseMap[pn]; ok {
						phID = &idVal
					}
				}
				tPri := strVal(tt["priority"])
				if tPri == "" {
					tPri = "medium"
				}
				tRes, err := tx.ExecContext(r.Context(), `
					INSERT INTO tasks (code, project_id, phase_id, title, description, priority, status, subtasks_done, subtasks_total, created_by, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)
				`, tCode, projectID, phID, strVal(tt["title"]), strVal(tt["description"]), tPri, firstKey, u.ID, nowStr, nowStr)
				if err != nil {
					return err
				}
				tid, _ := tRes.LastInsertId()
				_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, at) VALUES (?, ?, 'created', ?)`, tid, u.ID, nowStr)
			}
		}

		_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, note, at) VALUES (?, ?, 'created', ?, ?)`,
			projectID, u.ID, tplName, nowStr)
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var auditChanges []audit.Change
	if tplName != nil {
		auditChanges = []audit.Change{{Field: "template", To: tplName}}
	}
	s.audit.Log(r.Context(), &u.ID, "project_created", "project", &projectID, projName, auditChanges)
	s.notifications.Notify(r.Context(), memberIDs, "project_added", notifications.About{
		ActorID:   &u.ID,
		ProjectID: &projectID,
		Data:      notifications.AboutProject(projName, projNameEn),
	})

	access.InvalidateProjectsCache()
	p, _ := access.LoadProject(r.Context(), s.db, projectID)
	httpx.Success(w, p.ToJSON(), http.StatusCreated)
}

func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	patch, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if _, hasPM := patch["pm_id"]; hasPM {
		if toInt64Val(patch["pm_id"]) != p.PMID && !u.IsCEO() {
			httpx.HandleErr(w, r, httpx.ErrForbidden("api.projects.pm_change_forbidden"))
			return
		}
	}
	if _, hasState := patch["state"]; hasState {
		if !store.UserHasRole(strVal(patch["state"]), ProjectStates) {
			httpx.HandleErr(w, r, httpx.ErrValidation("state", "api.projects.state_invalid"))
			return
		}
	}
	if err := s.checkFields(r.Context(), patch, p); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var note *string
	if n := strings.TrimSpace(strVal(patch["note"])); n != "" {
		if support.RuneLen(n) > 300 {
			httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.projects.note_max"))
			return
		}
		note = &n
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		curMembers := append([]int64{}, p.MemberIDs...)
		if !containsInt64(curMembers, p.PMID) {
			curMembers = append([]int64{p.PMID}, curMembers...)
		}
		pmID := p.PMID
		if _, hasPM := patch["pm_id"]; hasPM {
			pmID = toInt64Val(patch["pm_id"])
		}

		if _, hasMembers := patch["member_ids"]; hasMembers {
			nextMembers := withPM(withPM(toInt64Slice(patch["member_ids"]), pmID), p.PMID)
			for _, oldID := range curMembers {
				if containsInt64(nextMembers, oldID) {
					continue
				}
				doneKeys := p.DoneKeys()
				var openCount int
				if len(doneKeys) > 0 {
					ph := dbx.Placeholders(len(doneKeys))
					args := []any{p.ID, oldID}
					for _, dk := range doneKeys {
						args = append(args, dk)
					}
					_ = tx.QueryRowContext(r.Context(), fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND assignee_id = ? AND status NOT IN (%s)`, ph), args...).Scan(&openCount)
				} else {
					_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tasks WHERE project_id = ? AND assignee_id = ?`, p.ID, oldID).Scan(&openCount)
				}
				if openCount > 0 {
					var nm string
					_ = tx.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, oldID).Scan(&nm)
					return httpx.ErrConflict("api.projects.member_has_open_tasks|" + nm)
				}
			}

			for _, nid := range nextMembers {
				if !containsInt64(curMembers, nid) {
					toStr := strconv.FormatInt(nid, 10)
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`to`"+`, at) VALUES (?, ?, 'member_added', ?, ?)`, p.ID, u.ID, toStr, nowStr)
					s.notifications.Notify(r.Context(), []int64{nid}, "project_added", notifications.About{
						ActorID: &u.ID, ProjectID: &p.ID, Data: notifications.AboutProject(p.Name, p.NameEn),
					})
				}
			}
			for _, oid := range curMembers {
				if !containsInt64(nextMembers, oid) {
					fromStr := strconv.FormatInt(oid, 10)
					_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, at) VALUES (?, ?, 'member_removed', ?, ?)`, p.ID, u.ID, fromStr, nowStr)
				}
			}

			_, _ = tx.ExecContext(r.Context(), `DELETE FROM project_members WHERE project_id = ?`, p.ID)
			for _, mid := range nextMembers {
				_, _ = tx.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO project_members (project_id, user_id, created_at) VALUES (?, ?, ?)`, p.ID, mid, nowStr)
			}
			curMembers = nextMembers
		}

		if _, hasPM := patch["pm_id"]; hasPM && pmID != p.PMID {
			oldPMID := p.PMID
			oldStr := strconv.FormatInt(oldPMID, 10)
			newStr := strconv.FormatInt(pmID, 10)
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'pm', ?, ?, ?, ?)`,
				p.ID, u.ID, oldStr, newStr, note, nowStr)

			var oldName, newName string
			_ = tx.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, oldPMID).Scan(&oldName)
			_ = tx.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, pmID).Scan(&newName)
			s.audit.Log(r.Context(), &u.ID, "project_pm_changed", "project", &p.ID, p.Name, []audit.Change{{Field: "pm", From: &oldName, To: &newName}})

			allRecips := withPM(curMembers, pmID)
			d := notifications.AboutProject(p.Name, p.NameEn)
			d["pm_id"] = newStr
			if note != nil {
				d["note"] = *note
			}
			s.notifications.Notify(r.Context(), allRecips, "project_pm", notifications.About{ActorID: &u.ID, ProjectID: &p.ID, Data: d})
			_, _ = tx.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO project_members (project_id, user_id, created_at) VALUES (?, ?, ?)`, p.ID, pmID, nowStr)
			p.PMID = pmID
		}

		newStart := p.Start
		newEnd := p.End
		if s := strVal(patch["start"]); s != "" {
			newStart = s
		}
		if e := strVal(patch["end"]); e != "" {
			newEnd = e
		}
		if newStart != p.Start || newEnd != p.End {
			fromRange := p.Start + " → " + p.End
			toRange := newStart + " → " + newEnd
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'dates', ?, ?, ?, ?)`,
				p.ID, u.ID, fromRange, toRange, note, nowStr)
			d := notifications.AboutProject(p.Name, p.NameEn)
			d["due"] = newEnd
			if note != nil {
				d["note"] = *note
			}
			s.notifications.Notify(r.Context(), curMembers, "project_plan", notifications.About{ActorID: &u.ID, ProjectID: &p.ID, Data: d})
			p.Start = newStart
			p.End = newEnd
		}

		if st := strVal(patch["state"]); st != "" && st != p.State {
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'state', ?, ?, ?, ?)`,
				p.ID, u.ID, p.State, st, note, nowStr)
			p.State = st
		}

		var details []string
		if _, ok := patch["name"]; ok {
			nm := strings.TrimSpace(strVal(patch["name"]))
			if nm != p.Name {
				p.Name = nm
				details = append(details, "name")
			}
		}
		if _, ok := patch["name_en"]; ok {
			ne := strings.TrimSpace(strVal(patch["name_en"]))
			var ptr *string
			if ne != "" {
				ptr = &ne
			}
			if (ptr == nil) != (p.NameEn == nil) || (ptr != nil && p.NameEn != nil && *ptr != *p.NameEn) {
				p.NameEn = ptr
				details = append(details, "name_en")
			}
		}
		if _, ok := patch["description"]; ok {
			desc := strings.TrimSpace(strVal(patch["description"]))
			if desc != p.Description {
				p.Description = desc
				details = append(details, "description")
			}
		}
		if _, ok := patch["priority"]; ok {
			pr := strVal(patch["priority"])
			if pr != "" && pr != p.Priority {
				p.Priority = pr
				details = append(details, "priority")
			}
		}
		if v, ok := patch["uses_sprints"]; ok {
			b := boolVal(v)
			if b != p.UsesSprints {
				p.UsesSprints = b
				details = append(details, "uses_sprints")
			}
		}
		if len(details) > 0 {
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`to`"+`, note, at) VALUES (?, ?, 'details', ?, ?, ?)`,
				p.ID, u.ID, strings.Join(details, ","), note, nowStr)
		}

		usInt := 0
		if p.UsesSprints {
			usInt = 1
		}
		_, err := tx.ExecContext(r.Context(), `
			UPDATE projects SET name = ?, name_en = ?, description = ?, pm_id = ?, start = ?, `+"`end`"+` = ?, state = ?, priority = ?, uses_sprints = ?, updated_at = ?
			WHERE id = ?
		`, p.Name, p.NameEn, p.Description, p.PMID, p.Start, p.End, p.State, p.Priority, usInt, nowStr, p.ID)
		return err
	})
	if err != nil {
		if strings.HasPrefix(err.Error(), "api.projects.member_has_open_tasks|") {
			nm := strings.TrimPrefix(err.Error(), "api.projects.member_has_open_tasks|")
			httpx.HandleErr(w, r, &httpx.APIError{Status: 409, Code: "CONFLICT", Key: "api.projects.member_has_open_tasks", Params: map[string]any{"name": nm}})
			return
		}
		httpx.HandleErr(w, r, err)
		return
	}

	access.InvalidateProjectsCache()
	updated, _ := access.LoadProject(r.Context(), s.db, id)
	httpx.Success(w, updated.ToJSON())
}

func (s *Service) HandleSaveStatuses(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rawStatuses, _ := body["statuses"].([]any)
	if len(rawStatuses) == 0 || len(rawStatuses) > 15 {
		httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_max", map[string]any{"max": 15}))
		return
	}

	currentMap := map[string]access.ProjectStatus{}
	var usedKeys []string
	for _, st := range p.Statuses {
		currentMap[st.Key] = st
		usedKeys = append(usedKeys, st.Key)
	}
	for _, base := range p.Statuses {
		if !base.IsBase {
			continue
		}
		found := false
		for _, rs := range rawStatuses {
			rm, _ := rs.(map[string]any)
			if strVal(rm["key"]) == base.Key && strVal(rm["tone"]) == base.Tone {
				found = true
				break
			}
		}
		if !found {
			httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_base_locked"))
			return
		}
	}

	var next []access.ProjectStatus
	n := 0
	for _, rs := range rawStatuses {
		rm, _ := rs.(map[string]any)
		key := strVal(rm["key"])
		if key != "" && store.UserHasRole(key, access.BaseStatusKeys) {
			if base, ok := currentMap[key]; ok {
				next = append(next, access.ProjectStatus{Key: base.Key, Name: base.Name, NameEn: base.NameEn, Tone: base.Tone, IsBase: true})
				continue
			}
		}
		name := strings.TrimSpace(strVal(rm["name"]))
		if name == "" {
			httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_name_required"))
			return
		}
		if support.RuneLen(name) > 40 {
			httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_name_max"))
			return
		}
		tone := strVal(rm["tone"])
		if !store.UserHasRole(tone, access.StatusTones) {
			httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_tone_invalid"))
			return
		}
		if key != "" {
			if _, ok := currentMap[key]; !ok {
				httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_unknown"))
				return
			}
		} else {
			for {
				n++
				key = fmt.Sprintf("custom_%s_%d", strconv.FormatInt(time.Now().UnixMilli(), 36), n)
				if !store.UserHasRole(key, usedKeys) {
					usedKeys = append(usedKeys, key)
					break
				}
			}
		}
		nameEn := strings.TrimSpace(strVal(rm["name_en"]))
		if nameEn == "" {
			nameEn = name
		}
		next = append(next, access.ProjectStatus{Key: key, Name: name, NameEn: nameEn, Tone: tone, IsBase: false})
	}

	seenNames := map[string]bool{}
	var afterNames []string
	var nextKeys []string
	for _, st := range next {
		if seenNames[st.Name] {
			httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_duplicate"))
			return
		}
		seenNames[st.Name] = true
		afterNames = append(afterNames, st.Name)
		nextKeys = append(nextKeys, st.Key)
	}
	if next[0].Tone == "blocked" || next[0].Tone == "done" {
		httpx.HandleErr(w, r, httpx.ErrValidation("statuses", "api.projects.statuses_first_invalid"))
		return
	}

	isEn := httpx.LocaleFrom(r.Context()) == "en"
	for _, old := range p.Statuses {
		if store.UserHasRole(old.Key, nextKeys) {
			continue
		}
		var cnt int
		_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tasks WHERE project_id = ? AND status = ?`, p.ID, old.Key).Scan(&cnt)
		if cnt > 0 {
			lbl := old.Name
			if isEn {
				lbl = old.NameEn
			}
			httpx.HandleErr(w, r, &httpx.APIError{Status: 409, Code: "CONFLICT", Key: "api.projects.statuses_in_use", Params: map[string]any{"name": lbl, "count": cnt}})
			return
		}
	}

	var beforeNames []string
	for _, st := range p.Statuses {
		beforeNames = append(beforeNames, st.Name)
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM project_statuses WHERE project_id = ?`, p.ID); err != nil {
			return err
		}
		for pos, st := range next {
			ib := 0
			if st.IsBase {
				ib = 1
			}
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO project_statuses (project_id, `+"`key`"+`, name, name_en, tone, position, is_base, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, p.ID, st.Key, st.Name, st.NameEn, st.Tone, pos, ib, nowStr, nowStr); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'statuses', ?, ?, ?)`,
			p.ID, u.ID, strings.Join(beforeNames, "، "), strings.Join(afterNames, "، "), nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	access.InvalidateProjectsCache()
	updated, _ := access.LoadProject(r.Context(), s.db, p.ID)
	httpx.Success(w, updated.ToJSON())
}

func (s *Service) HandleSavePhases(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rawPhases, _ := body["phases"].([]any)
	if len(rawPhases) > 20 {
		httpx.HandleErr(w, r, httpx.ErrValidation("phases", "api.projects.phases_max", map[string]any{"max": 20}))
		return
	}
	existing := map[int64]access.ProjectPhase{}
	for _, ph := range p.Phases {
		existing[ph.ID] = ph
	}
	type cleanPhase struct {
		id   *int64
		name string
	}
	var clean []cleanPhase
	seen := map[string]bool{}
	var keptIDs []int64
	var afterNames []string
	for _, rp := range rawPhases {
		rm, _ := rp.(map[string]any)
		nm := strings.TrimSpace(strVal(rm["name"]))
		if nm == "" {
			httpx.HandleErr(w, r, httpx.ErrValidation("phases", "api.projects.phases_name_required"))
			return
		}
		if support.RuneLen(nm) > 80 {
			httpx.HandleErr(w, r, httpx.ErrValidation("phases", "api.projects.phases_name_max"))
			return
		}
		if seen[nm] {
			httpx.HandleErr(w, r, httpx.ErrValidation("phases", "api.projects.phases_duplicate"))
			return
		}
		seen[nm] = true
		var phID *int64
		if pid := toInt64Val(rm["id"]); pid > 0 {
			if _, ok := existing[pid]; !ok {
				httpx.HandleErr(w, r, httpx.ErrValidation("phases", "api.projects.phases_unknown"))
				return
			}
			phID = &pid
			keptIDs = append(keptIDs, pid)
		}
		clean = append(clean, cleanPhase{id: phID, name: nm})
		afterNames = append(afterNames, nm)
	}

	var toDelete []int64
	var beforeNames []string
	for _, old := range p.Phases {
		beforeNames = append(beforeNames, old.Name)
		if !containsInt64(keptIDs, old.ID) {
			var cnt int
			_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tasks WHERE project_id = ? AND phase_id = ?`, p.ID, old.ID).Scan(&cnt)
			if cnt > 0 {
				httpx.HandleErr(w, r, &httpx.APIError{Status: 409, Code: "CONFLICT", Key: "api.projects.phases_in_use", Params: map[string]any{"name": old.Name, "count": cnt}})
				return
			}
			toDelete = append(toDelete, old.ID)
		}
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if len(toDelete) > 0 {
			ph, args := dbx.InInt64(toDelete)
			if _, err := tx.ExecContext(r.Context(), fmt.Sprintf(`DELETE FROM project_phases WHERE id IN (%s)`, ph), args...); err != nil {
				return err
			}
		}
		for pos, item := range clean {
			if item.id != nil {
				_, err := tx.ExecContext(r.Context(), `UPDATE project_phases SET name = ?, position = ?, updated_at = ? WHERE id = ?`, item.name, pos, nowStr, *item.id)
				if err != nil {
					return err
				}
			} else {
				_, err := tx.ExecContext(r.Context(), `INSERT INTO project_phases (project_id, name, position, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, p.ID, item.name, pos, nowStr, nowStr)
				if err != nil {
					return err
				}
			}
		}
		var fromPtr, toPtr *string
		if len(beforeNames) > 0 {
			s := strings.Join(beforeNames, "، ")
			fromPtr = &s
		}
		if len(afterNames) > 0 {
			s := strings.Join(afterNames, "، ")
			toPtr = &s
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'phases', ?, ?, ?)`,
			p.ID, u.ID, fromPtr, toPtr, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	access.InvalidateProjectsCache()
	updated, _ := access.LoadProject(r.Context(), s.db, p.ID)
	httpx.Success(w, updated.ToJSON())
}

func (s *Service) HandleHistory(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(r.Context(), s.db, u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at FROM project_events WHERE project_id = ? ORDER BY at ASC, id ASC`, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var eid, pid, aid int64
		var kind string
		var from, to, note sql.NullString
		var atRaw any
		if rows.Scan(&eid, &pid, &aid, &kind, &from, &to, &note, &atRaw) == nil {
			m := map[string]any{"id": eid, "project_id": pid, "actor_id": aid, "kind": kind, "at": store.IsoTime(dbx.ParseTime(atRaw))}
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

// Sprints

func (s *Service) HandleSprints(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(r.Context(), s.db, u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, project_id, name, start, `+"`end`"+`, closed_at, result FROM sprints WHERE project_id = ? ORDER BY start ASC`, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		sp, serr := scanSprintRow(rows)
		if serr == nil {
			out = append(out, sp)
		}
	}
	httpx.Success(w, out)
}

func (s *Service) HandleCreateSprint(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !p.UsesSprints {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.projects.no_sprints"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name, start, end, err := s.checkSprint(r.Context(), input, p.ID, nil)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var sprintID int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `INSERT INTO sprints (project_id, name, start, `+"`end`"+`, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, name, start, end, nowStr, nowStr)
		if err != nil {
			return err
		}
		sprintID, _ = res.LastInsertId()
		_, err = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`to`"+`, at) VALUES (?, ?, 'sprint_added', ?, ?)`,
			p.ID, u.ID, name+": "+start+" → "+end, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	sp, _ := s.loadSprint(r.Context(), sprintID)
	httpx.Success(w, sp, http.StatusCreated)
}

func (s *Service) HandleUpdateSprint(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var projID int64
	var oldName string
	var startRaw, endRaw, closedRaw any
	err = s.db.QueryRowContext(r.Context(), `SELECT project_id, name, start, `+"`end`"+`, closed_at FROM sprints WHERE id = ?`, id).
		Scan(&projID, &oldName, &startRaw, &endRaw, &closedRaw)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.projects.sprint_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := s.manageable(r.Context(), u, projID); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if dbx.ParseTime(closedRaw) != nil {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.projects.sprint_ended"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name, start, end, err := s.checkSprint(r.Context(), input, projID, &id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	oldStart := derefStr(dbx.ParseDateOnly(startRaw))
	oldEnd := derefStr(dbx.ParseDateOnly(endRaw))
	before := oldName + ": " + oldStart + " → " + oldEnd
	after := name + ": " + start + " → " + end
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE sprints SET name = ?, start = ?, `+"`end`"+` = ?, updated_at = ? WHERE id = ?`, name, start, end, nowStr, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'sprint_changed', ?, ?, ?)`,
			projID, u.ID, before, after, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	sp, _ := s.loadSprint(r.Context(), id)
	httpx.Success(w, sp)
}

func (s *Service) HandleDeleteSprint(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var projID int64
	var name string
	var startRaw, endRaw, closedRaw any
	err = s.db.QueryRowContext(r.Context(), `SELECT project_id, name, start, `+"`end`"+`, closed_at FROM sprints WHERE id = ?`, id).
		Scan(&projID, &name, &startRaw, &endRaw, &closedRaw)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.projects.sprint_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := s.manageable(r.Context(), u, projID); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if dbx.ParseTime(closedRaw) != nil {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.projects.sprint_ended"))
		return
	}
	var used int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM tasks WHERE sprint_id = ?`, id).Scan(&used)
	if used > 0 {
		httpx.HandleErr(w, r, &httpx.APIError{Status: 409, Code: "CONFLICT", Key: "api.projects.sprint_has_tasks", Params: map[string]any{"count": used}})
		return
	}
	lbl := name + ": " + derefStr(dbx.ParseDateOnly(startRaw)) + " → " + derefStr(dbx.ParseDateOnly(endRaw))
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM sprints WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, at) VALUES (?, ?, 'sprint_removed', ?, ?)`,
			projID, u.ID, lbl, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, nil)
}

func (s *Service) HandleCloseSprint(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body, _ := httpx.DecodeMap(r)
	var projID int64
	var name string
	var startRaw, closedRaw any
	err = s.db.QueryRowContext(r.Context(), `SELECT project_id, name, start, closed_at FROM sprints WHERE id = ?`, id).
		Scan(&projID, &name, &startRaw, &closedRaw)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.projects.sprint_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, projID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if dbx.ParseTime(closedRaw) != nil {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.projects.sprint_ended"))
		return
	}
	start := derefStr(dbx.ParseDateOnly(startRaw))
	if start > support.BaghdadToday() {
		httpx.HandleErr(w, r, httpx.ErrConflict("api.projects.sprint_not_started"))
		return
	}
	var moveTo *int64
	var targetName *string
	if mt := toInt64Val(body["move_to"]); mt > 0 {
		var tProjID int64
		var tName string
		var tClosed any
		qerr := s.db.QueryRowContext(r.Context(), `SELECT project_id, name, closed_at FROM sprints WHERE id = ?`, mt).Scan(&tProjID, &tName, &tClosed)
		if qerr != nil || tProjID != projID || mt == id || dbx.ParseTime(tClosed) != nil {
			httpx.HandleErr(w, r, httpx.ErrValidation("move_to", "api.projects.sprint_move_to_invalid"))
			return
		}
		moveTo = &mt
		targetName = &tName
	}

	doneSet := map[string]bool{}
	for _, k := range p.DoneKeys() {
		doneSet[k] = true
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var plannedCount, movedCount, doneCount int
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.Context(), `SELECT id, status FROM tasks WHERE sprint_id = ?`+s.db.ForUpdate(), id)
		if err != nil {
			return err
		}
		var leftIDs []int64
		for rows.Next() {
			var tid int64
			var tst string
			if rows.Scan(&tid, &tst) == nil {
				plannedCount++
				if !doneSet[tst] {
					leftIDs = append(leftIDs, tid)
				}
			}
		}
		_ = rows.Close()
		movedCount = len(leftIDs)
		doneCount = plannedCount - movedCount

		for _, tid := range leftIDs {
			_, _ = tx.ExecContext(r.Context(), `UPDATE tasks SET sprint_id = ?, updated_at = ? WHERE id = ?`, moveTo, nowStr, tid)
			_, _ = tx.ExecContext(r.Context(), `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, 'sprint', ?, ?, ?)`,
				tid, u.ID, name, targetName, nowStr)
		}

		resMap := map[string]any{"planned": plannedCount, "done": doneCount, "moved": movedCount}
		if moveTo != nil {
			resMap["moved_to"] = *moveTo
		}
		resJSON, _ := json.Marshal(resMap)
		if _, err := tx.ExecContext(r.Context(), `UPDATE sprints SET closed_at = ?, result = ?, updated_at = ? WHERE id = ?`,
			nowStr, string(resJSON), nowStr, id); err != nil {
			return err
		}
		note := fmt.Sprintf("%d/%d", doneCount, plannedCount)
		_, err = tx.ExecContext(r.Context(), `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, note, at) VALUES (?, ?, 'sprint_closed', ?, ?, ?, ?)`,
			projID, u.ID, name, targetName, note, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	movedToLabel := "backlog"
	if targetName != nil {
		movedToLabel = *targetName
	}
	doneLabel := fmt.Sprintf("%d/%d", doneCount, plannedCount)
	s.audit.Log(r.Context(), &u.ID, "sprint_closed", "sprint", &id, name, []audit.Change{
		{Field: "moved_to", To: &movedToLabel},
		{Field: "done", To: &doneLabel},
	})
	sp, _ := s.loadSprint(r.Context(), id)
	httpx.Success(w, sp)
}

// Templates

func (s *Service) HandleTemplates(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !access.CanCreateProject(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.projects.templates_forbidden"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, description, statuses, phases, uses_sprints, tasks, from_project, created_by, created_at
		FROM project_templates ORDER BY name ASC
	`)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if m := scanTemplateRow(rows); m != nil {
			out = append(out, m)
		}
	}
	httpx.Success(w, out)
}

func (s *Service) HandleSaveTemplate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := s.manageable(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanCreateProject(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.projects.templates_forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name := strings.TrimSpace(strVal(input["name"]))
	if name == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.projects.template_name_required"))
		return
	}
	if support.RuneLen(name) > 80 {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.projects.template_name_max"))
		return
	}
	var exists int
	_ = s.db.QueryRowContext(r.Context(), `SELECT 1 FROM project_templates WHERE name = ? LIMIT 1`, name).Scan(&exists)
	if exists == 1 {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.projects.template_name_taken"))
		return
	}

	phaseNames := map[int64]string{}
	var phasesList []string
	for _, ph := range p.Phases {
		phaseNames[ph.ID] = ph.Name
		phasesList = append(phasesList, ph.Name)
	}
	if phasesList == nil {
		phasesList = []string{}
	}

	rows, err := s.db.QueryContext(r.Context(), `SELECT title, description, priority, phase_id FROM tasks WHERE project_id = ? ORDER BY id ASC`, p.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	tplTasks := []map[string]any{}
	for rows.Next() {
		var title, desc, pri string
		var phID sql.NullInt64
		if rows.Scan(&title, &desc, &pri, &phID) == nil {
			item := map[string]any{"title": title, "description": desc, "priority": pri}
			if phID.Valid {
				if pn, ok := phaseNames[phID.Int64]; ok {
					item["phase"] = pn
				}
			}
			tplTasks = append(tplTasks, item)
		}
	}
	_ = rows.Close()
	if len(tplTasks) > 200 {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.projects.template_tasks_max", map[string]any{"max": 200}))
		return
	}

	var statusesList []map[string]any
	for _, st := range p.Statuses {
		statusesList = append(statusesList, map[string]any{"key": st.Key, "name": st.Name, "name_en": st.NameEn, "tone": st.Tone})
	}
	stJSON, _ := json.Marshal(statusesList)
	phJSON, _ := json.Marshal(phasesList)
	tkJSON, _ := json.Marshal(tplTasks)
	usInt := 0
	if p.UsesSprints {
		usInt = 1
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	desc := strings.TrimSpace(strVal(input["description"]))

	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO project_templates (name, description, statuses, phases, uses_sprints, tasks, from_project, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, name, desc, string(stJSON), string(phJSON), usInt, string(tkJSON), p.Name, u.ID, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	tplID, _ := res.LastInsertId()
	row := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, description, statuses, phases, uses_sprints, tasks, from_project, created_by, created_at
		FROM project_templates WHERE id = ?
	`, tplID)
	httpx.Success(w, scanTemplateRow(row), http.StatusCreated)
}

func (s *Service) HandleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var name string
	var createdBy int64
	err = s.db.QueryRowContext(r.Context(), `SELECT name, created_by FROM project_templates WHERE id = ?`, id).Scan(&name, &createdBy)
	if err == sql.ErrNoRows || !access.CanCreateProject(u) {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.projects.template_not_found_404"))
		return
	}
	if !u.IsCEO() && u.ID != createdBy {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.projects.template_delete_forbidden"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM project_templates WHERE id = ?`, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(r.Context(), &u.ID, "template_deleted", "template", &id, name, nil)
	httpx.Success(w, nil)
}

// Helpers

func (s *Service) manageable(ctx context.Context, u *store.User, projectID int64) (*access.Project, error) {
	p, err := access.ProjectForOrFail(ctx, s.db, u, projectID)
	if err != nil {
		return nil, err
	}
	if !access.CanManageProjectSetup(u, p) {
		return nil, httpx.ErrForbidden("api.projects.manage_forbidden")
	}
	return p, nil
}

func (s *Service) checkFields(ctx context.Context, fields map[string]any, current *access.Project) error {
	if _, ok := fields["name"]; ok {
		nm := strings.TrimSpace(strVal(fields["name"]))
		if nm == "" {
			return httpx.ErrValidation("name", "api.projects.name_required")
		}
		if support.RuneLen(nm) > 120 {
			return httpx.ErrValidation("name", "api.projects.name_max")
		}
	}
	if _, ok := fields["description"]; ok && support.RuneLen(strVal(fields["description"])) > 5000 {
		return httpx.ErrValidation("description", "api.projects.description_max")
	}
	start := ""
	end := ""
	if current != nil {
		start = current.Start
		end = current.End
	}
	if _, ok := fields["start"]; ok {
		st := strVal(fields["start"])
		if !support.IsRealDay(st) {
			return httpx.ErrValidation("start", "api.projects.start_required")
		}
		start = st
	}
	if _, ok := fields["end"]; ok {
		en := strVal(fields["end"])
		if !support.IsRealDay(en) {
			return httpx.ErrValidation("end", "api.projects.end_required")
		}
		end = en
	}
	if start != "" && end != "" && end < start {
		return httpx.ErrValidation("end", "api.projects.end_before_start")
	}
	if _, ok := fields["pm_id"]; ok {
		pmID := toInt64Val(fields["pm_id"])
		var role string
		var active int
		err := s.db.QueryRowContext(ctx, `SELECT role, active FROM users WHERE id = ?`, pmID).Scan(&role, &active)
		if err != nil || active == 0 || role != "pm" {
			return httpx.ErrValidation("pm_id", "api.projects.pm_invalid")
		}
	}
	if _, ok := fields["member_ids"]; ok {
		ids := toInt64Slice(fields["member_ids"])
		if len(ids) > 0 {
			uniq := uniqueInt64(ids)
			ph, args := dbx.InInt64(uniq)
			var cnt int
			_ = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM users WHERE id IN (%s) AND active = 1 AND role != 'admin'`, ph), args...).Scan(&cnt)
			if cnt != len(uniq) {
				return httpx.ErrValidation("member_ids", "api.projects.members_invalid")
			}
		}
	}
	return nil
}

func (s *Service) checkSprint(ctx context.Context, input map[string]any, projectID int64, selfID *int64) (string, string, string, error) {
	name := strings.TrimSpace(strVal(input["name"]))
	if name == "" {
		return "", "", "", httpx.ErrValidation("name", "api.projects.sprint_name_required")
	}
	if support.RuneLen(name) > 60 {
		return "", "", "", httpx.ErrValidation("name", "api.projects.sprint_name_max")
	}
	start := strVal(input["start"])
	end := strVal(input["end"])
	if !support.IsRealDay(start) {
		return "", "", "", httpx.ErrValidation("start", "api.projects.sprint_start_required")
	}
	if !support.IsRealDay(end) {
		return "", "", "", httpx.ErrValidation("end", "api.projects.sprint_end_required")
	}
	if end < start {
		return "", "", "", httpx.ErrValidation("end", "api.projects.sprint_end_before_start")
	}

	q := `SELECT name FROM sprints WHERE project_id = ? AND start <= ? AND ` + "`end`" + ` >= ?`
	args := []any{projectID, end, start}
	if selfID != nil {
		q += ` AND id != ?`
		args = append(args, *selfID)
	}
	q += ` LIMIT 1`
	var clash string
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&clash); err == nil {
		return "", "", "", httpx.ErrValidation("start", "api.projects.sprint_overlap", map[string]any{"name": clash})
	}
	return name, start, end, nil
}

func (s *Service) loadSprint(ctx context.Context, id int64) (map[string]any, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, project_id, name, start, `+"`end`"+`, closed_at, result FROM sprints WHERE id = ?`, id)
	return scanSprintRow(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSprintRow(sc scanner) (map[string]any, error) {
	var id, projID int64
	var name string
	var startRaw, endRaw, closedRaw any
	var resultRaw sql.NullString
	if err := sc.Scan(&id, &projID, &name, &startRaw, &endRaw, &closedRaw, &resultRaw); err != nil {
		return nil, err
	}
	start := derefStr(dbx.ParseDateOnly(startRaw))
	end := derefStr(dbx.ParseDateOnly(endRaw))
	closedAt := dbx.ParseTime(closedRaw)
	planned := 1.0
	if closedAt == nil {
		planned = support.PlannedRatio(start, end, support.BaghdadToday())
	}
	m := map[string]any{
		"id":            id,
		"project_id":    projID,
		"name":          name,
		"start":         start,
		"end":           end,
		"planned_ratio": planned,
	}
	if closedAt != nil {
		m["closed_at"] = store.IsoTime(closedAt)
	}
	if resultRaw.Valid && strings.TrimSpace(resultRaw.String) != "" {
		var res any
		if json.Unmarshal([]byte(resultRaw.String), &res) == nil {
			m["result"] = res
		}
	}
	return m, nil
}

func scanTemplateRow(sc scanner) map[string]any {
	var id, createdBy int64
	var name, desc, statusesRaw, phasesRaw, tasksRaw string
	var usesInt int
	var fromProj sql.NullString
	var createdRaw any
	if err := sc.Scan(&id, &name, &desc, &statusesRaw, &phasesRaw, &usesInt, &tasksRaw, &fromProj, &createdBy, &createdRaw); err != nil {
		return nil
	}
	m := map[string]any{
		"id":           id,
		"name":         name,
		"description":  desc,
		"statuses":     access.DecodeJSONArray(statusesRaw),
		"phases":       access.DecodeJSONArray(phasesRaw),
		"uses_sprints": usesInt != 0,
		"tasks":        access.DecodeJSONArray(tasksRaw),
		"created_by":   createdBy,
		"created_at":   store.IsoTime(dbx.ParseTime(createdRaw)),
	}
	if fromProj.Valid && fromProj.String != "" {
		m["from_project"] = fromProj.String
	}
	return m
}

func withPM(ids []int64, pmID int64) []int64 {
	return uniqueInt64(append([]int64{pmID}, ids...))
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

func toInt64Slice(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []int64
	for _, item := range arr {
		if n := toInt64Val(item); n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolVal(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
