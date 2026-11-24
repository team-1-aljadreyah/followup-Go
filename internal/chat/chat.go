package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/attachments"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/push"
	"followapi/internal/store"
	"followapi/internal/support"
	"followapi/internal/ws"
)

type Service struct {
	db            *dbx.DB
	notifications *notifications.Service
	advanced      *advanced.Service
	push          *push.Service
	attachments   *attachments.Service
	ws            *ws.Hub
}

func New(db *dbx.DB, notifSvc *notifications.Service, advSvc *advanced.Service, pushSvc *push.Service, attSvc *attachments.Service) *Service {
	return &Service{
		db:            db,
		notifications: notifSvc,
		advanced:      advSvc,
		push:          pushSvc,
		attachments:   attSvc,
	}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

var chatKeyRegex = regexp.MustCompile(`^([ptdg])-(\d+)$`)

func ParseKey(key string) (kind string, id int64, ok bool) {
	m := chatKeyRegex.FindStringSubmatch(key)
	if m == nil {
		return "", 0, false
	}
	id, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	switch m[1] {
	case "p":
		return "project", id, true
	case "t":
		return "team", id, true
	case "d":
		return "direct", id, true
	case "g":
		return "group", id, true
	}
	return "", 0, false
}

func (s *Service) ChatMembers(ctx context.Context, key string) ([]int64, bool) {
	kind, id, ok := ParseKey(key)
	if !ok {
		return nil, false
	}
	switch kind {
	case "direct":
		var lo, hi int64
		if s.db.QueryRowContext(ctx, `SELECT user_low_id, user_high_id FROM direct_conversations WHERE id = ?`, id).Scan(&lo, &hi) != nil {
			return nil, false
		}
		return []int64{lo, hi}, true
	case "group":
		var gExists int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM chat_groups WHERE id = ?`, id).Scan(&gExists) != nil {
			return nil, false
		}
		rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM chat_group_members WHERE group_id = ? ORDER BY position ASC, id ASC`, id)
		if err != nil {
			return nil, false
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var uid int64
			if rows.Scan(&uid) == nil {
				ids = append(ids, uid)
			}
		}
		return ids, true
	case "project":
		p, err := access.LoadProject(ctx, s.db, id)
		if err != nil || p == nil {
			return nil, false
		}
		ids := append([]int64{p.PMID}, p.MemberIDs...)
		ids = append(ids, s.followerIDs(ctx)...)
		return uniqueInt64(ids), true
	case "team":
		var tExists int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM teams WHERE id = ?`, id).Scan(&tExists) != nil {
			return nil, false
		}
		var ids []int64
		rows, _ := s.db.QueryContext(ctx, `SELECT id FROM users WHERE active = 1 AND team_id = ?`, id)
		if rows != nil {
			for rows.Next() {
				var uid int64
				if rows.Scan(&uid) == nil {
					ids = append(ids, uid)
				}
			}
			_ = rows.Close()
		}
		ids = append(ids, s.followerIDs(ctx)...)
		return uniqueInt64(ids), true
	}
	return nil, false
}

func (s *Service) HandleConversations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if err := s.advanced.RequireFeature(ctx, u, "chat"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	type projRow struct {
		id     int64
		name   string
		nameEn sql.NullString
		pmID   int64
	}
	type teamRow struct {
		id     int64
		name   string
		nameEn sql.NullString
	}
	type directRow struct {
		id, lo, hi int64
	}
	type groupMeta struct {
		id        int64
		name      string
		createdBy int64
		members   []int64
	}

	var (
		projs       []projRow
		projMembers = map[int64][]int64{}
		teams       []teamRow
		teamUsers   = map[int64][]int64{}
		followers   []int64
		directs     []directRow
		groups      []*groupMeta
	)

	loadProjs := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT id, name, name_en, pm_id FROM projects ORDER BY id`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p projRow
			if rows.Scan(&p.id, &p.name, &p.nameEn, &p.pmID) == nil {
				projs = append(projs, p)
			}
		}
	}
	loadProjMembers := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT project_id, user_id FROM project_members ORDER BY id ASC`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var pid, uid int64
			if rows.Scan(&pid, &uid) == nil {
				projMembers[pid] = append(projMembers[pid], uid)
			}
		}
	}
	loadTeams := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT id, name, name_en FROM teams ORDER BY id`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var t teamRow
			if rows.Scan(&t.id, &t.name, &t.nameEn) == nil {
				teams = append(teams, t)
			}
		}
	}
	loadUsers := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT id, team_id, role FROM users WHERE active = 1 ORDER BY id ASC`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var uid int64
			var tid sql.NullInt64
			var role string
			if rows.Scan(&uid, &tid, &role) == nil {
				if tid.Valid {
					teamUsers[tid.Int64] = append(teamUsers[tid.Int64], uid)
				}
				if role == "ceo" || role == "monitor" {
					followers = append(followers, uid)
				}
			}
		}
	}
	loadDirects := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT id, user_low_id, user_high_id FROM direct_conversations WHERE user_low_id = ? OR user_high_id = ? ORDER BY id`, u.ID, u.ID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var d directRow
			if rows.Scan(&d.id, &d.lo, &d.hi) == nil {
				directs = append(directs, d)
			}
		}
	}
	loadGroups := func() {
		rows, err := s.db.QueryContext(ctx, `
			SELECT g.id, g.name, g.created_by, m.user_id
			FROM chat_groups g
			JOIN chat_group_members m ON m.group_id = g.id
			WHERE EXISTS (SELECT 1 FROM chat_group_members my WHERE my.group_id = g.id AND my.user_id = ?)
			ORDER BY g.id ASC, m.position ASC, m.id ASC
		`, u.ID)
		if err != nil {
			return
		}
		defer rows.Close()
		byID := map[int64]*groupMeta{}
		for rows.Next() {
			var gid, cb, mid int64
			var name string
			if rows.Scan(&gid, &name, &cb, &mid) == nil {
				gm := byID[gid]
				if gm == nil {
					gm = &groupMeta{id: gid, name: name, createdBy: cb}
					byID[gid] = gm
					groups = append(groups, gm)
				}
				gm.members = append(gm.members, mid)
			}
		}
	}

	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(6)
		go func() { defer wg.Done(); loadProjs() }()
		go func() { defer wg.Done(); loadProjMembers() }()
		go func() { defer wg.Done(); loadTeams() }()
		go func() { defer wg.Done(); loadUsers() }()
		go func() { defer wg.Done(); loadDirects() }()
		go func() { defer wg.Done(); loadGroups() }()
		wg.Wait()
	} else {
		loadProjs()
		loadProjMembers()
		loadTeams()
		loadUsers()
		loadDirects()
		loadGroups()
	}

	conversations := []map[string]any{}
	var visibleKeys []string
	appendConv := func(key, kind, title string, titleEn sql.NullString, createdBy *int64, members []int64) {
		if !containsInt64(members, u.ID) {
			return
		}
		out := map[string]any{
			"key":        key,
			"kind":       kind,
			"title":      title,
			"member_ids": members,
			"unread":     0,
		}
		if createdBy != nil {
			out["created_by"] = *createdBy
		}
		if titleEn.Valid && titleEn.String != "" {
			out["title_en"] = titleEn.String
		}
		conversations = append(conversations, out)
		visibleKeys = append(visibleKeys, key)
	}

	for _, p := range projs {
		raw := append([]int64{p.pmID}, projMembers[p.id]...)
		raw = append(raw, followers...)
		appendConv(fmt.Sprintf("p-%d", p.id), "project", p.name, p.nameEn, nil, uniqueInt64(raw))
	}
	for _, t := range teams {
		raw := append([]int64{}, teamUsers[t.id]...)
		raw = append(raw, followers...)
		appendConv(fmt.Sprintf("t-%d", t.id), "team", t.name, t.nameEn, nil, uniqueInt64(raw))
	}
	for _, d := range directs {
		appendConv(fmt.Sprintf("d-%d", d.id), "direct", "", sql.NullString{}, nil, []int64{d.lo, d.hi})
	}
	for _, g := range groups {
		cb := g.createdBy
		appendConv(fmt.Sprintf("g-%d", g.id), "group", g.name, sql.NullString{}, &cb, g.members)
	}

	if len(visibleKeys) > 0 {
		ph := strings.Repeat("?,", len(visibleKeys))
		ph = ph[:len(ph)-1]
		keyArgs := make([]any, len(visibleKeys))
		for i, k := range visibleKeys {
			keyArgs[i] = k
		}

		unreads := map[string]int{}
		lastMsgs := map[string]msgRow{}
		var msgIDs []int64

		loadUnreads := func() {
			uArgs := make([]any, 0, len(visibleKeys)+2)
			uArgs = append(uArgs, u.ID)
			uArgs = append(uArgs, keyArgs...)
			uArgs = append(uArgs, u.ID)
			q := fmt.Sprintf(`
				SELECT m.chat_key, COUNT(*)
				FROM messages m
				LEFT JOIN chat_reads cr ON cr.chat_key = m.chat_key AND cr.user_id = ?
				WHERE m.chat_key IN (%s) AND m.author_id != ? AND m.id > COALESCE(cr.last_message_id, 0)
				GROUP BY m.chat_key
			`, ph)
			rows, err := s.db.QueryContext(ctx, q, uArgs...)
			if err != nil {
				return
			}
			defer rows.Close()
			for rows.Next() {
				var ck string
				var cnt int
				if rows.Scan(&ck, &cnt) == nil {
					unreads[ck] = cnt
				}
			}
		}
		loadLatest := func() {
			q := fmt.Sprintf(`
				SELECT m.id, m.chat_key, m.author_id, m.body, m.reply_to_id, m.mentions, m.created_at
				FROM messages m
				JOIN (
					SELECT chat_key, MAX(id) AS max_id
					FROM messages
					WHERE chat_key IN (%s)
					GROUP BY chat_key
				) latest ON latest.max_id = m.id
			`, ph)
			rows, err := s.db.QueryContext(ctx, q, keyArgs...)
			if err != nil {
				return
			}
			defer rows.Close()
			for rows.Next() {
				if m, err := scanMsgRow(rows); err == nil {
					lastMsgs[m.chatKey] = m
					msgIDs = append(msgIDs, m.id)
				}
			}
		}

		if !s.db.IsSQLite() {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); loadUnreads() }()
			go func() { defer wg.Done(); loadLatest() }()
			wg.Wait()
		} else {
			loadUnreads()
			loadLatest()
		}

		filesByMsg, _ := s.attachments.ListForMessages(ctx, msgIDs, u.ID)
		for _, conv := range conversations {
			k := conv["key"].(string)
			conv["unread"] = unreads[k]
			if m, ok := lastMsgs[k]; ok {
				conv["last_message"] = m.toJSON(filesByMsg[m.id])
			}
		}
	}

	sort.SliceStable(conversations, func(i, j int) bool {
		aAt := ""
		bAt := ""
		if lm, _ := conversations[i]["last_message"].(map[string]any); lm != nil {
			aAt = strVal(lm["created_at"])
		}
		if lm, _ := conversations[j]["last_message"].(map[string]any); lm != nil {
			bAt = strVal(lm["created_at"])
		}
		return bAt < aAt
	})

	httpx.Success(w, conversations)
}

