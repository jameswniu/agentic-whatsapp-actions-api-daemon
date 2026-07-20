package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WAClient wraps whatsmeow with lifecycle state the HTTP layer can query.
type WAClient struct {
	cfg    *Config
	store  *Store
	client *whatsmeow.Client

	connected        atomic.Bool
	loggedIn         atomic.Bool
	offlineSynced    atomic.Bool
	connectedAt      atomic.Int64 // unix ms
	lastEventAt      atomic.Int64 // unix ms, any event
	historySyncCount atomic.Int64
}

func NewWAClient(cfg *Config, st *Store) (*WAClient, error) {
	dbLog := waLog.Stdout("wmdb", "WARN", false)
	container, err := sqlstore.New(context.Background(),
		"sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL", filepath.Join(cfg.DataDir, "whatsmeow.db")),
		dbLog)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: %w", err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}
	cli := whatsmeow.NewClient(device, waLog.Stdout("wm", "INFO", false))
	w := &WAClient{cfg: cfg, store: st, client: cli}
	cli.AddEventHandler(w.handleEvent)
	return w, nil
}

func (w *WAClient) IsPaired() bool { return w.client.Store.ID != nil }

// PairInteractive runs the QR flow in the foreground. Must be run from a terminal.
func (w *WAClient) PairInteractive(ctx context.Context) error {
	if w.IsPaired() {
		return errors.New("already paired; delete data/whatsmeow.db to re-pair")
	}
	qrChan, err := w.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := w.client.Connect(); err != nil {
		return err
	}
	htmlPath := filepath.Join(w.cfg.DataDir, "qr.html")
	_ = os.WriteFile(htmlPath, []byte(`<!doctype html>
<title>WhatsApp pairing</title>
<body style="display:flex;flex-direction:column;align-items:center;justify-content:center;height:95vh;font-family:sans-serif;background:#fff">
<h2>Scan with WhatsApp &gt; Settings &gt; Linked Devices &gt; Link a Device</h2>
<img id="q" src="qr.png" style="width:min(70vh,90vw);image-rendering:pixelated">
<p id="s">Code refreshes live; scan any time.</p>
<script>
setInterval(function(){
  var i=document.getElementById('q');
  i.src='qr.png?t='+Date.now(); // cache-buster: always the CURRENT code
  fetch('qr.png?h='+Date.now()).catch(function(){
    document.getElementById('s').textContent='Pairing finished or stopped - check terminal.';
  });
},1500);
</script>
</body>`), 0o600)
	pngPath := filepath.Join(w.cfg.DataDir, "qr.png")
	for item := range qrChan {
		switch item.Event {
		case "code":
			fmt.Fprintln(os.Stderr, "\nScan with WhatsApp > Settings > Linked Devices > Link a Device:")
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stderr)
			if code, err := qr.Encode(item.Code, qr.M); err == nil {
				_ = os.WriteFile(pngPath, code.PNG(), 0o600)
				slog.Info("qr_png_written", "path", pngPath)
			}
		case "success":
			_ = os.Remove(pngPath)
			_ = os.Remove(htmlPath)
			return nil
		case "timeout":
			return errors.New("QR timed out; run -pair again")
		default:
			slog.Info("qr_event", "event", item.Event)
		}
	}
	return errors.New("QR channel closed before success")
}

// PairWithCode pairs via WhatsApp's 8-character code flow (no QR scan needed).
// The user enters the code on their phone: Settings > Linked Devices >
// Link a Device > "Link with phone number instead".
func (w *WAClient) PairWithCode(ctx context.Context, phone string) error {
	if w.IsPaired() {
		return errors.New("already paired; delete data/whatsmeow.db to re-pair")
	}
	if err := w.client.Connect(); err != nil {
		return err
	}
	code, err := w.client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (macOS)")
	if err != nil {
		return fmt.Errorf("pair-phone request: %w", err)
	}
	slog.Info("pairing_code_generated", "code", code, "phone", phone)
	fmt.Fprintf(os.Stderr, "\n=== PAIRING CODE: %s ===\n\n", code)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if w.client.IsLoggedIn() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("timed out waiting for code entry on phone (3m)")
}

