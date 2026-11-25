package monitoring

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
	"followapi/internal/performance"
	"followapi/internal/store"
	"followapi/internal/support"
)

type Service struct {
	db            *dbx.DB
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, notifications: notifSvc, advanced: advSvc}
}

func (s *Service) HandleNotes(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	q := `
		SELECT n.id, n.kind, n.title, n.body, n.about_type, n.about_id, n.about_label, n.about_label_en,
		       n.tone, n.subject, n.people, n.author_id, n.created_at, n.sent_at, n.ceo_state, n.ceo_reply,
		       n.replied_at, n.facts, n.triage, n.forward_note, n.monitor_reply, n.triaged_by, n.triaged_at
		FROM monitor_notes n
	`
	var args []any
	if !u.IsCEO() {
		if u.Role == "monitor" {
			q += ` WHERE n.author_id = ? OR n.kind = 'request' OR EXISTS (SELECT 1 FROM monitor_note_shares sh WHERE sh.note_id = n.id AND sh.user_id = ?)`
		} else {
			q += ` WHERE n.author_id = ? OR EXISTS (SELECT 1 FROM monitor_note_shares sh WHERE sh.note_id = n.id AND sh.user_id = ?)`
		}
		args = append(args, u.ID, u.ID)
	}
	q += ` ORDER BY COALESCE(n.sent_at, n.created_at) DESC, n.id DESC`
	notes, err := s.queryNotes(r.Context(), u, q, args...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, notes)
}