func (s *Service) HandleMessages(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	key := chi.URLParam(r, "key")
	if _, err := s.membersForOrFail(r.Context(), u, key); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limitStr := strings.TrimSpace(r.URL.Query().Get("limit"))
	hasLimit := limitStr != ""
	limit, _ := strconv.Atoi(limitStr)

	q := `SELECT id, chat_key, author_id, body, reply_to_id, mentions, created_at FROM messages WHERE chat_key = ?`
	args := []any{key}
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	if hasLimit {
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		q += ` ORDER BY id DESC LIMIT ?`
		args = append(args, limit)
	} else {
		q += ` ORDER BY id ASC`
	}

	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	list := []msgRow{}
	var ids []int64
	for rows.Next() {
		if m, err := scanMsgRow(rows); err == nil {
			list = append(list, m)
			ids = append(ids, m.id)
		}
	}
	_ = rows.Close()

	if hasLimit {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}

	filesByMsg, _ := s.attachments.ListForMessages(r.Context(), ids, u.ID)
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		out = append(out, m.toJSON(filesByMsg[m.id]))
	}
	httpx.Success(w, out)
}

func (s *Service) HandleSend(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "chat"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	key := chi.URLParam(r, "key")
	members, err := s.membersForOrFail(r.Context(), u, key)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if strings.HasPrefix(key, "d-") {
		var otherID int64
		for _, mid := range members {
			if mid != u.ID {
				otherID = mid
				break
			}
		}
		if otherID > 0 {
			other, _ := store.FindUserByID(r.Context(), s.db, "", otherID)
			if other != nil {
				var wrote int
				_ = s.db.QueryRowContext(r.Context(), `SELECT 1 FROM messages WHERE chat_key = ? AND author_id = ? LIMIT 1`, key, other.ID).Scan(&wrote)
				if !access.CanStartDirect(u, other) && wrote != 1 {
					httpx.HandleErr(w, r, httpx.ErrForbidden("api.chat.direct_ceo_forbidden"))
					return
				}
			}
		}
	}

	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body := strings.TrimSpace(strVal(input["body"]))
	if support.RuneLen(body) > 4000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.chat.body_max"))
		return
	}
	var replyTo *int64
	if rtid := toInt64Val(input["reply_to"]); rtid > 0 {
		var ok int
		_ = s.db.QueryRowContext(r.Context(), `SELECT 1 FROM messages WHERE id = ? AND chat_key = ? LIMIT 1`, rtid, key).Scan(&ok)
		if ok != 1 {
			httpx.HandleErr(w, r, httpx.ErrValidation("reply_to", "api.chat.reply_invalid"))
			return
		}
		replyTo = &rtid
	}

	var mentions []int64
	for _, mid := range uniqueInt64(toInt64Slice(input["mentions"])) {
		if mid != u.ID && containsInt64(members, mid) {
			mentions = append(mentions, mid)
		}
	}
	if mentions == nil {
		mentions = []int64{}
	}
	mentionsJSON, _ := json.Marshal(mentions)
	now := time.Now().UTC()
	nowStr := dbx.FormatDBTime(now)

	var msgID int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO messages (chat_key, author_id, body, reply_to_id, mentions, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, key, u.ID, body, replyTo, string(mentionsJSON), nowStr)
		if err != nil {
			return err
		}
		msgID, _ = res.LastInsertId()
		return s.upsertChatReadTx(r.Context(), tx, key, u.ID, msgID, nowStr)
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if len(mentions) > 0 {
		ck := key
		s.notifications.Notify(r.Context(), mentions, "chat_mention", notifications.About{
			ActorID: &u.ID,
			ChatKey: &ck,
			Data:    map[string]any{"note": support.TruncateRunes(body, 120)},
		})
	}
	s.push.QueueForChatMessage(r.Context(), msgID, body, u, key, members)
	s.ws.NotifyChatMessage(members, key, msgID)

	msg := msgRow{id: msgID, chatKey: key, authorID: u.ID, body: body, replyTo: replyTo, mentions: mentions, createdAt: &now}
	httpx.Success(w, msg.toJSON([]map[string]any{}), http.StatusCreated)
}