// Start connects a previously paired session. Auto-reconnect is whatsmeow's default.
func (w *WAClient) Start() error {
	if !w.IsPaired() {
		return errors.New("not paired; run with -pair first")
	}
	return w.client.Connect()
}

func (w *WAClient) Stop() {
	w.client.Disconnect()
}

func (w *WAClient) handleEvent(evt any) {
	w.lastEventAt.Store(time.Now().UnixMilli())
	switch e := evt.(type) {
	case *events.Connected:
		w.connected.Store(true)
		w.loggedIn.Store(true)
		w.offlineSynced.Store(false)
		w.connectedAt.Store(time.Now().UnixMilli())
		slog.Info("wa_connected")
	case *events.OfflineSyncCompleted:
		w.offlineSynced.Store(true)
		slog.Info("wa_offline_sync_completed", "count", e.Count)
	case *events.Disconnected:
		w.connected.Store(false)
		w.offlineSynced.Store(false)
		slog.Warn("wa_disconnected")
	case *events.KeepAliveTimeout:
		slog.Warn("wa_keepalive_timeout", "error_count", e.ErrorCount)
	case *events.LoggedOut:
		slog.Error("wa_logged_out", "reason", fmt.Sprintf("%v", e.Reason),
			"note", "session invalid; disconnecting and awaiting re-pair via POST /actions/pair. No reconnect attempts (anti-abuse).")
		// Stay alive but idle: disconnect so whatsmeow makes no reconnect
		// attempts (avoids tripping WhatsApp anti-abuse), keep serving HTTP so
		// re-pairing can be triggered remotely (Emmanuel relaying the 8-char
		// code over Telegram). whatsmeow deletes the dead session itself on
		// LoggedOut, so IsPaired() turns false and /actions/pair is unblocked.
		w.connected.Store(false)
		w.loggedIn.Store(false)
		go w.client.Disconnect()
	case *events.Message:
		w.ingestMessage(e)
	case *events.HistorySync:
		n := w.ingestHistorySync(e)
		w.historySyncCount.Add(int64(n))
		slog.Info("wa_history_sync_ingested", "messages", n)
	}
}

func (w *WAClient) ingestMessage(e *events.Message) {
	text := extractText(e.Message)
	err := w.store.UpsertMessage(MessageRow{
		ChatJID:   e.Info.Chat.String(),
		MsgID:     string(e.Info.ID),
		SenderJID: e.Info.Sender.String(),
		FromMe:    e.Info.IsFromMe,
		Timestamp: e.Info.Timestamp.UnixMilli(),
		Text:      text,
	})
	if err != nil {
		slog.Error("store_upsert_message_failed", "error", err.Error())
	}
	_ = w.store.TouchChat(e.Info.Chat.String(), e.Info.PushName, e.Info.Timestamp.UnixMilli())
	// Capture media off the event loop so a slow download never stalls ingest.
	go w.captureMedia(e.Info.Chat.String(), string(e.Info.ID), e.Message)
}

// mediaFromMessage returns the downloadable media sub-message plus a file
// extension for it, unwrapping the same container types as extractText. ok=false
// when the message carries no downloadable media.
func mediaFromMessage(m *waE2E.Message) (whatsmeow.DownloadableMessage, string, bool) {
	if m == nil {
		return nil, "", false
	}
	if ds := m.GetDeviceSentMessage(); ds != nil && ds.GetMessage() != nil {
		return mediaFromMessage(ds.GetMessage())
	}
	if em := m.GetEphemeralMessage(); em != nil && em.GetMessage() != nil {
		return mediaFromMessage(em.GetMessage())
	}
	if vo := m.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
		return mediaFromMessage(vo.GetMessage())
	}
	if vo := m.GetViewOnceMessageV2(); vo != nil && vo.GetMessage() != nil {
		return mediaFromMessage(vo.GetMessage())
	}
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage(), extFromMime(m.GetImageMessage().GetMimetype(), ".jpg"), true
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage(), extFromMime(m.GetVideoMessage().GetMimetype(), ".mp4"), true
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage(), extFromMime(m.GetAudioMessage().GetMimetype(), ".ogg"), true
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage(), docExt(m.GetDocumentMessage()), true
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage(), ".webp", true
	}
	return nil, "", false
}