func (s *Service) HandleCreate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	kind := strVal(input["kind"])
	isRequest := kind == "request"
	if isRequest && u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.request_ceo_forbidden"))
		return
	}
	if isRequest {
		if err := s.advanced.RequireFeature(r.Context(), u, "requests"); err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
	} else if !u.IsCEO() && u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.write_forbidden"))
		return
	}
	if kind != "report" && kind != "feedback" && kind != "problem" && kind != "request" {
		httpx.HandleErr(w, r, httpx.ErrValidation("kind", "api.monitoring.kind_invalid"))
		return
	}

	subject := strings.TrimSpace(strVal(input["subject"]))
	if isRequest && subject == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("subject", "api.monitoring.subject_required"))
		return
	}
	if support.RuneLen(subject) > 120 {
		httpx.HandleErr(w, r, httpx.ErrValidation("subject", "api.monitoring.subject_max"))
		return
	}
	title := strings.TrimSpace(strVal(input["title"]))
	if title == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.monitoring.title_required"))
		return
	}
	if support.RuneLen(title) > 160 {
		httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.monitoring.title_max"))
		return
	}
	body := strings.TrimSpace(strVal(input["body"]))
	if body == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.monitoring.body_required"))
		return
	}
	if support.RuneLen(body) > 5000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.monitoring.body_max"))
		return
	}

	rawAbout, _ := input["about"].(map[string]any)
	if rawAbout == nil {
		rawAbout = map[string]any{"type": "general"}
	}
	if kind == "feedback" {
		if strVal(rawAbout["type"]) != "person" {
			httpx.HandleErr(w, r, httpx.ErrValidation("about", "api.monitoring.feedback_person_only"))
			return
		}
		if toInt64Val(rawAbout["id"]) == u.ID {
			httpx.HandleErr(w, r, httpx.ErrValidation("about", "api.monitoring.feedback_not_self"))
			return
		}
	}

	rawPeopleIDs := uniqueInt64(toInt64Slice(input["people_ids"]))
	if len(rawPeopleIDs) > 30 {
		httpx.HandleErr(w, r, httpx.ErrValidation("people_ids", "api.monitoring.people_max"))
		return
	}
	var people []map[string]any
	if len(rawPeopleIDs) > 0 {
		uMap, _ := store.LoadUsersByIDs(r.Context(), s.db, "", rawPeopleIDs)
		for _, pid := range rawPeopleIDs {
			pu := uMap[pid]
			if pu == nil || !pu.Active {
				httpx.HandleErr(w, r, httpx.ErrValidation("people_ids", "api.monitoring.people_invalid"))
				return
			}
			ent := map[string]any{"id": pu.ID, "label": pu.Name}
			if pu.NameEn != nil && *pu.NameEn != "" {
				ent["label_en"] = *pu.NameEn
			}
			people = append(people, ent)
		}
	}

	if pv, hasP := input["period"]; hasP && pv != nil && kind != "report" {
		httpx.HandleErr(w, r, httpx.ErrValidation("period", "api.monitoring.period_report_only"))
		return
	}
	aboutTarget := rawAbout
	if isRequest {
		aboutTarget = map[string]any{"type": "general"}
	}
	about, err := s.resolveAbout(r.Context(), aboutTarget)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var factsJSON *string
	if perMap, ok := input["period"].(map[string]any); ok {
		facts, err := s.buildFacts(r.Context(), about, strVal(perMap["from"]), strVal(perMap["to"]))
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		fb, _ := json.Marshal(facts)
		fs := string(fb)
		factsJSON = &fs
	}

	var shareIDs []int64
	if !isRequest {
		shareIDs = uniqueInt64(toInt64Slice(input["share_ids"]))
		if len(shareIDs) > 0 {
			if err := s.validateShareIDs(r.Context(), shareIDs); err != nil {
				httpx.HandleErr(w, r, err)
				return
			}
		}
	}

	var activeMonitors []int64
	if isRequest {
		rows, _ := s.db.QueryContext(r.Context(), `SELECT id FROM users WHERE active = 1 AND role = 'monitor' AND id != ?`, u.ID)
		if rows != nil {
			for rows.Next() {
				var mid int64
				if rows.Scan(&mid) == nil {
					activeMonitors = append(activeMonitors, mid)
				}
			}
			_ = rows.Close()
		}
	}

	var straightToCEO bool
	if !isRequest {
		straightToCEO, _ = input["send"].(bool)
	} else {
		straightToCEO = u.Role == "monitor" || len(activeMonitors) == 0
	}

	var triagePtr *string
	if isRequest && !straightToCEO {
		t := "pending"
		triagePtr = &t
	}

	var tonePtr *string
	if kind == "feedback" {
		t := "improve"
		if strVal(input["tone"]) == "praise" {
			t = "praise"
		}
		tonePtr = &t
	}
	var subjPtr *string
	if subject != "" {
		subjPtr = &subject
	}
	var peopleJSON *string
	if len(people) > 0 {
		pb, _ := json.Marshal(people)
		ps := string(pb)
		peopleJSON = &ps
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var sentAtPtr, ceoStatePtr *string
	if straightToCEO {
		sentAtPtr = &nowStr
		cs := "new"
		ceoStatePtr = &cs
	}

	var shareUsers map[int64]*store.User
	if len(shareIDs) > 0 {
		shareUsers, _ = store.LoadUsersByIDs(r.Context(), s.db, "", shareIDs)
	}
	var noteID int64
	var sharedAdded []int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO monitor_notes (kind, title, body, about_type, about_id, about_label, about_label_en, tone, subject, people, author_id, created_at, sent_at, ceo_state, facts, triage)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, kind, title, body, about.typeStr, about.id, about.label, about.labelEn, tonePtr, subjPtr, peopleJSON, u.ID, nowStr, sentAtPtr, ceoStatePtr, factsJSON, triagePtr)
		if err != nil {
			return err
		}
		noteID, _ = res.LastInsertId()
		if len(shareIDs) > 0 {
			sharedAdded = s.applySharesTx(r.Context(), tx, noteID, u.ID, shareIDs, shareUsers, nowStr)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if straightToCEO {
		s.notifyCEOs(r.Context(), title, u.ID)
	} else if triagePtr != nil && *triagePtr == "pending" && len(activeMonitors) > 0 {
		s.notifications.Notify(r.Context(), activeMonitors, "note_sent", notifications.About{
			ActorID: &u.ID,
			Data:    map[string]any{"title": title, "queue": "monitor"},
		})
	}
	if len(sharedAdded) > 0 {
		s.notifications.Notify(r.Context(), sharedAdded, "note_sent", notifications.About{
			ActorID: &u.ID,
			Data:    map[string]any{"title": title, "queue": "received"},
		})
	}

	note, _ := s.findOne(r.Context(), u, noteID)
	httpx.Success(w, note, http.StatusCreated)
}

