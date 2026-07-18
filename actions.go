package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type API struct {
	cfg   *Config
	store *Store
	wa    *WAClient
	start time.Time
}

func (a *API) Routes() http.Handler {
	if a.start.IsZero() {
		a.start = time.Now()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /chats", a.auth(a.handleListChats))
	mux.HandleFunc("GET /messages", a.auth(a.handleGetMessages))
	mux.HandleFunc("GET /actions/{id}", a.auth(a.handleGetAction))

	mux.HandleFunc("POST /actions/send", a.auth(a.action("send",
		opSpec{needsTarget: true, exec: a.opSend})))
	mux.HandleFunc("POST /actions/archive", a.auth(a.action("archive", a.opArchive(true))))
	mux.HandleFunc("POST /actions/unarchive", a.auth(a.action("unarchive", a.opArchive(false))))
	mux.HandleFunc("POST /actions/mute", a.auth(a.action("mute", a.opMute(true))))
	mux.HandleFunc("POST /actions/unmute", a.auth(a.action("unmute", a.opMute(false))))
	mux.HandleFunc("POST /actions/delete_chat", a.auth(a.action("delete_chat",
		opSpec{needsTarget: true, destructive: true, exec: a.opDeleteChat})))
	mux.HandleFunc("POST /actions/delete_message", a.auth(a.action("delete_message",
		opSpec{needsTarget: true, destructive: true, exec: a.opDeleteMessage})))
	mux.HandleFunc("POST /actions/block", a.auth(a.action("block", a.opBlock(true))))
	mux.HandleFunc("POST /actions/unblock", a.auth(a.action("unblock", a.opBlock(false))))
	mux.HandleFunc("POST /actions/mark_read", a.auth(a.action("mark_read", a.opMarkRead(true))))
	mux.HandleFunc("POST /actions/mark_unread", a.auth(a.action("mark_unread", a.opMarkRead(false))))
	return mux
}

// ---- middleware ----

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Token") != a.cfg.APIToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// ---- request/response plumbing ----

type ActionRequest struct {
	// Target: chat JID or bare phone number. "chat", "to" and "jid" are aliases.
	Chat string `json:"chat"`
	To   string `json:"to"`
	JID  string `json:"jid"`

	Text        string `json:"text"`
	MsgID       string `json:"msg_id"`
	Sender      string `json:"sender"`
	DurationMS  int64  `json:"duration_ms"`
	DeleteMedia bool   `json:"delete_media"`

	DryRun         *bool  `json:"dry_run"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (r *ActionRequest) target() string {
	for _, v := range []string{r.Chat, r.To, r.JID} {
		if v != "" {
			return v
		}
	}
	return ""
}

type opFunc func(ctx context.Context, req *ActionRequest, jid types.JID) error

type opSpec struct {
	exec        opFunc
	destructive bool // requires post-reconnect sync gate + lastMessage context
	needsTarget bool
}

func parseJID(input string) (types.JID, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return types.EmptyJID, errors.New("empty target")
	}
	if strings.ContainsRune(input, '@') {
		return types.ParseJID(input)
	}
	num := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, input)
	if num == "" {
		return types.EmptyJID, fmt.Errorf("cannot parse target %q", input)
	}
	return types.NewJID(num, types.DefaultUserServer), nil
}

// action wraps an op with: auth-independent parsing, gates, dry-run preflight,
// journal + idempotency, connection wait, and structured response.
func (a *API) action(name string, spec opSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}

		var jid types.JID
		var err error
		if spec.needsTarget {
			jid, err = parseJID(req.target())
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}

		effectiveDryRun := a.cfg.DryRunDefault
		if req.DryRun != nil {
			effectiveDryRun = *req.DryRun
		}

		gates := map[string]bool{
			"actions_enabled":        a.cfg.ActionsEnabled,
			"live_mutations_enabled": a.cfg.LiveMutationsEnabled,
			"dry_run":                effectiveDryRun,
		}
		liveAllowed := a.cfg.ActionsEnabled && a.cfg.LiveMutationsEnabled && !effectiveDryRun

		// Preflight context: what would this hit?
		chatName := ""
		var lastMsg *MessageRow
		if spec.needsTarget {
			chatName = a.store.ChatName(jid.String())
			lastMsg, _ = a.store.LastMessage(jid.String())
		}

		if effectiveDryRun {
			entry, replayed, jerr := a.store.BeginAction(name, jid.String(), req.IdempotencyKey, req, true)
			if jerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": jerr.Error()})
				return
			}
			if !replayed {
				_ = a.store.FinishAction(entry.ID, "succeeded", "")
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"action_id":    entry.ID,
				"action":       name,
				"dry_run":      true,
				"replayed":     replayed,
				"chat_jid":     jid.String(),
				"chat_name":    chatName,
				"last_message": previewMsg(lastMsg),
				"would_execute_live": liveAllowed ||
					(a.cfg.ActionsEnabled && a.cfg.LiveMutationsEnabled), // true once dry_run=false is passed
				"gates": gates,
			})
			return
		}

		if !liveAllowed {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "live execution blocked by gates",
				"gates": gates,
			})
			return
		}

		// Connection + accuracy gates.
		if err := a.wa.WaitConnected(a.cfg.ConnectWaitTimeout); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if spec.destructive {
			if err := a.wa.DestructiveReady(); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
		}

		entry, replayed, jerr := a.store.BeginAction(name, jid.String(), req.IdempotencyKey, req, false)
		if jerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": jerr.Error()})
			return
		}
		if replayed {
			writeJSON(w, http.StatusOK, map[string]any{
				"action_id": entry.ID, "action": entry.Action, "status": entry.Status,
				"replayed": true,
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		execErr := spec.exec(ctx, &req, jid)
		if execErr != nil {
			_ = a.store.FinishAction(entry.ID, "failed", execErr.Error())
			slog.Error("action_failed", "action", name, "chat", jid.String(), "error", execErr.Error())
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"action_id": entry.ID, "action": name, "status": "failed", "error": execErr.Error(),
			})
			return
		}
		_ = a.store.FinishAction(entry.ID, "succeeded", "")
		slog.Info("action_succeeded", "action", name, "chat", jid.String(), "action_id", entry.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"action_id": entry.ID, "action": name, "status": "succeeded",
			"chat_jid": jid.String(), "chat_name": chatName,
		})
	}
}

func previewMsg(m *MessageRow) map[string]any {
	if m == nil {
		return nil
	}
	text := m.Text
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return map[string]any{
		"text": text, "timestamp": m.Timestamp, "from_me": m.FromMe, "msg_id": m.MsgID,
	}
}

// ---- ops ----

func (a *API) opSend(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.Text == "" {
		return errors.New("text is required")
	}
	_, err := a.wa.SendText(ctx, jid, req.Text)
	return err
}

func (a *API) opArchive(archive bool) opSpec {
	return opSpec{needsTarget: true, destructive: true, exec: func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.Archive(ctx, jid, archive)
	}}
}

func (a *API) opMute(mute bool) opSpec {
	return opSpec{needsTarget: true, destructive: false, exec: func(ctx context.Context, req *ActionRequest, jid types.JID) error {
		return a.wa.Mute(ctx, jid, mute, time.Duration(req.DurationMS)*time.Millisecond)
	}}
}

func (a *API) opDeleteChat(ctx context.Context, req *ActionRequest, jid types.JID) error {
	return a.wa.DeleteChat(ctx, jid, req.DeleteMedia)
}

func (a *API) opDeleteMessage(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.MsgID == "" {
		return errors.New("msg_id is required")
	}
	sender := jid
	if req.Sender != "" {
		s, err := parseJID(req.Sender)
		if err != nil {
			return fmt.Errorf("bad sender: %w", err)
		}
		sender = s
	} else if me := a.wa.client.Store.ID; me != nil {
		sender = me.ToNonAD()
	}
	return a.wa.RevokeMessage(ctx, jid, sender, req.MsgID)
}

func (a *API) opBlock(block bool) opSpec {
	return opSpec{needsTarget: true, destructive: false, exec: func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.SetBlocked(ctx, jid, block)
	}}
}

func (a *API) opMarkRead(read bool) opSpec {
	return opSpec{needsTarget: true, destructive: true, exec: func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.MarkChatRead(ctx, jid, read)
	}}
}

// ---- reads ----

func (a *API) handleListChats(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 500)
	chats, err := a.store.ListChats(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(chats))
	for _, c := range chats {
		name := c.Name
		resolved := a.wa.ResolveName(c.JID)
		if resolved != "" {
			name = resolved
		}
		out = append(out, map[string]any{
			"jid":     c.JID,
			"name":    name,
			"display": displayLabel(c.JID, name),
			"last_ts": c.LastTS,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": out, "count": len(out)})
}

// displayLabel produces a human label: the resolved name if known, else a
// readable phone number for user JIDs, else the raw JID.
func displayLabel(jidStr, name string) string {
	if name != "" {
		return name
	}
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr
	}
	if jid.Server == types.DefaultUserServer && jid.User != "" {
		return "+" + jid.User
	}
	return jidStr
}

func (a *API) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	chat := r.URL.Query().Get("chat")
	if chat == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chat query param required"})
		return
	}
	jid, err := parseJID(chat)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit := queryInt(r, "limit", 50, 500)
	msgs, err := a.store.GetMessages(jid.String(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": jid.String(), "messages": msgs, "count": len(msgs)})
}

func (a *API) handleGetAction(w http.ResponseWriter, r *http.Request) {
	entry, err := a.store.GetAction(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if entry == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// ---- health ----

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	msgs, chats := a.store.CountMessages()
	lastEvt := a.wa.lastEventAt.Load()
	var lastEventAgoMS int64 = -1
	if lastEvt > 0 {
		lastEventAgoMS = time.Now().UnixMilli() - lastEvt
	}
	connected := a.wa.client.IsConnected()
	body := map[string]any{
		"service":            "whatsapp-actiond-go",
		"provider":           "whatsmeow",
		"paired":             a.wa.IsPaired(),
		"connected":          connected,
		"logged_in":          a.wa.client.IsLoggedIn(),
		"offline_synced":     a.wa.offlineSynced.Load(),
		"last_event_ago_ms":  lastEventAgoMS,
		"history_sync_count": a.wa.historySyncCount.Load(),
		"store":              map[string]int64{"messages": msgs, "chats": chats},
		"uptime_s":           int64(time.Since(a.start).Seconds()),
		"gates": map[string]bool{
			"actions_enabled":        a.cfg.ActionsEnabled,
			"dry_run_default":        a.cfg.DryRunDefault,
			"live_mutations_enabled": a.cfg.LiveMutationsEnabled,
		},
	}
	status := http.StatusOK
	if !connected {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, body)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func queryInt(r *http.Request, key string, def, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