func extFromMime(mime, def string) string {
	mime = strings.ToLower(strings.SplitN(mime, ";", 2)[0])
	switch strings.TrimSpace(mime) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/aac":
		return ".m4a"
	case "audio/amr":
		return ".amr"
	case "application/pdf":
		return ".pdf"
	}
	return def
}

func docExt(dm *waE2E.DocumentMessage) string {
	name := dm.GetFileName()
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		return name[i:]
	}
	return extFromMime(dm.GetMimetype(), ".bin")
}

// captureMedia downloads a message's media (if any) into DATA_DIR/media and
// records the on-disk path on the stored row. Best-effort: any failure (no keys,
// expired link, network) is logged and skipped so ingest never blocks. Files are
// named by message ID and are stable, so re-delivery overwrites rather than dups.
func (w *WAClient) captureMedia(chatJID, msgID string, m *waE2E.Message) {
	dl, ext, ok := mediaFromMessage(m)
	if !ok {
		return
	}
	if len(dl.GetMediaKey()) == 0 || dl.GetDirectPath() == "" {
		return // no keys => not downloadable (e.g. already-expired history media)
	}
	dir := filepath.Join(w.cfg.DataDir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("media_mkdir_failed", "error", err.Error())
		return
	}
	// Skip if already on disk (dedupes re-delivery / history re-sync).
	if p := filepath.Join(dir, sanitizeFilename(msgID)+ext); fileExists(p) {
		_ = w.store.SetMediaPath(chatJID, msgID, p)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := w.client.Download(ctx, dl)
	if err != nil {
		slog.Warn("media_download_failed", "msg_id", msgID, "error", err.Error())
		return
	}
	path := filepath.Join(dir, sanitizeFilename(msgID)+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("media_write_failed", "error", err.Error())
		return
	}
	if err := w.store.SetMediaPath(chatJID, msgID, path); err != nil {
		slog.Error("media_setpath_failed", "error", err.Error())
		return
	}
	slog.Info("media_captured", "msg_id", msgID, "bytes", len(data), "path", path)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}

// recordSentMedia makes outbound media readable: whatsmeow does not echo our own
// sends back as events.Message, so we persist the row and copy the source file
// into the media dir at send time. label matches extractText's format.
func (w *WAClient) recordSentMedia(chat types.JID, msgID, srcPath, label string) {
	dir := filepath.Join(w.cfg.DataDir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("media_mkdir_failed", "error", err.Error())
	}
	dst := filepath.Join(dir, sanitizeFilename(msgID)+filepath.Ext(srcPath))
	if data, err := os.ReadFile(srcPath); err == nil {
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			slog.Error("media_copy_failed", "error", err.Error())
			dst = ""
		}
	} else {
		dst = ""
	}
	ts := time.Now().UnixMilli()
	self := ""
	if id := w.client.Store.ID; id != nil {
		self = id.String()
	}
	if err := w.store.UpsertMessage(MessageRow{
		ChatJID: chat.String(), MsgID: msgID, SenderJID: self,
		FromMe: true, Timestamp: ts, Text: label,
	}); err != nil {
		slog.Error("store_upsert_sent_failed", "error", err.Error())
	}
	if dst != "" {
		_ = w.store.SetMediaPath(chat.String(), msgID, dst)
	}
	_ = w.store.TouchChat(chat.String(), "", ts)
}

func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