func (s *Service) HandleSend(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	meta, err := s.noteMetaFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !u.IsCEO() && meta.authorID != u.ID {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.send_forbidden"))
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(r.Context(), `UPDATE monitor_notes SET sent_at = ?, ceo_state = 'new' WHERE id = ?`, nowStr, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.notifyCEOs(r.Context(), meta.title, u.ID)
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

func (s *Service) HandleShare(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	meta, err := s.noteMetaFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if meta.kind == "request" || (!u.IsCEO() && meta.authorID != u.ID) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.share_forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rawVal := input["user_ids"]
	if rawVal == nil {
		rawVal = input["share_ids"]
	}
	rawIDs := uniqueInt64(toInt64Slice(rawVal))
	if len(rawIDs) == 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("share_ids", "api.monitoring.share_min"))
		return
	}
	if err := s.validateShareIDs(r.Context(), rawIDs); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	shareUsers, _ := store.LoadUsersByIDs(r.Context(), s.db, "", rawIDs)
	var added []int64
	_ = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		added = s.applySharesTx(r.Context(), tx, id, meta.authorID, rawIDs, shareUsers, nowStr)
		return nil
	})
	if len(added) > 0 {
		s.notifications.Notify(r.Context(), added, "note_sent", notifications.About{
			ActorID: &u.ID,
			Data:    map[string]any{"title": meta.title, "queue": "received"},
		})
	}
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

