package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	mux.HandleFunc("POST /actions/pair", a.auth(a.handlePair))
	mux.HandleFunc("GET /chats", a.auth(a.handleListChats))
	mux.HandleFunc("GET /messages", a.auth(a.handleGetMessages))
	mux.HandleFunc("GET /actions/{id}", a.auth(a.handleGetAction))

	mux.HandleFunc("POST /actions/send_audio", a.auth(a.action("send_audio",
		opSpec{needsTarget: true, execR: a.opSendAudio})))
	mux.HandleFunc("POST /actions/send", a.auth(a.action("send",
		opSpec{needsTarget: true, execR: a.opSend})))
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

	// message-level
	mux.HandleFunc("POST /actions/edit_message", a.auth(a.action("edit_message",
		opSpec{needsTarget: true, exec: a.opEditMessage})))
	mux.HandleFunc("POST /actions/react", a.auth(a.action("react",
		opSpec{needsTarget: true, exec: a.opReact})))
	mux.HandleFunc("POST /actions/star", a.auth(a.action("star",
		opSpec{needsTarget: true, destructive: true, exec: a.opStar(true)})))
	mux.HandleFunc("POST /actions/unstar", a.auth(a.action("unstar",
		opSpec{needsTarget: true, destructive: true, exec: a.opStar(false)})))

	// chat-level
	mux.HandleFunc("POST /actions/pin", a.auth(a.action("pin",
		opSpec{needsTarget: true, destructive: true, exec: a.opPin(true)})))
	mux.HandleFunc("POST /actions/unpin", a.auth(a.action("unpin",
		opSpec{needsTarget: true, destructive: true, exec: a.opPin(false)})))
	mux.HandleFunc("POST /actions/typing", a.auth(a.action("typing",
		opSpec{needsTarget: true, exec: a.opTyping(true)})))
	mux.HandleFunc("POST /actions/stop_typing", a.auth(a.action("stop_typing",
		opSpec{needsTarget: true, exec: a.opTyping(false)})))

	// newsletter
	mux.HandleFunc("POST /actions/follow", a.auth(a.action("follow",
		opSpec{needsTarget: true, exec: a.opFollow})))
	mux.HandleFunc("POST /actions/mute_newsletter", a.auth(a.action("mute_newsletter",
		opSpec{needsTarget: true, exec: a.opMuteNewsletter(true)})))
	mux.HandleFunc("POST /actions/unmute_newsletter", a.auth(a.action("unmute_newsletter",
		opSpec{needsTarget: true, exec: a.opMuteNewsletter(false)})))

	// group management
	mux.HandleFunc("POST /actions/group_create", a.auth(a.action("group_create",
		opSpec{needsTarget: false, execR: a.opGroupCreate})))
	mux.HandleFunc("POST /actions/group_leave", a.auth(a.action("group_leave",
		opSpec{needsTarget: true, destructive: true, exec: a.opGroupLeave})))
	mux.HandleFunc("POST /actions/group_add", a.auth(a.action("group_add",
		opSpec{needsTarget: true, exec: a.opGroupParticipants("add")})))
	mux.HandleFunc("POST /actions/group_remove", a.auth(a.action("group_remove",
		opSpec{needsTarget: true, destructive: true, exec: a.opGroupParticipants("remove")})))
	mux.HandleFunc("POST /actions/group_promote", a.auth(a.action("group_promote",
		opSpec{needsTarget: true, exec: a.opGroupParticipants("promote")})))
	mux.HandleFunc("POST /actions/group_demote", a.auth(a.action("group_demote",
		opSpec{needsTarget: true, exec: a.opGroupParticipants("demote")})))
	mux.HandleFunc("POST /actions/group_name", a.auth(a.action("group_name",
		opSpec{needsTarget: true, exec: a.opGroupName})))
	mux.HandleFunc("POST /actions/group_topic", a.auth(a.action("group_topic",
		opSpec{needsTarget: true, exec: a.opGroupTopic})))
	mux.HandleFunc("POST /actions/group_photo", a.auth(a.action("group_photo",
		opSpec{needsTarget: true, exec: a.opGroupPhoto})))
	mux.HandleFunc("POST /actions/group_join", a.auth(a.action("group_join",
		opSpec{needsTarget: false, execR: a.opGroupJoin})))

	// presence (no chat target)
	mux.HandleFunc("POST /actions/presence", a.auth(a.action("presence",
		opSpec{needsTarget: false, exec: a.opPresence})))

	// reads
	mux.HandleFunc("GET /blocklist", a.auth(a.handleBlocklist))
	// group reads
	mux.HandleFunc("GET /groups", a.auth(a.handleJoinedGroups))
	mux.HandleFunc("GET /group", a.auth(a.handleGroupInfo))
	mux.HandleFunc("GET /group/invite", a.auth(a.handleGroupInvite))
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

	// message/chat extras
	Mentions []string `json:"mentions"`
	Media    string   `json:"media"`
	Seconds  uint32   `json:"seconds"`
	Emoji    string   `json:"emoji"`
	FromMe bool   `json:"from_me"`

	// group extras
	Participants []string `json:"participants"`
	Name         string   `json:"name"`
	Topic        string   `json:"topic"`
	Code         string   `json:"code"`
	Reset        bool     `json:"reset"`
	ImagePath    string   `json:"image_path"` // group_photo: path to a JPEG on disk

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

