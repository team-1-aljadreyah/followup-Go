package attachments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
	"followapi/internal/ws"
)

const (
	MaxFileBytes    = 10 * 1024 * 1024
	MaxFiles        = 30
	MaxVoiceBytes   = 5 * 1024 * 1024
	MaxVoiceSeconds = 300.0
	VoicePeaks      = 48
)

var AllowedExtensions = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true,
	"pdf": true, "doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true,
	"txt": true, "csv": true, "zip": true,
}

var VoiceExtensions = map[string]bool{
	"webm": true, "weba": true, "ogg": true, "oga": true, "opus": true,
	"mp4": true, "m4a": true, "aac": true, "mp3": true,
}

var CanonicalMimes = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp",
	"pdf": "application/pdf", "doc": "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":  "application/vnd.ms-powerpoint",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"txt":  "text/plain", "csv": "text/csv", "zip": "application/zip",
}

var VoiceMimes = map[string]string{
	"webm": "audio/webm", "ogg": "audio/ogg", "mp4": "audio/mp4", "mp3": "audio/mpeg", "aac": "audio/aac",
}

type ChatMemberResolver func(ctx context.Context, chatKey string) ([]int64, bool)

type Service struct {
	db          *dbx.DB
	cfg         config.Config
	advanced    *advanced.Service
	chatMembers ChatMemberResolver
	ws          *ws.Hub
}

func New(db *dbx.DB, cfg config.Config, adv *advanced.Service, resolver ChatMemberResolver) *Service {
	return &Service{db: db, cfg: cfg, advanced: adv, chatMembers: resolver}
}

func (s *Service) SetChatMemberResolver(fn ChatMemberResolver) {
	s.chatMembers = fn
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func (s *Service) emitChatAttachmentUpdate(ctx context.Context, ownerType string, ownerID int64) {
	if s.ws == nil || ownerType != "message" || s.chatMembers == nil {
		return
	}
	var chatKey string
	if s.db.QueryRowContext(ctx, `SELECT chat_key FROM messages WHERE id = ?`, ownerID).Scan(&chatKey) == nil && chatKey != "" {
		if members, ok := s.chatMembers(ctx, chatKey); ok {
			s.ws.NotifyChatMessage(members, chatKey, ownerID)
		}
	}
}

type Record struct {
	ID         int64
	OwnerType  string
	OwnerID    int64
	Name       string
	Size       int64
	Mime       string
	Voice      map[string]any
	DiskPath   string
	UploadedBy int64
	At         *time.Time
}

func (s *Service) SignedDownloadURL(attachmentID, userID int64) string {
	now := time.Now().UTC().Add(60 * time.Minute)
	expires := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
	expUnix := expires.Unix()
	sig := s.signDownload(attachmentID, userID, expUnix)
	return fmt.Sprintf("%s/api/v1/attachments/%d/download?expires=%d&uid=%d&signature=%s",
		strings.TrimRight(s.cfg.AppURL, "/"), attachmentID, expUnix, userID, sig)
}

func (s *Service) signDownload(attachmentID, userID, expires int64) string {
	key := s.cfg.AppKey
	if key == "" {
		key = "followapi-fallback-signing-key"
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = fmt.Fprintf(mac, "%d:%d:%d", attachmentID, userID, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) ToJSON(rec Record, viewerID int64) map[string]any {
	m := map[string]any{
		"id":          rec.ID,
		"owner_type":  rec.OwnerType,
		"owner_id":    rec.OwnerID,
		"name":        rec.Name,
		"size":        rec.Size,
		"mime":        rec.Mime,
		"url":         s.SignedDownloadURL(rec.ID, viewerID),
		"uploaded_by": rec.UploadedBy,
		"at":          store.IsoTime(rec.At),
	}
	if len(rec.Voice) > 0 {
		m["voice"] = rec.Voice
	}
	return m
}

func (s *Service) ListForMessages(ctx context.Context, msgIDs []int64, viewerID int64) (map[int64][]map[string]any, error) {
	out := map[int64][]map[string]any{}
	if len(msgIDs) == 0 {
		return out, nil
	}
	ph, args := dbx.InInt64(msgIDs)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, owner_type, owner_id, name, size, mime, voice, disk_path, uploaded_by, at
		FROM attachments
		WHERE owner_type = 'message' AND owner_id IN (%s)
		ORDER BY id ASC
	`, ph), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err == nil {
			out[rec.OwnerID] = append(out[rec.OwnerID], s.ToJSON(rec, viewerID))
		}
	}
	return out, rows.Err()
}

func (s *Service) HandleList(ownerType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := httpx.UserFrom(r.Context())
		id, err := httpx.URLInt64(r, "id")
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		if _, _, err := s.authorizeAccess(r.Context(), u, ownerType, id); err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT id, owner_type, owner_id, name, size, mime, voice, disk_path, uploaded_by, at
			FROM attachments WHERE owner_type = ? AND owner_id = ? ORDER BY id ASC
		`, ownerType, id)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			if rec, err := scanRecord(rows); err == nil {
				out = append(out, s.ToJSON(rec, u.ID))
			}
		}
		httpx.Success(w, out)
	}
}