func (s *Service) HandleMarkSeen(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := s.noteMetaFor(r.Context(), u, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var shID int64
	var seenRaw any
	if s.db.QueryRowContext(r.Context(), `SELECT id, seen_at FROM monitor_note_shares WHERE note_id = ? AND user_id = ? LIMIT 1`, id, u.ID).Scan(&shID, &seenRaw) != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.monitoring.not_found"))
		return
	}
	if dbx.ParseTime(seenRaw) == nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE monitor_note_shares SET seen_at = ? WHERE id = ?`, dbx.FormatDBTime(time.Now().UTC()), shID)
	}
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

func (s *Service) HandleReceived(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	q := `
		SELECT n.id, n.kind, n.title, n.body, n.about_type, n.about_id, n.about_label, n.about_label_en,
		       n.tone, n.subject, n.people, n.author_id, n.created_at, n.sent_at, n.ceo_state, n.ceo_reply,
		       n.replied_at, n.facts, n.triage, n.forward_note, n.monitor_reply, n.triaged_by, n.triaged_at
		FROM monitor_notes n
		WHERE EXISTS (SELECT 1 FROM monitor_note_shares sh WHERE sh.note_id = n.id AND sh.user_id = ?)
		ORDER BY n.created_at DESC, n.id DESC
	`
	notes, err := s.queryNotes(r.Context(), u, q, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, notes)
}

func (s *Service) HandleForwardRequest(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() && u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.request_triage_forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	meta, err := s.waitingRequestFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, _ := httpx.DecodeMap(r)
	text := strings.TrimSpace(strVal(input["note"]))
	if support.RuneLen(text) > 2000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("note", "api.monitoring.forward_note_max"))
		return
	}
	var fNote *string
	if text != "" {
		fNote = &text
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE monitor_notes SET triage = 'forwarded', triaged_by = ?, triaged_at = ?, forward_note = COALESCE(?, forward_note), sent_at = ?, ceo_state = 'new'
		WHERE id = ?
	`, u.ID, nowStr, fNote, nowStr, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.notifyCEOs(r.Context(), meta.title, u.ID)
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

func (s *Service) HandleAnswerRequest(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() && u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.request_triage_forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	meta, err := s.waitingRequestFor(r.Context(), u, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, _ := httpx.DecodeMap(r)
	reply := strings.TrimSpace(strVal(input["reply"]))
	if reply == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("reply", "api.monitoring.reply_required"))
		return
	}
	if support.RuneLen(reply) > 2000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("reply", "api.monitoring.reply_max"))
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE monitor_notes SET triage = 'answered', triaged_by = ?, triaged_at = ?, monitor_reply = ? WHERE id = ?
	`, u.ID, nowStr, reply, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.notifications.Notify(r.Context(), []int64{meta.authorID}, "note_answered", notifications.About{
		ActorID: &u.ID,
		Data:    map[string]any{"title": meta.title, "note": reply, "note_kind": "request"},
	})
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

func (s *Service) HandleReportFacts(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() && u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.reports_forbidden"))
		return
	}
	t := r.URL.Query().Get("type")
	if t != "team" && t != "person" && t != "project" {
		httpx.HandleErr(w, r, httpx.ErrValidation("about", "api.monitoring.about_not_general"))
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	about, err := s.resolveAbout(r.Context(), map[string]any{"type": t, "id": id})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	facts, err := s.buildFacts(r.Context(), about, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, facts)
}

func (s *Service) HandleInbox(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.inbox_forbidden"))
		return
	}
	q := `
		SELECT n.id, n.kind, n.title, n.body, n.about_type, n.about_id, n.about_label, n.about_label_en,
		       n.tone, n.subject, n.people, n.author_id, n.created_at, n.sent_at, n.ceo_state, n.ceo_reply,
		       n.replied_at, n.facts, n.triage, n.forward_note, n.monitor_reply, n.triaged_by, n.triaged_at
		FROM monitor_notes n
		WHERE n.sent_at IS NOT NULL
		ORDER BY n.sent_at DESC, n.id DESC
	`
	notes, err := s.queryNotes(r.Context(), u, q)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, notes)
}

func (s *Service) HandleAnswerInbox(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.monitoring.inbox_forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	meta, err := s.noteMetaFor(r.Context(), u, id)
	if err != nil || !meta.hasSent {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.monitoring.not_found"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	reply := strings.TrimSpace(strVal(input["reply"]))
	if support.RuneLen(reply) > 2000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("reply", "api.monitoring.reply_max"))
		return
	}
	state := strVal(input["state"])
	if state != "seen" && state != "done" {
		httpx.HandleErr(w, r, httpx.ErrValidation("state", "api.monitoring.state_invalid"))
		return
	}
	reopen, _ := input["reopen"].(bool)
	nextState := state
	if reopen {
		nextState = "seen"
	} else if meta.ceoState == "done" && state == "seen" && reply == "" {
		nextState = "done"
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	if reply != "" {
		_, err = s.db.ExecContext(r.Context(), `UPDATE monitor_notes SET ceo_state = ?, ceo_reply = ?, replied_at = ? WHERE id = ?`,
			nextState, reply, nowStr, id)
	} else {
		_, err = s.db.ExecContext(r.Context(), `UPDATE monitor_notes SET ceo_state = ? WHERE id = ?`, nextState, id)
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if reply != "" {
		s.notifications.Notify(r.Context(), []int64{meta.authorID}, "note_answered", notifications.About{
			ActorID: &u.ID,
			Data:    map[string]any{"title": meta.title, "note": reply, "note_kind": meta.kind},
		})
	}
	note, _ := s.findOne(r.Context(), u, id)
	httpx.Success(w, note)
}

// Helpers

type noteMeta struct {
	id       int64
	kind     string
	title    string
	authorID int64
	hasSent  bool
	ceoState string
	triage   string
}

func (s *Service) noteMetaFor(ctx context.Context, u *store.User, id int64) (noteMeta, error) {
	var m noteMeta
	var sentRaw any
	var ceoSt, tr sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, title, author_id, sent_at, ceo_state, triage FROM monitor_notes WHERE id = ?`, id).
		Scan(&m.id, &m.kind, &m.title, &m.authorID, &sentRaw, &ceoSt, &tr)
	if err != nil {
		return m, httpx.ErrNotFound("api.monitoring.not_found")
	}
	m.hasSent = dbx.ParseTime(sentRaw) != nil
	m.ceoState = ceoSt.String
	m.triage = tr.String
	canSee := u.IsCEO() || m.authorID == u.ID || (u.Role == "monitor" && m.kind == "request")
	if !canSee {
		var sh int
		_ = s.db.QueryRowContext(ctx, `SELECT 1 FROM monitor_note_shares WHERE note_id = ? AND user_id = ? LIMIT 1`, id, u.ID).Scan(&sh)
		canSee = sh == 1
	}
	if !canSee {
		return m, httpx.ErrNotFound("api.monitoring.not_found")
	}
	return m, nil
}