func (w *WAClient) ingestHistorySync(e *events.HistorySync) int {
	n := 0
	// Only download media for messages from the last 14 days, capped per sync,
	// so a full history backfill cannot spawn thousands of downloads.
	recentCutoff := time.Now().Add(-14 * 24 * time.Hour).UnixMilli()
	mediaBudget := 200
	for _, conv := range e.Data.GetConversations() {
		jid := conv.GetID()
		name := conv.GetName()
		var lastTS int64
		for _, hmsg := range conv.GetMessages() {
			wmi := hmsg.GetMessage()
			if wmi == nil || wmi.GetKey() == nil {
				continue
			}
			ts := int64(wmi.GetMessageTimestamp()) * 1000
			if ts > lastTS {
				lastTS = ts
			}
			text := ""
			if m := wmi.GetMessage(); m != nil {
				text = extractText(m)
			}
			sender := jid
			if p := wmi.GetParticipant(); p != "" {
				sender = p
			}
			err := w.store.UpsertMessage(MessageRow{
				ChatJID:   jid,
				MsgID:     wmi.GetKey().GetID(),
				SenderJID: sender,
				FromMe:    wmi.GetKey().GetFromMe(),
				Timestamp: ts,
				Text:      text,
			})
			if err == nil {
				n++
			}
			// Capture media for recent history messages too, bounded by a
			// recency window and a per-sync budget so a large backfill never
			// triggers a download storm. The fileExists guard dedupes.
			if m := wmi.GetMessage(); m != nil && ts >= recentCutoff && mediaBudget > 0 {
				if _, _, ok := mediaFromMessage(m); ok {
					mediaBudget--
					go w.captureMedia(jid, wmi.GetKey().GetID(), m)
				}
			}
		}
		_ = w.store.TouchChat(jid, name, lastTS)
	}
	return n
}

// extractText renders any WhatsApp message into a readable one-line string.
// Media types become bracketed labels with whatever metadata is available
// (caption, duration, filename, coordinates) so a text-only reader still knows
// what arrived. Trailing whitespace from empty captions is trimmed.
func extractText(m *waE2E.Message) string {
	return strings.TrimSpace(extractRaw(m))
}

// mediaLabel builds "[kind Ns] caption", omitting the duration when zero and the
// trailing space when there is no caption.
func mediaLabel(kind, caption string, seconds int) string {
	label := "[" + kind
	if seconds > 0 {
		label += fmt.Sprintf(" %ds", seconds)
	}
	label += "]"
	if caption != "" {
		return label + " " + caption
	}
	return label
}

func extractRaw(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	// Unwrap containers: self-chat sends arrive as DeviceSentMessage,
	// disappearing-mode chats wrap in EphemeralMessage, and view-once media
	// nests the real payload one level down.
	if ds := m.GetDeviceSentMessage(); ds != nil && ds.GetMessage() != nil {
		return extractRaw(ds.GetMessage())
	}
	if em := m.GetEphemeralMessage(); em != nil && em.GetMessage() != nil {
		return extractRaw(em.GetMessage())
	}
	if vo := m.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
		return "[view-once] " + extractRaw(vo.GetMessage())
	}
	if vo := m.GetViewOnceMessageV2(); vo != nil && vo.GetMessage() != nil {
		return "[view-once] " + extractRaw(vo.GetMessage())
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if et := m.GetExtendedTextMessage(); et != nil {
		return et.GetText()
	}
	if im := m.GetImageMessage(); im != nil {
		return mediaLabel("image", im.GetCaption(), 0)
	}
	if vm := m.GetVideoMessage(); vm != nil {
		return mediaLabel("video", vm.GetCaption(), int(vm.GetSeconds()))
	}
	if am := m.GetAudioMessage(); am != nil {
		kind := "audio"
		if am.GetPTT() {
			kind = "voice note"
		}
		return mediaLabel(kind, "", int(am.GetSeconds()))
	}
	if dm := m.GetDocumentMessage(); dm != nil {
		name := dm.GetFileName()
		if name == "" {
			name = dm.GetTitle()
		}
		return strings.TrimSpace("[document] " + name)
	}
	if m.GetStickerMessage() != nil {
		return "[sticker]"
	}
	if lm := m.GetLocationMessage(); lm != nil {
		if n := lm.GetName(); n != "" {
			return "[location] " + n
		}
		return fmt.Sprintf("[location] %.5f,%.5f", lm.GetDegreesLatitude(), lm.GetDegreesLongitude())
	}
	if m.GetLiveLocationMessage() != nil {
		return "[live location]"
	}
	if cm := m.GetContactMessage(); cm != nil {
		return strings.TrimSpace("[contact] " + cm.GetDisplayName())
	}
	if cam := m.GetContactsArrayMessage(); cam != nil {
		return fmt.Sprintf("[contacts] %d shared", len(cam.GetContacts()))
	}
	if rm := m.GetReactionMessage(); rm != nil {
		return strings.TrimSpace("[reaction] " + rm.GetText())
	}
	if pm := m.GetPollCreationMessage(); pm != nil {
		return strings.TrimSpace("[poll] " + pm.GetName())
	}
	if gm := m.GetGroupInviteMessage(); gm != nil {
		return strings.TrimSpace("[group invite] " + gm.GetGroupName())
	}
	if pm := m.GetProtocolMessage(); pm != nil {
		if pm.GetType() == waE2E.ProtocolMessage_REVOKE {
			return "[deleted]"
		}
		return ""
	}
	return ""
}

