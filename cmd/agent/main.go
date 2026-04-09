package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	_ "modernc.org/sqlite"
)

// ── Sheet column headers ───────────────────────────────────────────────────────
var sheetHeaders = []interface{}{
	"SN", "Message ID", "Thread ID",
	"Group Name", "Group JID",
	"Sender Name", "Sender Phone",
	"Message Type", "Message", "Quoted Message",
	"Date", "Time",
	"Is Reply", "Replied To Message ID", "Replied To Sender",
	"Reply Status",
}

// ── Message record struct ──────────────────────────────────────────────────────
type Record struct {
	MessageID          string
	ThreadID           string
	GroupName          string
	GroupJID           string
	SenderName         string
	SenderPhone        string
	MessageType        string
	Message            string
	QuotedMessage      string
	Date               string
	Time               string
	IsReply            bool
	RepliedToMessageID string
	RepliedToSender    string
}

// ── Agent ──────────────────────────────────────────────────────────────────────
type Agent struct {
	client     *whatsmeow.Client
	groupCache sync.Map // JID string -> group name
	seenIDs    sync.Map // message ID -> bool (dedup)
	sheetsvc   *sheets.Service
	sheetID    string
	sheetName  string
	queue      chan Record
	log        waLog.Logger
}

func newAgent(client *whatsmeow.Client, svc *sheets.Service, sheetID, sheetName string, log waLog.Logger) *Agent {
	return &Agent{
		client:    client,
		sheetsvc:  svc,
		sheetID:   sheetID,
		sheetName: sheetName,
		queue:     make(chan Record, 512),
		log:       log,
	}
}

// ── Group name resolver (cached) ──────────────────────────────────────────────
func (a *Agent) groupName(jid types.JID) string {
	key := jid.String()
	if v, ok := a.groupCache.Load(key); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := a.client.GetGroupInfo(ctx, jid)
	if err == nil && info.Name != "" {
		a.groupCache.Store(key, info.Name)
		return info.Name
	}
	return jid.User
}

// ── Sender phone resolver ─────────────────────────────────────────────────────
func senderPhone(v *events.Message) string {
	// SenderAlt carries the phone number JID when addressing mode is LID
	if v.Info.SenderAlt.Server == "s.whatsapp.net" && v.Info.SenderAlt.User != "" {
		return v.Info.SenderAlt.User
	}
	return v.Info.Sender.User
}

// ── Message text + type extractor ─────────────────────────────────────────────
func extractText(msg *waE2E.Message) (msgType, text string) {
	if msg == nil {
		return "unknown", ""
	}
	if t := msg.GetConversation(); t != "" {
		return "text", t
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		if t := ext.GetText(); t != "" {
			return "text", t
		}
	}
	if img := msg.GetImageMessage(); img != nil {
		c := img.GetCaption()
		if c == "" {
			c = "[Image]"
		}
		return "image", c
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		c := vid.GetCaption()
		if c == "" {
			c = "[Video]"
		}
		return "video", c
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		c := doc.GetCaption()
		if c == "" {
			fn := doc.GetFileName()
			if fn != "" {
				c = "[Document: " + fn + "]"
			} else {
				c = "[Document]"
			}
		}
		return "document", c
	}
	if msg.GetAudioMessage() != nil {
		return "audio", "[Audio Message]"
	}
	if msg.GetStickerMessage() != nil {
		return "sticker", "[Sticker]"
	}
	if r := msg.GetReactionMessage(); r != nil {
		return "reaction", "[Reaction: " + r.GetText() + "]"
	}
	if loc := msg.GetLocationMessage(); loc != nil {
		return "location", fmt.Sprintf("[Location: %.5f, %.5f]", loc.GetDegreesLatitude(), loc.GetDegreesLongitude())
	}
	if ct := msg.GetContactMessage(); ct != nil {
		return "contact", "[Contact: " + ct.GetDisplayName() + "]"
	}
	return "other", ""
}

// ── Context info extractor (works across all message types) ──────────────────
func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	if msg == nil {
		return nil
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetContextInfo()
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetContextInfo()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetContextInfo()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetContextInfo()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return aud.GetContextInfo()
	}
	return nil
}

// ── Event handler ──────────────────────────────────────────────────────────────
func (a *Agent) handleEvent(rawEvt interface{}) {
	switch v := rawEvt.(type) {
	case *events.Connected:
		fmt.Println("[Agent] Connected to WhatsApp servers.")
	case *events.LoggedOut:
		fmt.Println("[Agent] Logged out from phone — restart the agent to reconnect.")

	case *events.Message:
		// Group messages only
		if !v.Info.IsGroup {
			return
		}
		// Skip edits, revokes, protocol messages
		if v.Info.Edit != "" {
			return
		}
		// Dedup
		if _, seen := a.seenIDs.LoadOrStore(v.Info.ID, true); seen {
			return
		}

		msgType, msgText := extractText(v.Message)
		if msgText == "" {
			return // skip media-only or unsupported with no text
		}

		// Reply threading
		var replyMsgID, replySender, quotedText string
		var isReply bool
		if ci := contextInfo(v.Message); ci != nil && ci.GetQuotedMessage() != nil {
			isReply = true
			replyMsgID = ci.GetStanzaID()
			// Strip @domain from participant
			replySender = strings.Split(ci.GetParticipant(), "@")[0]
			_, quotedText = extractText(ci.GetQuotedMessage())
		}

		// Thread ID: parent message if reply, own ID otherwise
		threadID := v.Info.ID
		if replyMsgID != "" {
			threadID = replyMsgID
		}

		name := v.Info.PushName
		phone := senderPhone(v)
		if name == "" {
			name = phone
		}

		rec := Record{
			MessageID:          v.Info.ID,
			ThreadID:           threadID,
			GroupName:          a.groupName(v.Info.Chat),
			GroupJID:           v.Info.Chat.String(),
			SenderName:         name,
			SenderPhone:        phone,
			MessageType:        msgType,
			Message:            msgText,
			QuotedMessage:      quotedText,
			Date:               v.Info.Timestamp.Format("2006-01-02"),
			Time:               v.Info.Timestamp.Format("15:04:05"),
			IsReply:            isReply,
			RepliedToMessageID: replyMsgID,
			RepliedToSender:    replySender,
		}

		select {
		case a.queue <- rec:
		default:
			fmt.Println("[WARN] Queue full, message dropped:", v.Info.ID)
		}
	}
}