func (s *Service) HandleUpload(ownerType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := httpx.UserFrom(r.Context())
		id, err := httpx.URLInt64(r, "id")
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		canAdd, _, err := s.authorizeAccess(r.Context(), u, ownerType, id)
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		if !canAdd {
			httpx.HandleErr(w, r, httpx.ErrForbidden("api.attachments.upload_forbidden"))
			return
		}

		if err := r.ParseMultipartForm(MaxFileBytes + (1 << 20)); err != nil {
			httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.attachments.file_required"))
			return
		}

		rawVoice := strings.TrimSpace(r.FormValue("voice"))
		file, hdr, err := r.FormFile("file")
		if err != nil {
			httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.attachments.file_required"))
			return
		}
		defer file.Close()

		var rec Record
		if rawVoice != "" {
			rec, err = s.uploadVoice(r.Context(), u, ownerType, id, file, hdr, rawVoice)
		} else {
			rec, err = s.uploadRegular(r.Context(), u, ownerType, id, file, hdr)
		}
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		s.emitChatAttachmentUpdate(r.Context(), ownerType, id)
		httpx.Success(w, s.ToJSON(rec, u.ID), http.StatusCreated)
	}
}

func (s *Service) HandleDelete(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var ownerType, name, diskPath string
	var ownerID, uploadedBy int64
	err = s.db.QueryRowContext(r.Context(), `SELECT owner_type, owner_id, name, disk_path, uploaded_by FROM attachments WHERE id = ?`, id).
		Scan(&ownerType, &ownerID, &name, &diskPath, &uploadedBy)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.attachments.file_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	_, canRemoveAny, err := s.authorizeAccess(r.Context(), u, ownerType, ownerID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if uploadedBy != u.ID && !canRemoveAny {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.attachments.delete_forbidden"))
		return
	}

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM attachments WHERE id = ?`, id); err != nil {
			return err
		}
		s.logHistoryTx(r.Context(), tx, ownerType, ownerID, u.ID, "attachment_removed", name)
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if diskPath != "" {
		_ = os.Remove(filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(diskPath)))
	}
	s.emitChatAttachmentUpdate(r.Context(), ownerType, ownerID)
	httpx.Success(w, nil)
}

func (s *Service) HandleDownload(w http.ResponseWriter, r *http.Request) {
	attachmentID, err := httpx.URLInt64(r, "attachment")
	if err != nil {
		httpx.HandleErr(w, r, httpx.ErrForbidden(""))
		return
	}
	expStr := r.URL.Query().Get("expires")
	uidStr := r.URL.Query().Get("uid")
	sig := r.URL.Query().Get("signature")
	expires, err1 := strconv.ParseInt(expStr, 10, 64)
	uid, err2 := strconv.ParseInt(uidStr, 10, 64)
	expectedSig := s.signDownload(attachmentID, uid, expires)
	if err1 != nil || err2 != nil || time.Now().UTC().Unix() > expires || !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		httpx.HandleErr(w, r, httpx.ErrForbidden(""))
		return
	}

	user, err := store.FindUserByID(r.Context(), s.db, s.cfg.AppURL, uid)
	if err != nil || user == nil || !user.Active {
		httpx.HandleErr(w, r, httpx.ErrUnauth())
		return
	}

	row := s.db.QueryRowContext(r.Context(), `
		SELECT id, owner_type, owner_id, name, size, mime, voice, disk_path, uploaded_by, at
		FROM attachments WHERE id = ?
	`, attachmentID)
	rec, err := scanRecord(row)
	if err != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.attachments.file_not_found"))
		return
	}
	if _, _, err := s.authorizeAccess(r.Context(), user, rec.OwnerType, rec.OwnerID); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	fullPath := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(rec.DiskPath))
	if _, statErr := os.Stat(fullPath); statErr != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.attachments.file_not_found"))
		return
	}

	dispType := "attachment"
	if len(rec.Voice) > 0 || strings.HasPrefix(rec.Mime, "audio/") {
		dispType = "inline"
	}
	w.Header().Set("Content-Type", rec.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, dispType, sanitizeFilename(rec.Name)))
	http.ServeFile(w, r, fullPath)
}

func (s *Service) uploadRegular(ctx context.Context, u *store.User, ownerType string, ownerID int64, file multipart.File, hdr *multipart.FileHeader) (Record, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(hdr.Filename), "."))
	if ownerType == "message" && VoiceExtensions[ext] {
		return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_missing_meta")
	}
	if ext == "" || !AllowedExtensions[ext] {
		return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return Record{}, httpx.ErrValidation("file", "api.attachments.file_required")
	}
	if len(raw) == 0 {
		return Record{}, httpx.ErrValidation("file", "api.attachments.empty_file")
	}
	if len(raw) > MaxFileBytes {
		return Record{}, httpx.ErrValidation("file", "api.attachments.too_large")
	}

	headLen := 512
	if len(raw) < headLen {
		headLen = len(raw)
	}
	sniff := strings.ToLower(http.DetectContentType(raw[:headLen]))
	if strings.Contains(sniff, "text/html") || strings.Contains(sniff, "svg") || strings.Contains(sniff, "x-dosexec") {
		return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
	}
	if store.UserHasRole(ext, []string{"png", "jpg", "jpeg", "gif", "webp", "txt", "csv"}) {
		headStr := strings.ToLower(string(raw[:headLen]))
		if strings.Contains(headStr, "<svg") || strings.Contains(headStr, "<!doctype html") || strings.Contains(headStr, "<html") || strings.Contains(headStr, "<script") {
			return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
		}
	}
	if ext == "png" && !bytes.HasPrefix(raw, []byte{0x89, 0x50, 0x4E, 0x47}) {
		return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
	}
	if (ext == "jpg" || ext == "jpeg") && !bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}) {
		return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
	}
	if ext == "pdf" && !bytes.HasPrefix(raw, []byte("%PDF")) {
		return Record{}, httpx.ErrValidation("file", "api.attachments.type_not_allowed")
	}

	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE owner_type = ? AND owner_id = ?`, ownerType, ownerID).Scan(&count)
	if count >= MaxFiles {
		return Record{}, httpx.ErrValidation("file", "api.attachments.max_files", map[string]any{"max": MaxFiles})
	}

	diskRel := fmt.Sprintf("attachments/%s/%d/%s.%s", ownerType, ownerID, uuid.NewString(), ext)
	fullPath := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(diskRel))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return Record{}, err
	}
	if err := os.WriteFile(fullPath, raw, 0o644); err != nil {
		return Record{}, err
	}

	displayName := support.TruncateRunes(filepath.Base(hdr.Filename), 200)
	mime := CanonicalMimes[ext]
	if mime == "" {
		mime = "application/octet-stream"
	}
	now := time.Now().UTC()
	nowStr := dbx.FormatDBTime(now)
	var id int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO attachments (owner_type, owner_id, name, size, mime, disk_path, uploaded_by, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, ownerType, ownerID, displayName, int64(len(raw)), mime, diskRel, u.ID, nowStr)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		s.logHistoryTx(ctx, tx, ownerType, ownerID, u.ID, "attachment_added", displayName)
		return nil
	})
	if err != nil {
		_ = os.Remove(fullPath)
		return Record{}, err
	}
	return Record{ID: id, OwnerType: ownerType, OwnerID: ownerID, Name: displayName, Size: int64(len(raw)), Mime: mime, DiskPath: diskRel, UploadedBy: u.ID, At: &now}, nil
}