func (s *Service) HandleMarkRead(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	key := chi.URLParam(r, "key")
	if _, err := s.membersForOrFail(r.Context(), u, key); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var lastID sql.NullInt64
	_ = s.db.QueryRowContext(r.Context(), `SELECT MAX(id) FROM messages WHERE chat_key = ?`, key).Scan(&lastID)
	curMax := int64(0)
	if lastID.Valid {
		curMax = lastID.Int64
	}
	var existingID int64
	err := s.db.QueryRowContext(r.Context(), `SELECT last_message_id FROM chat_reads WHERE chat_key = ? AND user_id = ?`, key, u.ID).Scan(&existingID)
	if err != nil || existingID < curMax {
		nowStr := dbx.FormatDBTime(time.Now().UTC())
		_ = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
			return s.upsertChatReadTx(r.Context(), tx, key, u.ID, curMax, nowStr)
		})
	}
	s.ws.NotifyChatRead(u.ID, key)
	httpx.Success(w, nil)
}

func (s *Service) HandleStartDirect(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "chat"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	otherID := toInt64Val(input["user_id"])
	other, _ := store.FindUserByID(r.Context(), s.db, "", otherID)
	if other == nil || !other.Active || other.ID == u.ID {
		httpx.HandleErr(w, r, httpx.ErrValidation("user_id", "api.chat.direct_invalid_user"))
		return
	}
	if !access.CanStartDirect(u, other) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.chat.direct_ceo_forbidden"))
		return
	}
	lo, hi := u.ID, other.ID
	if lo > hi {
		lo, hi = hi, lo
	}
	var directID int64
	err = s.db.QueryRowContext(r.Context(), `SELECT id FROM direct_conversations WHERE user_low_id = ? AND user_high_id = ?`, lo, hi).Scan(&directID)
	if err == sql.ErrNoRows {
		res, ierr := s.db.ExecContext(r.Context(), `INSERT INTO direct_conversations (user_low_id, user_high_id, created_at) VALUES (?, ?, ?)`,
			lo, hi, dbx.FormatDBTime(time.Now().UTC()))
		if ierr != nil {
			_ = s.db.QueryRowContext(r.Context(), `SELECT id FROM direct_conversations WHERE user_low_id = ? AND user_high_id = ?`, lo, hi).Scan(&directID)
		} else {
			directID, _ = res.LastInsertId()
		}
	}
	key := fmt.Sprintf("d-%d", directID)
	s.ws.NotifyChatChanged([]int64{lo, hi}, key)
	httpx.Success(w, s.buildConversation(r.Context(), key, u.ID, []int64{lo, hi}))
}