func (s *Service) waitingRequestFor(ctx context.Context, u *store.User, id int64) (noteMeta, error) {
	m, err := s.noteMetaFor(ctx, u, id)
	if err != nil || m.kind != "request" {
		return m, httpx.ErrNotFound("api.monitoring.request_not_found")
	}
	if m.triage != "pending" {
		return m, httpx.ErrConflict("api.monitoring.request_already_handled")
	}
	return m, nil
}

func (s *Service) findOne(ctx context.Context, u *store.User, id int64) (map[string]any, error) {
	q := `
		SELECT n.id, n.kind, n.title, n.body, n.about_type, n.about_id, n.about_label, n.about_label_en,
		       n.tone, n.subject, n.people, n.author_id, n.created_at, n.sent_at, n.ceo_state, n.ceo_reply,
		       n.replied_at, n.facts, n.triage, n.forward_note, n.monitor_reply, n.triaged_by, n.triaged_at
		FROM monitor_notes n WHERE n.id = ?
	`
	list, err := s.queryNotes(ctx, u, q, id)
	if err != nil || len(list) == 0 {
		return nil, httpx.ErrNotFound("api.monitoring.not_found")
	}
	return list[0], nil
}

func (s *Service) queryNotes(ctx context.Context, viewer *store.User, q string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	type rawNote struct {
		id, authorID                   int64
		kind, title, body, aboutType   string
		aboutID, triagedBy             sql.NullInt64
		aboutLbl, aboutEn, tone, subj  sql.NullString
		peopleRaw, ceoSt, ceoRep, facR sql.NullString
		triage, fwd, monRep            sql.NullString
		crRaw, sentRaw, repRaw, triRaw any
	}
	var list []rawNote
	var ids []int64
	for rows.Next() {
		var r rawNote
		if rows.Scan(&r.id, &r.kind, &r.title, &r.body, &r.aboutType, &r.aboutID, &r.aboutLbl, &r.aboutEn,
			&r.tone, &r.subj, &r.peopleRaw, &r.authorID, &r.crRaw, &r.sentRaw, &r.ceoSt, &r.ceoRep,
			&r.repRaw, &r.facR, &r.triage, &r.fwd, &r.monRep, &r.triagedBy, &r.triRaw) == nil {
			list = append(list, r)
			ids = append(ids, r.id)
		}
	}
	_ = rows.Close()

	sharesByNote := map[int64][]map[string]any{}
	if len(ids) > 0 {
		ph, inArgs := dbx.InInt64(ids)
		sRows, _ := s.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT note_id, user_id, label, label_en, shared_at, seen_at
			FROM monitor_note_shares WHERE note_id IN (%s) ORDER BY id ASC
		`, ph), inArgs...)
		if sRows != nil {
			for sRows.Next() {
				var nid, uid int64
				var lbl string
				var lblEn sql.NullString
				var shRaw, snRaw any
				if sRows.Scan(&nid, &uid, &lbl, &lblEn, &shRaw, &snRaw) == nil {
					sharesByNote[nid] = append(sharesByNote[nid], store.CleanMap(map[string]any{
						"id":       uid,
						"label":    lbl,
						"label_en": nullStrPtr(lblEn),
						"at":       store.IsoTime(dbx.ParseTime(shRaw)),
						"seen_at":  store.IsoTime(dbx.ParseTime(snRaw)),
					}))
				}
			}
			_ = sRows.Close()
		}
	}

	canSeeForward := viewer != nil && (viewer.IsCEO() || viewer.Role == "monitor")
	out := make([]map[string]any, 0, len(list))
	for _, r := range list {
		about := store.CleanMap(map[string]any{
			"type":     r.aboutType,
			"id":       nullIntPtr(r.aboutID),
			"label":    nullStrPtr(r.aboutLbl),
			"label_en": nullStrPtr(r.aboutEn),
		})
		var people any
		if r.peopleRaw.Valid && strings.TrimSpace(r.peopleRaw.String) != "" {
			var p []any
			if json.Unmarshal([]byte(r.peopleRaw.String), &p) == nil && len(p) > 0 {
				people = p
			}
		}
		var facts any
		if r.facR.Valid && strings.TrimSpace(r.facR.String) != "" {
			var f map[string]any
			if json.Unmarshal([]byte(r.facR.String), &f) == nil {
				facts = f
			}
		}
		var fwdPtr *string
		if canSeeForward {
			fwdPtr = nullStrPtr(r.fwd)
		}
		var shared any
		if sh := sharesByNote[r.id]; len(sh) > 0 {
			shared = sh
		}
		out = append(out, store.CleanMap(map[string]any{
			"id":            r.id,
			"kind":          r.kind,
			"title":         r.title,
			"body":          r.body,
			"about":         about,
			"tone":          nullStrPtr(r.tone),
			"subject":       nullStrPtr(r.subj),
			"people":        people,
			"author_id":     r.authorID,
			"created_at":    store.IsoTime(dbx.ParseTime(r.crRaw)),
			"sent_at":       store.IsoTime(dbx.ParseTime(r.sentRaw)),
			"ceo_state":     nullStrPtr(r.ceoSt),
			"ceo_reply":     nullStrPtr(r.ceoRep),
			"replied_at":    store.IsoTime(dbx.ParseTime(r.repRaw)),
			"facts":         facts,
			"triage":        nullStrPtr(r.triage),
			"forward_note":  fwdPtr,
			"monitor_reply": nullStrPtr(r.monRep),
			"triaged_by":    nullIntPtr(r.triagedBy),
			"triaged_at":    store.IsoTime(dbx.ParseTime(r.triRaw)),
			"shared_with":   shared,
		}))
	}
	return out, nil
}

func (s *Service) validateShareIDs(ctx context.Context, ids []int64) error {
	if len(ids) > 30 {
		return httpx.ErrValidation("share_ids", "api.monitoring.people_max")
	}
	ph, args := dbx.InInt64(ids)
	var cnt int
	_ = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM users WHERE id IN (%s) AND active = 1`, ph), args...).Scan(&cnt)
	if cnt != len(ids) {
		return httpx.ErrValidation("share_ids", "api.monitoring.people_invalid")
	}
	return nil
}