// ---- readiness gates ----

var (
	ErrNotConnected = errors.New("whatsapp connection is down")
	ErrNotSynced    = errors.New("post-reconnect sync incomplete; destructive ops gated")
)

// WaitConnected blocks up to timeout for the socket to be up.
func (w *WAClient) WaitConnected(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if w.client.IsConnected() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ErrNotConnected
}

// DestructiveReady enforces the accuracy gate: after a (re)connect, destructive
// app-state ops wait for OfflineSyncCompleted, or a grace period as fallback.
func (w *WAClient) DestructiveReady() error {
	if !w.client.IsConnected() {
		return ErrNotConnected
	}
	if w.offlineSynced.Load() {
		return nil
	}
	connAt := w.connectedAt.Load()
	if connAt > 0 && time.Since(time.UnixMilli(connAt)) > w.cfg.OfflineSyncGracePeriod {
		return nil
	}
	return ErrNotSynced
}

// ---- op primitives (no gates here; gates live in actions.go) ----

func (w *WAClient) lastMessageKey(chatJID types.JID) (time.Time, *waCommon.MessageKey, *MessageRow) {
	row, err := w.store.LastMessage(chatJID.String())
	if err != nil || row == nil {
		return time.Time{}, nil, nil
	}
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(row.ChatJID),
		FromMe:    proto.Bool(row.FromMe),
		ID:        proto.String(row.MsgID),
	}
	return time.UnixMilli(row.Timestamp), key, row
}

// ResolveName returns a best-effort display name for a chat JID by consulting
// whatsmeow's contact store (synced from the phone's address book + push names),
// resolving @lid privacy IDs to phone numbers first, and group subjects for groups.
// Returns "" when nothing better than the raw ID is known.
func (w *WAClient) ResolveName(jidStr string) string {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return ""
	}
	// Group-participant senders carry a device suffix (e.g. "...:22@lid") that
	// misses both the LID->phone and contact lookups. Normalise to the bare
	// user JID so resolution hits.
	jid = jid.ToNonAD()
	ctx := context.Background()

	switch jid.Server {
	case types.GroupServer:
		if gi, err := w.client.GetGroupInfo(ctx, jid); err == nil && gi != nil && gi.Name != "" {
			return gi.Name
		}
		return ""
	case types.BroadcastServer:
		if jid.User == "status" {
			return "Status Updates"
		}
	case types.NewsletterServer:
		if ni, err := w.client.GetNewsletterInfo(ctx, jid); err == nil && ni != nil && ni.ThreadMeta.Name.Text != "" {
			return ni.ThreadMeta.Name.Text
		}
		return ""
	}

	// Resolve @lid -> phone JID so the contact lookup can hit.
	lookup := jid
	if jid.Server == types.HiddenUserServer {
		if pn, err := w.client.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			lookup = pn
		}
	}
	if info, err := w.client.Store.Contacts.GetContact(ctx, lookup); err == nil && info.Found {
		switch {
		case info.FullName != "":
			return info.FullName
		case info.FirstName != "":
			return info.FirstName
		case info.PushName != "":
			return info.PushName
		case info.BusinessName != "":
			return info.BusinessName
		}
	}
	return ""
}

func (w *WAClient) SendText(ctx context.Context, to types.JID, text string) (string, error) {
	return w.SendTextWithMentions(ctx, to, text, nil)
}

// SendTextWithMentions sends text that can @-tag participants. A real WhatsApp
// mention needs BOTH halves: the literal "@<number>" in the body AND the tagged
// JID in ContextInfo.MentionedJID. Text alone renders as plain characters and
// notifies nobody, which is why plain Conversation messages cannot tag.
func (w *WAClient) SendTextWithMentions(ctx context.Context, to types.JID, text string, mentions []string) (string, error) {
	if len(mentions) == 0 {
		resp, err := w.client.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
		if err != nil {
			return "", err
		}
		return string(resp.ID), nil
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        proto.String(text),
		ContextInfo: &waE2E.ContextInfo{MentionedJID: mentions},
	}}
	resp, err := w.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	return string(resp.ID), nil
}