// opFuncR is for ops that return structured data (e.g. send -> msg_id,
// group_create -> new group jid). Takes precedence over exec when set.
type opFuncR func(ctx context.Context, req *ActionRequest, jid types.JID) (map[string]any, error)

type opSpec struct {
	exec        opFunc
	execR       opFuncR
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
		var result map[string]any
		var execErr error
		if spec.execR != nil {
			result, execErr = spec.execR(ctx, &req, jid)
		} else {
			execErr = spec.exec(ctx, &req, jid)
		}
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
		resp := map[string]any{
			"action_id": entry.ID, "action": name, "status": "succeeded",
			"chat_jid": jid.String(), "chat_name": chatName,
		}
		for k, v := range result {
			resp[k] = v
		}
		writeJSON(w, http.StatusOK, resp)
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

// normalizeMentions turns bare numbers ("19995550000") into full JIDs. Values
// that already carry a server (@s.whatsapp.net, @lid) pass through untouched,
// so LID-addressed groups can be tagged by their LID.
func normalizeMentions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(strings.TrimPrefix(m, "@"))
		if m == "" {
			continue
		}
		if !strings.Contains(m, "@") {
			m += "@s.whatsapp.net"
		}
		out = append(out, m)
	}
	return out
}

func (a *API) opSendAudio(ctx context.Context, req *ActionRequest, jid types.JID) (map[string]any, error) {
	if req.Media == "" {
		return nil, errors.New("media (path to audio file) is required")
	}
	id, err := a.wa.SendAudio(ctx, jid, req.Media, req.Seconds)
	if err != nil {
		return nil, err
	}
	return map[string]any{"msg_id": id}, nil
}

func (a *API) opSend(ctx context.Context, req *ActionRequest, jid types.JID) (map[string]any, error) {
	if req.Text == "" {
		return nil, errors.New("text is required")
	}
	id, err := a.wa.SendTextWithMentions(ctx, jid, req.Text, normalizeMentions(req.Mentions))
	if err != nil {
		return nil, err
	}
	return map[string]any{"msg_id": id}, nil
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

// resolveSender returns the sender JID for message ops (defaults to self).
func (a *API) resolveSender(req *ActionRequest, chat types.JID) (types.JID, error) {
	if req.Sender != "" {
		return parseJID(req.Sender)
	}
	if me := a.wa.client.Store.ID; me != nil {
		return me.ToNonAD(), nil
	}
	return chat, nil
}

func (a *API) opEditMessage(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.MsgID == "" || req.Text == "" {
		return errors.New("msg_id and text are required")
	}
	return a.wa.EditMessage(ctx, jid, req.MsgID, req.Text)
}

func (a *API) opReact(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.MsgID == "" {
		return errors.New("msg_id is required (emoji empty removes the reaction)")
	}
	sender, err := a.resolveSender(req, jid)
	if err != nil {
		return err
	}
	return a.wa.React(ctx, jid, sender, req.MsgID, req.Emoji)
}

func (a *API) opStar(star bool) opFunc {
	return func(ctx context.Context, req *ActionRequest, jid types.JID) error {
		if req.MsgID == "" {
			return errors.New("msg_id is required")
		}
		sender, err := a.resolveSender(req, jid)
		if err != nil {
			return err
		}
		return a.wa.StarMessage(ctx, jid, sender, req.MsgID, req.FromMe, star)
	}
}

func (a *API) opPin(pin bool) opFunc {
	return func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.PinChat(ctx, jid, pin)
	}
}