// ── Sheet writer ───────────────────────────────────────────────────────────────
func (a *Agent) ensureHeaders() {
	rangeStr := a.sheetName + "!A1:P1"
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, rangeStr).Do()
	if err != nil || len(resp.Values) == 0 {
		vr := &sheets.ValueRange{Values: [][]interface{}{sheetHeaders}}
		_, _ = a.sheetsvc.Spreadsheets.Values.Update(a.sheetID, rangeStr, vr).
			ValueInputOption("RAW").Do()
	}
}

func (a *Agent) writeToSheet(rec Record) {
	// Get row count for SN
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!A:A").Do()
	sn := 1
	if err == nil && len(resp.Values) > 0 {
		sn = len(resp.Values) // header = row 1, first data SN = 1 (len = 1 before append)
	}

	isReplyStr := "No"
	if rec.IsReply {
		isReplyStr = "Yes"
	}

	row := []interface{}{
		strconv.Itoa(sn),
		rec.MessageID,
		rec.ThreadID,
		rec.GroupName,
		rec.GroupJID,
		rec.SenderName,
		rec.SenderPhone,
		rec.MessageType,
		rec.Message,
		rec.QuotedMessage,
		rec.Date,
		rec.Time,
		isReplyStr,
		rec.RepliedToMessageID,
		rec.RepliedToSender,
		"Not Replied",
	}

	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	_, err = a.sheetsvc.Spreadsheets.Values.Append(a.sheetID, a.sheetName+"!A1", vr).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Do()
	if err != nil {
		fmt.Printf("[ERROR] Sheet write failed: %v\n", err)
		return
	}

	replyTag := ""
	if rec.IsReply {
		replyTag = " [REPLY→" + rec.RepliedToMessageID[:min(8, len(rec.RepliedToMessageID))] + "]"
	}
	fmt.Printf("[%s %s] %s | %s: %s%s\n",
		rec.Date, rec.Time, rec.GroupName, rec.SenderName, rec.Message, replyTag)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *Agent) runWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-a.queue:
			a.writeToSheet(rec)
		}
	}
}

// ── Sheets client ──────────────────────────────────────────────────────────────
func newSheetsService(ctx context.Context, credPath string) (*sheets.Service, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account: %w", err)
	}
	cfg, err := google.JWTConfigFromJSON(data, sheets.SpreadsheetsScope)
	if err != nil {
		return nil, fmt.Errorf("parsing service account: %w", err)
	}
	return sheets.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx)))
}

// ── Main ───────────────────────────────────────────────────────────────────────
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// WhatsApp DB
	dbPath := strings.TrimSpace(os.Getenv("WHATSAPP_DB_PATH"))
	if dbPath == "" {
		dbPath = "./data/whatsapp.db"
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		fmt.Printf("db path error: %v\n", err)
		os.Exit(1)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(absDBPath))
	log := waLog.Stdout("WhatsApp", "WARN", true)

	container, err := sqlstore.New(ctx, "sqlite", dsn, log)
	if err != nil {
		fmt.Printf("sqlstore error: %v\n", err)
		os.Exit(1)
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		fmt.Printf("device store error: %v\n", err)
		os.Exit(1)
	}
	client := whatsmeow.NewClient(deviceStore, log)
	if client.Store.ID == nil {
		fmt.Println("No linked account. Run `go run ./cmd/link` first.")
		os.Exit(1)
	}

	// Google Sheets
	credPath := strings.TrimSpace(os.Getenv("GOOGLE_SERVICE_ACCOUNT_JSON"))
	if credPath == "" {
		credPath = "./service_account.json"
	}
	sheetID := strings.TrimSpace(os.Getenv("GOOGLE_SHEET_ID"))
	sheetName := strings.TrimSpace(os.Getenv("GOOGLE_SHEET_NAME"))
	if sheetName == "" {
		sheetName = "Sheet2"
	}
	if sheetID == "" {
		fmt.Println("GOOGLE_SHEET_ID is not set in .env")
		os.Exit(1)
	}

	svc, err := newSheetsService(ctx, credPath)
	if err != nil {
		fmt.Printf("Google Sheets init error: %v\n", err)
		os.Exit(1)
	}

	agent := newAgent(client, svc, sheetID, sheetName, log)
	agent.ensureHeaders()

	client.AddEventHandler(agent.handleEvent)
	go agent.runWriter(ctx)

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" WhatsApp Group CRM Agent starting...")
	fmt.Printf(" Sheet : %s (ID: %s)\n", sheetName, sheetID)
	fmt.Println(" Scope : Group messages only")
	fmt.Println(" Press CTRL+C to stop")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	if err = client.Connect(); err != nil {
		fmt.Printf("connect error: %v\n", err)
		os.Exit(1)
	}

	<-ctx.Done()
	fmt.Println("\nShutting down...")
	client.Disconnect()
}