func (s *Service) HandleCreateGroup(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "chat"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := s.advanced.RequireFeature(r.Context(), u, "group_chats"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanCreateGroupChat(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.chat.group_create_forbidden"))
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "create_group_chat"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name, err := checkGroupName(strVal(input["name"]))
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	others, err := s.checkGroupMembers(r.Context(), toInt64Slice(input["member_ids"]), u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var gid int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `INSERT INTO chat_groups (name, created_by, created_at) VALUES (?, ?, ?)`, name, u.ID, nowStr)
		if err != nil {
			return err
		}
		gid, _ = res.LastInsertId()
		return syncGroupMembersTx(r.Context(), tx, gid, u.ID, others)
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	key := fmt.Sprintf("g-%d", gid)
	members := append([]int64{u.ID}, others...)
	s.ws.NotifyChatChanged(members, key)
	httpx.Success(w, s.buildConversation(r.Context(), key, u.ID, members), http.StatusCreated)
}

func (s *Service) HandleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	key := chi.URLParam(r, "key")
	oldMembers, err := s.membersForOrFail(r.Context(), u, key)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	kind, gid, ok := ParseKey(key)
	if !ok || kind != "group" {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.chat.group_not_found"))
		return
	}
	var curName string
	var createdBy int64
	if s.db.QueryRowContext(r.Context(), `SELECT name, created_by FROM chat_groups WHERE id = ?`, gid).Scan(&curName, &createdBy) != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.chat.group_not_found"))
		return
	}
	if !u.IsCEO() && createdBy != u.ID {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.chat.group_manage_forbidden"))
		return
	}
	change, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nextName := curName
	if _, hasName := change["name"]; hasName {
		nextName, err = checkGroupName(strVal(change["name"]))
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
	}
	var others []int64
	hasMembers := false
	if _, hasM := change["member_ids"]; hasM {
		hasMembers = true
		others, err = s.checkGroupMembers(r.Context(), toInt64Slice(change["member_ids"]), createdBy)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
	}
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if nextName != curName {
			if _, err := tx.ExecContext(r.Context(), `UPDATE chat_groups SET name = ? WHERE id = ?`, nextName, gid); err != nil {
				return err
			}
		}
		if hasMembers {
			return syncGroupMembersTx(r.Context(), tx, gid, createdBy, others)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	members, _ := s.ChatMembers(r.Context(), key)
	affected := uniqueInt64(append(append([]int64{}, oldMembers...), members...))
	s.ws.NotifyChatChanged(affected, key)
	httpx.Success(w, s.buildConversation(r.Context(), key, u.ID, members))
}

func (s *Service) HandleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	key := chi.URLParam(r, "key")
	oldMembers, err := s.membersForOrFail(r.Context(), u, key)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	kind, gid, ok := ParseKey(key)
	if !ok || kind != "group" {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.chat.group_not_found"))
		return
	}
	var createdBy int64
	if s.db.QueryRowContext(r.Context(), `SELECT created_by FROM chat_groups WHERE id = ?`, gid).Scan(&createdBy) != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.chat.group_not_found"))
		return
	}
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM chat_group_members WHERE group_id = ? AND user_id = ?`, gid, u.ID)
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM chat_reads WHERE chat_key = ? AND user_id = ?`, key, u.ID)
		if createdBy == u.ID {
			var nextOwner int64
			if tx.QueryRowContext(r.Context(), `SELECT user_id FROM chat_group_members WHERE group_id = ? ORDER BY position ASC, id ASC LIMIT 1`, gid).Scan(&nextOwner) == nil {
				_, _ = tx.ExecContext(r.Context(), `UPDATE chat_groups SET created_by = ? WHERE id = ?`, nextOwner, gid)
			}
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.ws.NotifyChatChanged(oldMembers, key)
	httpx.Success(w, nil)
}