// SendAudio uploads a local audio file and sends it as a playable audio message.
// WhatsApp's AudioMessage has no caption field, so any accompanying text must be
// sent as its own message.
func (w *WAClient) SendAudio(ctx context.Context, to types.JID, path string, seconds uint32) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read audio: %w", err)
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return "", fmt.Errorf("upload audio: %w", err)
	}
	mime := "audio/mpeg"
	if strings.HasSuffix(strings.ToLower(path), ".ogg") {
		mime = "audio/ogg; codecs=opus"
	}
	msg := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String(mime),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
		Seconds:       proto.Uint32(seconds),
		PTT:           proto.Bool(false),
	}}
	resp, err := w.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	id := string(resp.ID)
	w.recordSentMedia(to, id, path, mediaLabel("audio", "", int(seconds)))
	return id, nil
}

func mimeFromExt(path, def string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".3gp":
		return "video/3gpp"
	case ".pdf":
		return "application/pdf"
	}
	return def
}

// SendImage uploads a local image and sends it with an optional caption.
func (w *WAClient) SendImage(ctx context.Context, to types.JID, path, caption string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return "", fmt.Errorf("upload image: %w", err)
	}
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:       proto.String(caption),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String(mimeFromExt(path, "image/jpeg")),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
	resp, err := w.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	id := string(resp.ID)
	w.recordSentMedia(to, id, path, strings.TrimSpace("[image] "+caption))
	return id, nil
}

// SendVideo uploads a local video and sends it with an optional caption. Duration
// is probed with ffprobe when available; a zero duration still sends fine.
func (w *WAClient) SendVideo(ctx context.Context, to types.JID, path, caption string, seconds uint32) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read video: %w", err)
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaVideo)
	if err != nil {
		return "", fmt.Errorf("upload video: %w", err)
	}
	msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
		Caption:       proto.String(caption),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String(mimeFromExt(path, "video/mp4")),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
		Seconds:       proto.Uint32(seconds),
	}}
	resp, err := w.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	id := string(resp.ID)
	w.recordSentMedia(to, id, path, mediaLabel("video", caption, int(seconds)))
	return id, nil
}

// SendDocument uploads a local file and sends it as a document attachment.
func (w *WAClient) SendDocument(ctx context.Context, to types.JID, path, caption string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read document: %w", err)
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return "", fmt.Errorf("upload document: %w", err)
	}
	name := filepath.Base(path)
	msg := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption:       proto.String(caption),
		FileName:      proto.String(name),
		Title:         proto.String(name),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String(mimeFromExt(path, "application/octet-stream")),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
	resp, err := w.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	id := string(resp.ID)
	w.recordSentMedia(to, id, path, strings.TrimSpace("[document] "+name))
	return id, nil
}

// SelfTestMediaDownload uploads a local file to WhatsApp's media servers and
// immediately pulls it back through the same client.Download() path that inbound
// capture uses, then compares bytes. A matching round-trip proves the download
// path works end to end against live servers without needing a real inbound message.
func (w *WAClient) SelfTestMediaDownload(ctx context.Context, path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	msg := &waE2E.ImageMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String("image/jpeg"),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}
	got, err := w.client.Download(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return map[string]any{
		"uploaded_bytes":   len(data),
		"downloaded_bytes": len(got),
		"bytes_match":      bytes.Equal(data, got),
	}, nil
}

// RequestHistory asks the server for `count` messages immediately before the
// given reference message (an on-demand history sync). The response arrives as
// an events.HistorySync and flows through ingestHistorySync, which re-downloads
// any media it carries. Used to backfill media for older messages.
func (w *WAClient) RequestHistory(ctx context.Context, chat types.JID, refMsgID string, refFromMe bool, refTS time.Time, count int) error {
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: refFromMe},
		ID:            refMsgID,
		Timestamp:     refTS,
	}
	msg := w.client.BuildHistorySyncRequest(info, count)
	_, err := w.client.SendPeerMessage(ctx, msg)
	return err
}