func (a *API) opTyping(typing bool) opFunc {
	return func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.SetTyping(ctx, jid, typing)
	}
}

func (a *API) opFollow(ctx context.Context, _ *ActionRequest, jid types.JID) error {
	return a.wa.FollowNewsletter(ctx, jid)
}

func (a *API) opMuteNewsletter(mute bool) opFunc {
	return func(ctx context.Context, _ *ActionRequest, jid types.JID) error {
		return a.wa.MuteNewsletter(ctx, jid, mute)
	}
}

func (a *API) opGroupCreate(ctx context.Context, req *ActionRequest, _ types.JID) (map[string]any, error) {
	if req.Name == "" || len(req.Participants) == 0 {
		return nil, errors.New("name and participants are required")
	}
	gi, err := a.wa.CreateGroup(ctx, req.Name, req.Participants)
	if err != nil {
		return nil, err
	}
	return map[string]any{"group_jid": gi.JID.String(), "group_name": gi.Name}, nil
}

func (a *API) opGroupLeave(ctx context.Context, _ *ActionRequest, jid types.JID) error {
	return a.wa.LeaveGroup(ctx, jid)
}

func (a *API) opGroupParticipants(action string) opFunc {
	return func(ctx context.Context, req *ActionRequest, jid types.JID) error {
		if len(req.Participants) == 0 {
			return errors.New("participants are required")
		}
		return a.wa.GroupParticipants(ctx, jid, action, req.Participants)
	}
}

func (a *API) opGroupName(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.Name == "" {
		return errors.New("name is required")
	}
	return a.wa.SetGroupName(ctx, jid, req.Name)
}

func (a *API) opGroupTopic(ctx context.Context, req *ActionRequest, jid types.JID) error {
	return a.wa.SetGroupTopic(ctx, jid, req.Topic)
}

func (a *API) opGroupPhoto(ctx context.Context, req *ActionRequest, jid types.JID) error {
	if req.ImagePath == "" {
		return errors.New("image_path is required (path to a JPEG)")
	}
	img, err := os.ReadFile(req.ImagePath)
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}
	_, err = a.wa.SetGroupPhoto(ctx, jid, img)
	return err
}

func (a *API) opGroupJoin(ctx context.Context, req *ActionRequest, _ types.JID) (map[string]any, error) {
	if req.Code == "" {
		return nil, errors.New("code (invite link/code) is required")
	}
	jid, err := a.wa.JoinGroup(ctx, req.Code)
	if err != nil {
		return nil, err
	}
	return map[string]any{"group_jid": jid.String()}, nil
}

func (a *API) opPresence(ctx context.Context, req *ActionRequest, _ types.JID) error {
	// presence uses the "text" field as available|unavailable
	return a.wa.SetOnline(ctx, req.Text != "unavailable")
}

func (a *API) handleBlocklist(w http.ResponseWriter, r *http.Request) {
	bl, err := a.wa.client.GetBlocklist(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(bl.JIDs))
	for _, j := range bl.JIDs {
		out = append(out, map[string]any{"jid": j.String(), "name": a.wa.ResolveName(j.String())})
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": out, "count": len(out)})
}

func (a *API) handleJoinedGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.wa.client.GetJoinedGroups(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"jid": g.JID.String(), "name": g.Name, "participants": len(g.Participants),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "count": len(out)})
}

func (a *API) handleGroupInfo(w http.ResponseWriter, r *http.Request) {
	jid, err := parseJID(r.URL.Query().Get("chat"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	gi, err := a.wa.GroupInfo(r.Context(), jid)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, gi)
}

func (a *API) handleGroupInvite(w http.ResponseWriter, r *http.Request) {
	jid, err := parseJID(r.URL.Query().Get("chat"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	reset := r.URL.Query().Get("reset") == "true"
	link, err := a.wa.GroupInviteLink(r.Context(), jid, reset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": jid.String(), "invite_link": link, "reset": reset})
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

func (a *API) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := strings.TrimPrefix(strings.TrimSpace(body.Phone), "+")
	if phone == "" {
		phone = "19995550000" // James's number; override via body.phone
	}
	code, err := a.wa.PairForCode(r.Context(), phone)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"code":  code,
		"phone": phone,
		"hint":  "Enter the code on the phone: WhatsApp > Linked Devices > Link a Device > Link with phone number instead. Code valid a few minutes.",
	})
}