// Helpers

func (s *Service) membersForOrFail(ctx context.Context, u *store.User, key string) ([]int64, error) {
	members, ok := s.ChatMembers(ctx, key)
	if !ok || !containsInt64(members, u.ID) {
		return nil, httpx.ErrNotFound("api.chat.not_found")
	}
	return members, nil
}

func (s *Service) followerIDs(ctx context.Context) []int64 {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM users WHERE active = 1 AND role IN ('ceo', 'monitor')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Service) buildConversation(ctx context.Context, key string, userID int64, members []int64) map[string]any {
	kind, id, _ := ParseKey(key)
	var title string
	var titleEn sql.NullString
	var createdBy *int64

	switch kind {
	case "project":
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM projects WHERE id = ?`, id).Scan(&title, &titleEn)
	case "team":
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM teams WHERE id = ?`, id).Scan(&title, &titleEn)
	case "group":
		var cb int64
		if s.db.QueryRowContext(ctx, `SELECT name, created_by FROM chat_groups WHERE id = ?`, id).Scan(&title, &cb) == nil {
			createdBy = &cb
		}
	}

	var lastRead int64
	_ = s.db.QueryRowContext(ctx, `SELECT last_message_id FROM chat_reads WHERE chat_key = ? AND user_id = ?`, key, userID).Scan(&lastRead)

	var unread int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_key = ? AND id > ? AND author_id != ?`, key, lastRead, userID).Scan(&unread)

	out := map[string]any{
		"key":        key,
		"kind":       kind,
		"title":      title,
		"member_ids": members,
		"unread":     unread,
	}
	if createdBy != nil {
		out["created_by"] = *createdBy
	}
	if titleEn.Valid && titleEn.String != "" {
		out["title_en"] = titleEn.String
	}

	row := s.db.QueryRowContext(ctx, `SELECT id, chat_key, author_id, body, reply_to_id, mentions, created_at FROM messages WHERE chat_key = ? ORDER BY id DESC LIMIT 1`, key)
	if m, err := scanMsgRow(row); err == nil {
		filesByMsg, _ := s.attachments.ListForMessages(ctx, []int64{m.id}, userID)
		out["last_message"] = m.toJSON(filesByMsg[m.id])
	}
	return out
}

func (s *Service) upsertChatReadTx(ctx context.Context, tx *sql.Tx, key string, userID, lastMsgID int64, nowStr string) error {
	if s.db.IsSQLite() {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO chat_reads (chat_key, user_id, last_message_id, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(chat_key, user_id) DO UPDATE SET last_message_id = excluded.last_message_id, updated_at = excluded.updated_at
		`, key, userID, lastMsgID, nowStr)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO chat_reads (chat_key, user_id, last_message_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE last_message_id = VALUES(last_message_id), updated_at = VALUES(updated_at)
	`, key, userID, lastMsgID, nowStr)
	return err
}

func checkGroupName(raw string) (string, error) {
	nm := strings.TrimSpace(raw)
	if nm == "" {
		return "", httpx.ErrValidation("name", "api.chat.group_name_required")
	}
	if support.RuneLen(nm) > 60 {
		return "", httpx.ErrValidation("name", "api.chat.group_name_max", map[string]any{"max": 60})
	}
	return nm, nil
}

func (s *Service) checkGroupMembers(ctx context.Context, ids []int64, ownerID int64) ([]int64, error) {
	var others []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		if id > 0 && id != ownerID && !seen[id] {
			seen[id] = true
			others = append(others, id)
		}
	}
	if len(others) > 0 {
		ph, args := dbx.InInt64(others)
		var cnt int
		_ = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM users WHERE id IN (%s) AND active = 1 AND role != 'admin'`, ph), args...).Scan(&cnt)
		if cnt != len(others) {
			return nil, httpx.ErrValidation("member_ids", "api.chat.group_members_invalid")
		}
	}
	if len(others) < 2 {
		return nil, httpx.ErrValidation("member_ids", "api.chat.group_members_min")
	}
	return others, nil
}