func (s *Service) applySharesTx(ctx context.Context, tx *sql.Tx, noteID, authorID int64, ids []int64, uMap map[int64]*store.User, nowStr string) []int64 {
	already := map[int64]bool{}
	rows, _ := tx.QueryContext(ctx, `SELECT user_id FROM monitor_note_shares WHERE note_id = ?`, noteID)
	if rows != nil {
		for rows.Next() {
			var uid int64
			if rows.Scan(&uid) == nil {
				already[uid] = true
			}
		}
		_ = rows.Close()
	}
	var added []int64
	for _, id := range ids {
		tu := uMap[id]
		if tu == nil || !tu.Active || tu.ID == authorID || tu.IsCEO() || already[tu.ID] {
			continue
		}
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO monitor_note_shares (note_id, user_id, label, label_en, shared_at)
			VALUES (?, ?, ?, ?, ?)
		`, noteID, tu.ID, tu.Name, tu.NameEn, nowStr)
		added = append(added, tu.ID)
		already[tu.ID] = true
	}
	return added
}

type aboutInfo struct {
	typeStr string
	id      *int64
	label   *string
	labelEn *string
}

func (a aboutInfo) toMap() map[string]any {
	return store.CleanMap(map[string]any{
		"type":     a.typeStr,
		"id":       a.id,
		"label":    a.label,
		"label_en": a.labelEn,
	})
}

func (s *Service) resolveAbout(ctx context.Context, raw map[string]any) (aboutInfo, error) {
	t := strVal(raw["type"])
	if t == "general" || t == "" {
		return aboutInfo{typeStr: "general"}, nil
	}
	id := toInt64Val(raw["id"])
	if id <= 0 {
		return aboutInfo{}, httpx.ErrValidation("about", "api.monitoring.about_invalid")
	}
	var lbl string
	var lblEn sql.NullString
	var err error
	switch t {
	case "team":
		err = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM teams WHERE id = ?`, id).Scan(&lbl, &lblEn)
	case "project":
		err = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM projects WHERE id = ?`, id).Scan(&lbl, &lblEn)
	case "person":
		err = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM users WHERE id = ? AND active = 1`, id).Scan(&lbl, &lblEn)
	default:
		err = sql.ErrNoRows
	}
	if err != nil {
		return aboutInfo{}, httpx.ErrValidation("about", "api.monitoring.about_invalid")
	}
	return aboutInfo{typeStr: t, id: &id, label: &lbl, labelEn: nullStrPtr(lblEn)}, nil
}