func (w *WAClient) Archive(ctx context.Context, chat types.JID, archive bool) error {
	ts, key, _ := w.lastMessageKey(chat)
	return w.client.SendAppState(ctx, appstate.BuildArchive(chat, archive, ts, key))
}

func (w *WAClient) Mute(ctx context.Context, chat types.JID, mute bool, duration time.Duration) error {
	return w.client.SendAppState(ctx, appstate.BuildMute(chat, mute, duration))
}

func (w *WAClient) DeleteChat(ctx context.Context, chat types.JID, deleteMedia bool) error {
	// Newsletters are unfollowed, not deleted; a DeleteChatAction patch is a no-op for them.
	if chat.Server == types.NewsletterServer {
		if err := w.client.UnfollowNewsletter(ctx, chat); err != nil {
			return fmt.Errorf("unfollow newsletter: %w", err)
		}
		// Verify the unfollow actually took effect (guards against silent no-ops).
		if subs, err := w.client.GetSubscribedNewsletters(ctx); err == nil {
			for _, n := range subs {
				if n != nil && n.ID.String() == chat.String() {
					return fmt.Errorf("unfollow reported ok but newsletter still subscribed: %s", chat.String())
				}
			}
		}
	} else {
		ts, key, _ := w.lastMessageKey(chat)
		if err := w.client.SendAppState(ctx, appstate.BuildDeleteChat(chat, ts, key, deleteMedia)); err != nil {
			return err
		}
	}
	// Prune the local index so readback is immediately consistent with the mutation.
	if err := w.store.RemoveChat(chat.String()); err != nil {
		slog.Warn("delete_chat_local_prune_failed", "chat", chat.String(), "error", err.Error())
	}
	return nil
}

func (w *WAClient) RevokeMessage(ctx context.Context, chat, sender types.JID, msgID string) error {
	_, err := w.client.SendMessage(ctx, chat, w.client.BuildRevoke(chat, sender, types.MessageID(msgID)))
	return err
}

func (w *WAClient) SetBlocked(ctx context.Context, jid types.JID, block bool) error {
	action := events.BlocklistChangeActionBlock
	if !block {
		action = events.BlocklistChangeActionUnblock
	}
	_, err := w.client.UpdateBlocklist(ctx, jid, action)
	return err
}

// ---- message-level ops ----

func (w *WAClient) EditMessage(ctx context.Context, chat types.JID, msgID, newText string) error {
	newContent := &waE2E.Message{Conversation: proto.String(newText)}
	_, err := w.client.SendMessage(ctx, chat, w.client.BuildEdit(chat, types.MessageID(msgID), newContent))
	return err
}

// React sets (or with empty emoji, removes) a reaction on a message.
func (w *WAClient) React(ctx context.Context, chat, sender types.JID, msgID, emoji string) error {
	_, err := w.client.SendMessage(ctx, chat, w.client.BuildReaction(chat, sender, types.MessageID(msgID), emoji))
	return err
}

func (w *WAClient) StarMessage(ctx context.Context, chat, sender types.JID, msgID string, fromMe, starred bool) error {
	return w.client.SendAppState(ctx, appstate.BuildStar(chat, sender, types.MessageID(msgID), fromMe, starred))
}

// ---- chat-level ops ----

func (w *WAClient) PinChat(ctx context.Context, chat types.JID, pin bool) error {
	return w.client.SendAppState(ctx, appstate.BuildPin(chat, pin))
}

// ---- newsletter ops ----

func (w *WAClient) FollowNewsletter(ctx context.Context, jid types.JID) error {
	return w.client.FollowNewsletter(ctx, jid)
}

func (w *WAClient) MuteNewsletter(ctx context.Context, jid types.JID, mute bool) error {
	return w.client.NewsletterToggleMute(ctx, jid, mute)
}

// ---- presence ----

func (w *WAClient) SetTyping(ctx context.Context, chat types.JID, typing bool) error {
	state := types.ChatPresencePaused
	if typing {
		state = types.ChatPresenceComposing
	}
	return w.client.SendChatPresence(ctx, chat, state, types.ChatPresenceMediaText)
}