func syncGroupMembersTx(ctx context.Context, tx *sql.Tx, gid, ownerID int64, others []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_group_members WHERE group_id = ?`, gid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_group_members (group_id, user_id, position) VALUES (?, ?, 0)`, gid, ownerID); err != nil {
		return err
	}
	for idx, mid := range others {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_group_members (group_id, user_id, position) VALUES (?, ?, ?)`, gid, mid, idx+1); err != nil {
			return err
		}
	}
	return nil
}

type msgRow struct {
	id        int64
	chatKey   string
	authorID  int64
	body      string
	replyTo   *int64
	mentions  []int64
	createdAt *time.Time
}

func (m msgRow) toJSON(files []map[string]any) map[string]any {
	if files == nil {
		files = []map[string]any{}
	}
	mentions := m.mentions
	if mentions == nil {
		mentions = []int64{}
	}
	out := map[string]any{
		"id":         m.id,
		"chat":       m.chatKey,
		"author_id":  m.authorID,
		"body":       m.body,
		"mentions":   mentions,
		"created_at": store.IsoTime(m.createdAt),
		"files":      files,
	}
	if m.replyTo != nil {
		out["reply_to"] = *m.replyTo
	}
	return out
}

type scanner interface{ Scan(dest ...any) error }

func scanMsgRow(sc scanner) (msgRow, error) {
	var m msgRow
	var rt sql.NullInt64
	var mentionsRaw string
	var crRaw any
	if err := sc.Scan(&m.id, &m.chatKey, &m.authorID, &m.body, &rt, &mentionsRaw, &crRaw); err != nil {
		return m, err
	}
	if rt.Valid {
		v := rt.Int64
		m.replyTo = &v
	}
	_ = json.Unmarshal([]byte(mentionsRaw), &m.mentions)
	m.createdAt = dbx.ParseTime(crRaw)
	return m, nil
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

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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