func (s *Service) buildFacts(ctx context.Context, about aboutInfo, from, to string) (map[string]any, error) {
	if !support.IsRealDay(from) || !support.IsRealDay(to) {
		return nil, httpx.ErrValidation("period", "api.monitoring.period_invalid")
	}
	if from > to {
		return nil, httpx.ErrValidation("period", "api.monitoring.period_order")
	}
	if about.typeStr == "general" || about.id == nil {
		return nil, httpx.ErrValidation("about", "api.monitoring.about_not_general")
	}
	today := support.BaghdadToday()

	allProjects, _ := access.LoadAllProjects(ctx, s.db)
	var scope []*access.Project
	var scopeIDs []int64
	projMap := map[int64]*access.Project{}
	for _, p := range allProjects {
		if about.typeStr == "project" && p.ID != *about.id {
			continue
		}
		scope = append(scope, p)
		scopeIDs = append(scopeIDs, p.ID)
		projMap[p.ID] = p
	}
	tasks, _ := performance.LoadTasksForProjects(ctx, s.db, scopeIDs, nil)
	activeUsers, _ := store.LoadAllUsers(ctx, s.db, "")

	var people []*store.User
	switch about.typeStr {
	case "team":
		for _, u := range activeUsers {
			if u.Active && u.TeamID != nil && *u.TeamID == *about.id {
				people = append(people, u)
			}
		}
	case "person":
		for _, u := range activeUsers {
			if u.Active && u.ID == *about.id {
				people = append(people, u)
			}
		}
	default:
		assSet := map[int64]bool{}
		for _, t := range tasks {
			if t.AssigneeID != nil {
				assSet[*t.AssigneeID] = true
			}
		}
		for _, u := range activeUsers {
			if u.Active && assSet[u.ID] {
				people = append(people, u)
			}
		}
	}

	peopleSet := map[int64]bool{}
	for _, u := range people {
		peopleSet[u.ID] = true
	}

	rows := []map[string]any{}
	var sumAssigned, sumDone, sumLate, sumBlocked int
	if about.typeStr == "person" {
		for _, p := range scope {
			hasTask := false
			for _, t := range tasks {
				if t.ProjectID == p.ID && t.AssigneeID != nil && *t.AssigneeID == *about.id {
					hasTask = true
					break
				}
			}
			if !hasTask {
				continue
			}
			perf := performance.ComputePersonPerf(*about.id, nil, tasks, []*access.Project{p}, from, to, today)
			asg := intVal(perf["assigned"])
			if asg <= 0 {
				continue
			}
			dn := intVal(perf["done"])
			lt := intVal(perf["late"])
			bl := intVal(perf["blocked"])
			sumAssigned += asg
			sumDone += dn
			sumLate += lt
			sumBlocked += bl
			rows = append(rows, store.CleanMap(map[string]any{
				"type":       "project",
				"id":         p.ID,
				"label":      p.Name,
				"label_en":   p.NameEn,
				"assigned":   asg,
				"done":       dn,
				"late":       lt,
				"blocked":    bl,
				"completion": perf["completion"],
			}))
		}
	} else {
		for _, u := range people {
			perf := performance.ComputePersonPerf(u.ID, u.TeamID, tasks, scope, from, to, today)
			asg := intVal(perf["assigned"])
			dn := intVal(perf["done"])
			lt := intVal(perf["late"])
			bl := intVal(perf["blocked"])
			sumAssigned += asg
			sumDone += dn
			sumLate += lt
			sumBlocked += bl
			rows = append(rows, store.CleanMap(map[string]any{
				"type":       "person",
				"id":         u.ID,
				"label":      u.Name,
				"label_en":   u.NameEn,
				"assigned":   asg,
				"done":       dn,
				"late":       lt,
				"blocked":    bl,
				"completion": perf["completion"],
			}))
		}
	}

	withDue := 0
	onTime := 0
	involvedProj := map[int64]bool{}
	for _, t := range tasks {
		p := projMap[t.ProjectID]
		if p == nil || t.AssigneeID == nil || !peopleSet[*t.AssigneeID] {
			continue
		}
		involvedProj[p.ID] = true
		fin := performance.TaskFinishedDay(p, t)
		if t.Due != nil && fin != nil && *fin >= from && *fin <= to {
			withDue++
			if *fin <= *t.Due {
				onTime++
			}
		}
	}
	var planned any = nil
	if len(involvedProj) > 0 {
		sumPl := 0.0
		cntPl := 0
		for _, p := range scope {
			if involvedProj[p.ID] {
				sumPl += support.PlannedRatio(p.Start, p.End, to)
				cntPl++
			}
		}
		if cntPl > 0 {
			planned = sumPl / float64(cntPl)
		}
	}

	var bugsRep, bugsClosed, bugsOpen int
	if len(scopeIDs) > 0 {
		ph, args := dbx.InInt64(scopeIDs)
		bRows, _ := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT status, assignee_id, created_at, closed_at FROM bugs WHERE project_id IN (%s)`, ph), args...)
		if bRows != nil {
			for bRows.Next() {
				var st string
				var assID sql.NullInt64
				var crRaw, clRaw any
				if bRows.Scan(&st, &assID, &crRaw, &clRaw) == nil {
					if about.typeStr != "project" && (!assID.Valid || !peopleSet[assID.Int64]) {
						continue
					}
					if dt := dbx.ParseTime(crRaw); dt != nil {
						d := support.ToBaghdadDate(*dt)
						if d >= from && d <= to {
							bugsRep++
						}
					}
					if dt := dbx.ParseTime(clRaw); dt != nil {
						d := support.ToBaghdadDate(*dt)
						if d >= from && d <= to {
							bugsClosed++
						}
					}
					if st != "closed" {
						bugsOpen++
					}
				}
			}
			_ = bRows.Close()
		}
	}

	comp := 0.0
	if sumAssigned > 0 {
		comp = float64(sumDone) / float64(sumAssigned)
	}
	var onTimeVal any = nil
	if withDue > 0 {
		onTimeVal = float64(onTime) / float64(withDue)
	}
	now := time.Now().UTC()

	return map[string]any{
		"about":         about.toMap(),
		"period":        map[string]any{"from": from, "to": to},
		"assigned":      sumAssigned,
		"done":          sumDone,
		"left":          sumAssigned - sumDone,
		"late":          sumLate,
		"blocked":       sumBlocked,
		"completion":    comp,
		"on_time":       onTimeVal,
		"planned":       planned,
		"bugs_reported": bugsRep,
		"bugs_closed":   bugsClosed,
		"bugs_open":     bugsOpen,
		"rows":          rows,
		"counted_at":    store.IsoTime(&now),
	}, nil
}

func (s *Service) notifyCEOs(ctx context.Context, title string, actorID int64) {
	rows, _ := s.db.QueryContext(ctx, `SELECT id FROM users WHERE active = 1 AND role = 'ceo'`)
	var ceoIDs []int64
	if rows != nil {
		for rows.Next() {
			var cid int64
			if rows.Scan(&cid) == nil {
				ceoIDs = append(ceoIDs, cid)
			}
		}
		_ = rows.Close()
	}
	s.notifications.Notify(ctx, ceoIDs, "note_sent", notifications.About{
		ActorID: &actorID,
		Data:    map[string]any{"title": title},
	})
}

func nullStrPtr(s sql.NullString) *string {
	if s.Valid && s.String != "" {
		v := s.String
		return &v
	}
	return nil
}

func nullIntPtr(i sql.NullInt64) *int64 {
	if i.Valid {
		v := i.Int64
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
	if n, ok := v.(int); ok {
		return n
	}
	return 0
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
	for _, x := range arr {
		if n := toInt64Val(x); n > 0 {
			out = append(out, n)
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