func (w *WAClient) SetOnline(ctx context.Context, available bool) error {
	state := types.PresenceUnavailable
	if available {
		state = types.PresenceAvailable
	}
	return w.client.SendPresence(ctx, state)
}

// ---- group management ----

func (w *WAClient) parseJIDs(inputs []string) ([]types.JID, error) {
	out := make([]types.JID, 0, len(inputs))
	for _, s := range inputs {
		j, err := parseJID(s)
		if err != nil {
			return nil, fmt.Errorf("bad participant %q: %w", s, err)
		}
		out = append(out, j)
	}
	return out, nil
}

func (w *WAClient) CreateGroup(ctx context.Context, name string, participants []string) (*types.GroupInfo, error) {
	jids, err := w.parseJIDs(participants)
	if err != nil {
		return nil, err
	}
	return w.client.CreateGroup(ctx, whatsmeow.ReqCreateGroup{Name: name, Participants: jids})
}

func (w *WAClient) LeaveGroup(ctx context.Context, jid types.JID) error {
	return w.client.LeaveGroup(ctx, jid)
}

func (w *WAClient) GroupParticipants(ctx context.Context, jid types.JID, action string, participants []string) error {
	jids, err := w.parseJIDs(participants)
	if err != nil {
		return err
	}
	var pc whatsmeow.ParticipantChange
	switch action {
	case "add":
		pc = whatsmeow.ParticipantChangeAdd
	case "remove":
		pc = whatsmeow.ParticipantChangeRemove
	case "promote":
		pc = whatsmeow.ParticipantChangePromote
	case "demote":
		pc = whatsmeow.ParticipantChangeDemote
	default:
		return fmt.Errorf("unknown participant action: %s", action)
	}
	_, err = w.client.UpdateGroupParticipants(ctx, jid, jids, pc)
	return err
}

func (w *WAClient) SetGroupName(ctx context.Context, jid types.JID, name string) error {
	return w.client.SetGroupName(ctx, jid, name)
}

func (w *WAClient) SetGroupTopic(ctx context.Context, jid types.JID, topic string) error {
	return w.client.SetGroupTopic(ctx, jid, "", "", topic)
}

func (w *WAClient) GroupInviteLink(ctx context.Context, jid types.JID, reset bool) (string, error) {
	return w.client.GetGroupInviteLink(ctx, jid, reset)
}

func (w *WAClient) JoinGroup(ctx context.Context, code string) (types.JID, error) {
	return w.client.JoinGroupWithLink(ctx, code)
}

func (w *WAClient) GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	return w.client.GetGroupInfo(ctx, jid)
}

// SetGroupPhoto sets a group's profile photo. avatar must be a JPEG; pass nil to remove.
func (w *WAClient) SetGroupPhoto(ctx context.Context, jid types.JID, avatar []byte) (string, error) {
	return w.client.SetGroupPhoto(ctx, jid, avatar)
}

func (w *WAClient) MarkChatRead(ctx context.Context, chat types.JID, read bool) error {
	ts, key, row := w.lastMessageKey(chat)
	if err := w.client.SendAppState(ctx, appstate.BuildMarkChatAsRead(chat, read, ts, key)); err != nil {
		return err
	}
	// Best-effort read receipt for the latest inbound message; failure is non-fatal.
	if read && row != nil && !row.FromMe {
		sender, err := types.ParseJID(row.SenderJID)
		if err == nil {
			_ = w.client.MarkRead(ctx, []types.MessageID{types.MessageID(row.MsgID)}, time.Now(), chat, sender)
		}
	}
	return nil
}

// PairForCode requests an 8-char pairing code and returns it immediately.
// Login completes asynchronously when the user enters the code on the phone
// (whatsmeow handles the handshake via its event loop). Added for the
// /actions/pair endpoint so Emmanuel can relay the code over Telegram.
func (w *WAClient) PairForCode(ctx context.Context, phone string) (string, error) {
	if w.IsPaired() {
		return "", errors.New("already paired; delete data/whatsmeow.db to re-pair")
	}
	if err := w.client.Connect(); err != nil {
		return "", err
	}
	code, err := w.client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (macOS)")
	if err != nil {
		return "", fmt.Errorf("pair-phone request: %w", err)
	}
	slog.Info("pairing_code_generated", "code", code, "phone", phone)
	return code, nil
}