func (s *Service) uploadVoice(ctx context.Context, u *store.User, ownerType string, ownerID int64, file multipart.File, hdr *multipart.FileHeader, rawVoice string) (Record, error) {
	if ownerType != "message" {
		return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_message_only")
	}
	if err := s.advanced.RequireFeature(ctx, u, "voice_messages"); err != nil {
		return Record{}, err
	}
	var voice map[string]any
	if err := json.Unmarshal([]byte(rawVoice), &voice); err != nil || voice == nil {
		return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_peaks")
	}
	var dup int
	_ = s.db.QueryRowContext(ctx, `SELECT 1 FROM attachments WHERE owner_type = 'message' AND owner_id = ? AND voice IS NOT NULL LIMIT 1`, ownerID).Scan(&dup)
	if dup == 1 {
		return Record{}, httpx.ErrValidation("file", "api.attachments.voice_duplicate")
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(hdr.Filename), "."))
	if ext == "" || !VoiceExtensions[ext] {
		return Record{}, httpx.ErrValidation("file", "api.attachments.voice_type_not_allowed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxVoiceBytes+1))
	if err != nil || len(raw) == 0 {
		return Record{}, httpx.ErrValidation("file", "api.attachments.voice_empty")
	}
	if len(raw) > MaxVoiceBytes {
		return Record{}, httpx.ErrValidation("file", "api.attachments.voice_too_large")
	}
	dur, okDur := toFloat64(voice["duration_s"])
	if !okDur || math.IsNaN(dur) || math.IsInf(dur, 0) || dur <= 0 || dur > MaxVoiceSeconds {
		return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_duration")
	}
	peaksRaw, okPk := voice["peaks"].([]any)
	if !okPk || len(peaksRaw) != VoicePeaks {
		return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_peaks")
	}
	peaks := make([]int, VoicePeaks)
	for i, pv := range peaksRaw {
		pf, ok := toFloat64(pv)
		if !ok || float64(int(pf)) != pf || int(pf) < 0 || int(pf) > 100 {
			return Record{}, httpx.ErrValidation("voice", "api.attachments.voice_peaks")
		}
		peaks[i] = int(pf)
	}
	kind := sniffAudio(raw)
	if kind == "" {
		return Record{}, httpx.ErrValidation("file", "api.attachments.voice_type_not_allowed")
	}
	if kind == "mp4" {
		sample := raw
		if len(sample) > 65536 {
			sample = sample[:65536]
		}
		if bytes.Contains(sample, []byte("vide")) || bytes.Contains(sample, []byte("avc1")) || bytes.Contains(sample, []byte("hvc1")) {
			return Record{}, httpx.ErrValidation("file", "api.attachments.voice_type_not_allowed")
		}
	}
	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE owner_type = 'message' AND owner_id = ?`, ownerID).Scan(&count)
	if count >= MaxFiles {
		return Record{}, httpx.ErrValidation("file", "api.attachments.max_files", map[string]any{"max": MaxFiles})
	}

	diskRel := fmt.Sprintf("attachments/message/%d/%s.%s", ownerID, uuid.NewString(), ext)
	fullPath := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(diskRel))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return Record{}, err
	}
	if err := os.WriteFile(fullPath, raw, 0o644); err != nil {
		return Record{}, err
	}

	displayName := support.TruncateRunes(filepath.Base(hdr.Filename), 200)
	mime := VoiceMimes[kind]
	meta := map[string]any{
		"duration_s": math.Round(dur*10) / 10,
		"peaks":      peaks,
	}
	metaJSON, _ := json.Marshal(meta)
	now := time.Now().UTC()
	nowStr := dbx.FormatDBTime(now)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO attachments (owner_type, owner_id, name, size, mime, voice, disk_path, uploaded_by, at)
		VALUES ('message', ?, ?, ?, ?, ?, ?, ?, ?)
	`, ownerID, displayName, int64(len(raw)), mime, string(metaJSON), diskRel, u.ID, nowStr)
	if err != nil {
		_ = os.Remove(fullPath)
		return Record{}, err
	}
	id, _ := res.LastInsertId()
	return Record{ID: id, OwnerType: ownerType, OwnerID: ownerID, Name: displayName, Size: int64(len(raw)), Mime: mime, Voice: meta, DiskPath: diskRel, UploadedBy: u.ID, At: &now}, nil
}

func sniffAudio(head []byte) string {
	if len(head) < 4 {
		return ""
	}
	if bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "webm"
	}
	if bytes.HasPrefix(head, []byte("OggS")) {
		return "ogg"
	}
	if len(head) >= 8 && string(head[4:8]) == "ftyp" {
		return "mp4"
	}
	if bytes.HasPrefix(head, []byte("ID3")) {
		return "mp3"
	}
	if head[0] == 0xff && (head[1]&0xe0) == 0xe0 {
		if (head[1] & 0x06) == 0 {
			return "aac"
		}
		return "mp3"
	}
	return ""
}

func (s *Service) authorizeAccess(ctx context.Context, u *store.User, ownerType string, ownerID int64) (canAdd bool, canRemoveAny bool, err error) {
	if ownerType == "message" {
		var chatKey string
		var authorID int64
		qerr := s.db.QueryRowContext(ctx, `SELECT chat_key, author_id FROM messages WHERE id = ?`, ownerID).Scan(&chatKey, &authorID)
		if qerr != nil {
			return false, false, httpx.ErrNotFound("api.attachments.not_found")
		}
		if s.chatMembers == nil {
			return false, false, httpx.ErrNotFound("api.attachments.not_found")
		}
		members, ok := s.chatMembers(ctx, chatKey)
		if !ok || !containsInt64(members, u.ID) {
			return false, false, httpx.ErrNotFound("api.attachments.not_found")
		}
		return authorID == u.ID, false, nil
	}

	var projID int64
	switch ownerType {
	case "project":
		projID = ownerID
	case "task":
		_ = s.db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, ownerID).Scan(&projID)
	case "bug":
		_ = s.db.QueryRowContext(ctx, `SELECT project_id FROM bugs WHERE id = ?`, ownerID).Scan(&projID)
	}
	if projID <= 0 {
		return false, false, httpx.ErrNotFound("api.attachments.not_found")
	}
	p, perr := access.LoadProject(ctx, s.db, projID)
	if perr != nil || p == nil || !access.CanSeeProject(u, p) {
		return false, false, httpx.ErrNotFound("api.attachments.not_found")
	}
	return access.CanTakePart(u, p), access.CanManageProjectSetup(u, p), nil
}

func (s *Service) logHistoryTx(ctx context.Context, tx *sql.Tx, ownerType string, ownerID, actorID int64, kind, name string) {
	if ownerType == "message" {
		return
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var from, to *string
	if kind == "attachment_removed" {
		from = &name
	} else {
		to = &name
	}
	switch ownerType {
	case "task":
		_, _ = tx.ExecContext(ctx, `INSERT INTO task_events (task_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, ?, ?, ?, ?)`, ownerID, actorID, kind, from, to, nowStr)
	case "bug":
		_, _ = tx.ExecContext(ctx, `INSERT INTO bug_events (bug_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, ?, ?, ?, ?)`, ownerID, actorID, kind, from, to, nowStr)
	case "project":
		_, _ = tx.ExecContext(ctx, `INSERT INTO project_events (project_id, actor_id, kind, `+"`from`"+`, `+"`to`"+`, at) VALUES (?, ?, ?, ?, ?, ?)`, ownerID, actorID, kind, from, to, nowStr)
	}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(sc scanner) (Record, error) {
	var r Record
	var voiceRaw sql.NullString
	var atRaw any
	err := sc.Scan(&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Size, &r.Mime, &voiceRaw, &r.DiskPath, &r.UploadedBy, &atRaw)
	if err != nil {
		return r, err
	}
	r.At = dbx.ParseTime(atRaw)
	if voiceRaw.Valid && strings.TrimSpace(voiceRaw.String) != "" {
		_ = json.Unmarshal([]byte(voiceRaw.String), &r.Voice)
	}
	return r, nil
}

func containsInt64(arr []int64, target int64) bool {
	for _, v := range arr {
		if v == target {
			return true
		}
	}
	return false
}

func sanitizeFilename(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, `"`, ""), "\n", "")
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}
