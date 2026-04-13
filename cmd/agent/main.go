package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"SN", "Message ID", "Sender Name", "Sender Phone",
	"Group Name", "Group JID",
	"Date Sent",
	"Message Summary", "Message", "Type",
	"Reply Status",
	"Replied By Names", "Replied By Phones", "Replies",
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
	Date               string
	Time               string
	IsReply            bool
	RepliedToMessageID string
	QuotedMessage      string
	QuotedSenderName   string
	QuotedSenderPhone  string
}

// ── Supabase DB row (matches the messages table) ───────────────────────────────
type DBMessage struct {
	MessageID       string `json:"message_id"`
	ThreadID        string `json:"thread_id"`
	GroupName       string `json:"group_name"`
	GroupJID        string `json:"group_jid"`
	SenderName      string `json:"sender_name"`
	SenderPhone     string `json:"sender_phone"`
	MessageType     string `json:"message_type"`
	Message         string `json:"message"`
	Summary         string `json:"summary"`
	DateSent        string `json:"date_sent"`
	ReplyStatus     string `json:"reply_status"`
	RepliedByNames  string `json:"replied_by_names"`
	RepliedByPhones string `json:"replied_by_phones"`
	Replies         string `json:"replies"`
}

// ── Supabase client ────────────────────────────────────────────────────────────
type SupabaseClient struct {
	url  string // e.g. https://xxxx.supabase.co
	key  string // service_role key
	http *http.Client
}

func newSupabaseClient(url, key string) *SupabaseClient {
	return &SupabaseClient{
		url:  strings.TrimRight(url, "/"),
		key:  key,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *SupabaseClient) do(method, path string, body interface{}, extraHeaders map[string]string) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.url+"/rest/v1/"+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("apikey", s.key)
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// IsSeen returns true if message_id exists in seen_ids table.
func (s *SupabaseClient) IsSeen(id string) bool {
	_, status, err := s.do("GET", "seen_ids?message_id=eq."+id+"&select=message_id", nil, nil)
	if err != nil {
		return false
	}
	// If response is "[]" then not seen
	return status == 200 && strings.TrimSpace(string([]byte{})) != "[]"
}

// MarkSeen inserts message_id into seen_ids. Returns true if already existed.
func (s *SupabaseClient) MarkSeen(id string) (alreadySeen bool) {
	// Check first
	b, status, err := s.do("GET", "seen_ids?message_id=eq."+id+"&select=message_id", nil, nil)
	if err == nil && status == 200 {
		var rows []map[string]interface{}
		if json.Unmarshal(b, &rows) == nil && len(rows) > 0 {
			return true
		}
	}
	// Insert
	s.do("POST", "seen_ids", map[string]string{"message_id": id},
		map[string]string{"Prefer": "return=minimal"})
	return false
}

// GetMessage fetches a message row by message_id. Returns nil if not found.
func (s *SupabaseClient) GetMessage(id string) *DBMessage {
	b, status, err := s.do("GET", "messages?message_id=eq."+id+"&select=*", nil, nil)
	if err != nil || status != 200 {
		return nil
	}
	var rows []DBMessage
	if json.Unmarshal(b, &rows) != nil || len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// UpsertMessage inserts or updates a message row (merge on message_id).
func (s *SupabaseClient) UpsertMessage(msg DBMessage) error {
	_, status, err := s.do("POST", "messages", msg,
		map[string]string{
			"Prefer": "resolution=merge-duplicates,return=minimal",
		})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("supabase upsert failed: status %d", status)
	}
	return nil
}

// AppendReply updates reply columns on the root message row.
func (s *SupabaseClient) AppendReply(rootID, name, phone, replyText string) error {
	root := s.GetMessage(rootID)
	if root == nil {
		return fmt.Errorf("root message %s not found in supabase", rootID)
	}
	updatedNames := appendCSV(root.RepliedByNames, name)
	updatedPhones := appendCSV(root.RepliedByPhones, phone)
	updatedReplies := appendCSV(root.Replies, replyText)

	patch := map[string]string{
		"reply_status":      "Replied",
		"replied_by_names":  updatedNames,
		"replied_by_phones": updatedPhones,
		"replies":           updatedReplies,
	}
	_, status, err := s.do("PATCH", "messages?message_id=eq."+rootID, patch,
		map[string]string{"Prefer": "return=minimal"})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("supabase patch failed: status %d", status)
	}
	return nil
}

// AllNotReplied returns all messages with reply_status = 'Not Replied'.
func (s *SupabaseClient) AllNotReplied() []DBMessage {
	b, status, err := s.do("GET", "messages?reply_status=eq.Not+Replied&select=*", nil, nil)
	if err != nil || status != 200 {
		return nil
	}
	var rows []DBMessage
	json.Unmarshal(b, &rows)
	return rows
}

// AllSeenIDs returns all message_ids from the seen_ids table.
func (s *SupabaseClient) AllSeenIDs() []string {
	b, status, err := s.do("GET", "seen_ids?select=message_id", nil, nil)
	if err != nil || status != 200 {
		return nil
	}
	var rows []map[string]string
	json.Unmarshal(b, &rows)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := r["message_id"]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ── Agent ──────────────────────────────────────────────────────────────────────
type Agent struct {
	client     *whatsmeow.Client
	groupCache sync.Map // JID string -> group name (kept in-memory, cheap)
	sheetsvc   *sheets.Service
	sheetID    string
	sheetName  string
	queue      chan Record
	log        waLog.Logger
	db         *SupabaseClient
	myPhone    string
	myName     string
}

func newAgent(client *whatsmeow.Client, svc *sheets.Service, sheetID, sheetName string, db *SupabaseClient, log waLog.Logger) *Agent {
	return &Agent{
		client:    client,
		sheetsvc:  svc,
		sheetID:   sheetID,
		sheetName: sheetName,
		queue:     make(chan Record, 512),
		log:       log,
		db:        db,
		myPhone:   "14699816010",
		myName:    "P.E Team",
	}
}

// ── Group name resolver ────────────────────────────────────────────────────────
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

// ── Sender phone resolver ──────────────────────────────────────────────────────
func senderPhone(v *events.Message) string {
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

// ── Context info extractor ────────────────────────────────────────────────────
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
	case *events.HistorySync:
		// Drop history sync events immediately — we only care about live messages.
		// Processing history sync is the #1 cause of OOM on low-memory servers.
		_ = v
		return
	case *events.Connected:
		fmt.Println("[Agent] Connected to WhatsApp servers.")
		fmt.Printf("[Agent] Linked as: %s (%s)\n", a.myName, a.myPhone)
	case *events.LoggedOut:
		fmt.Println("[Agent] Logged out from phone — restart the agent to reconnect.")

	case *events.Message:
		if !v.Info.IsGroup {
			return
		}
		if v.Info.Edit != "" {
			return
		}

		// ── Dedup via Supabase ────────────────────────────────────────────
		if a.db.MarkSeen(v.Info.ID) {
			return // already processed
		}

		msgType, msgText := extractText(v.Message)
		if msgText == "" {
			return
		}

		var replyMsgID string
		var isReply bool
		var quotedText, quotedSenderName, quotedSenderPhone string
		if ci := contextInfo(v.Message); ci != nil && ci.GetQuotedMessage() != nil {
			isReply = true
			replyMsgID = ci.GetStanzaID()
			qMsgType, qMsgText := extractText(ci.GetQuotedMessage())
			if qMsgType != "unknown" && qMsgText != "" {
				quotedText = qMsgText
			}
			if participant := ci.GetParticipant(); participant != "" {
				quotedSenderPhone = strings.Split(participant, "@")[0]
				quotedSenderName = quotedSenderPhone
			}
		}

		threadID := v.Info.ID
		if replyMsgID != "" {
			threadID = replyMsgID
		}

		name := v.Info.PushName
		phone := senderPhone(v)
		if v.Info.IsFromMe {
			phone = a.myPhone
			name = a.myName
		}
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
			Date:               v.Info.Timestamp.Format("2006-01-02"),
			Time:               v.Info.Timestamp.Format("15:04:05"),
			IsReply:            isReply,
			RepliedToMessageID: replyMsgID,
			QuotedMessage:      quotedText,
			QuotedSenderName:   quotedSenderName,
			QuotedSenderPhone:  quotedSenderPhone,
		}

		select {
		case a.queue <- rec:
		default:
			fmt.Println("[WARN] Queue full, message dropped:", v.Info.ID)
		}
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func cellStr(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func safeIdx(row []interface{}, i int) string {
	if i < len(row) {
		return fmt.Sprintf("%v", row[i])
	}
	return ""
}

func appendCSV(existing, val string) string {
	if existing == "" {
		return val
	}
	return existing + ", " + val
}

func makeSummary(msg string) string {
	runes := []rune(msg)
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return msg
}

func recordToDBMessage(rec Record, replyStatus string) DBMessage {
	return DBMessage{
		MessageID:   rec.MessageID,
		ThreadID:    rec.ThreadID,
		GroupName:   rec.GroupName,
		GroupJID:    rec.GroupJID,
		SenderName:  rec.SenderName,
		SenderPhone: rec.SenderPhone,
		MessageType: rec.MessageType,
		Message:     rec.Message,
		Summary:     makeSummary(rec.Message),
		DateSent:    rec.Date,
		ReplyStatus: replyStatus,
	}
}

// ── Sheet helpers ──────────────────────────────────────────────────────────────

func (a *Agent) ensureHeaders() {
	rangeStr := a.sheetName + "!A1:N1"
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, rangeStr).Do()
	needsHeaders := err != nil ||
		len(resp.Values) == 0 ||
		len(resp.Values[0]) == 0 ||
		fmt.Sprintf("%v", resp.Values[0][0]) == ""
	if needsHeaders {
		vr := &sheets.ValueRange{Values: [][]interface{}{sheetHeaders}}
		_, _ = a.sheetsvc.Spreadsheets.Values.Update(a.sheetID, rangeStr, vr).
			ValueInputOption("RAW").Do()
	}

	spreadsheet, err2 := a.sheetsvc.Spreadsheets.Get(a.sheetID).Fields("sheets.properties").Do()
	if err2 == nil {
		var sid int64 = -1
		for _, sh := range spreadsheet.Sheets {
			if sh.Properties != nil && sh.Properties.Title == a.sheetName {
				sid = sh.Properties.SheetId
				break
			}
		}
		if sid >= 0 {
			reqs := []*sheets.Request{
				{
					UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
						Properties: &sheets.SheetProperties{
							SheetId:        sid,
							GridProperties: &sheets.GridProperties{FrozenRowCount: 1},
						},
						Fields: "gridProperties.frozenRowCount",
					},
				},
				{
					UpdateDimensionProperties: &sheets.UpdateDimensionPropertiesRequest{
						Range: &sheets.DimensionRange{
							SheetId: sid, Dimension: "ROWS", StartIndex: 0, EndIndex: 1,
						},
						Properties: &sheets.DimensionProperties{PixelSize: 32},
						Fields:     "pixelSize",
					},
				},
				{
					UpdateDimensionProperties: &sheets.UpdateDimensionPropertiesRequest{
						Range: &sheets.DimensionRange{
							SheetId: sid, Dimension: "COLUMNS",
							StartIndex: 0, EndIndex: int64(len(sheetHeaders)),
						},
						Properties: &sheets.DimensionProperties{PixelSize: 160},
						Fields:     "pixelSize",
					},
				},
				{
					RepeatCell: &sheets.RepeatCellRequest{
						Range: &sheets.GridRange{
							SheetId: sid, StartRowIndex: 0, EndRowIndex: 1,
							StartColumnIndex: 0, EndColumnIndex: int64(len(sheetHeaders)),
						},
						Cell: &sheets.CellData{
							UserEnteredFormat: &sheets.CellFormat{
								BackgroundColor: &sheets.Color{Red: 0.17, Green: 0.45, Blue: 0.71},
								TextFormat: &sheets.TextFormat{
									Bold:            true,
									ForegroundColor: &sheets.Color{Red: 1.0, Green: 1.0, Blue: 1.0},
									FontSize:        12,
								},
								HorizontalAlignment: "CENTER",
								VerticalAlignment:   "MIDDLE",
							},
						},
						Fields: "userEnteredFormat(backgroundColor,textFormat,horizontalAlignment,verticalAlignment)",
					},
				},
			}
			_, _ = a.sheetsvc.Spreadsheets.BatchUpdate(a.sheetID, &sheets.BatchUpdateSpreadsheetRequest{
				Requests: reqs,
			}).Do()
		}
	}
}

func (a *Agent) findRowByMessageID(msgID string) (int, error) {
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!B:B").Do()
	if err != nil {
		return -1, err
	}
	for i, row := range resp.Values {
		if len(row) > 0 && cellStr(row[0]) == msgID {
			return i + 1, nil
		}
	}
	return -1, nil
}

func (a *Agent) nextSN() int {
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!A:A").Do()
	if err == nil && len(resp.Values) > 0 {
		return len(resp.Values)
	}
	return 1
}

// appendNewRow writes a brand-new root message row to the sheet.
func (a *Agent) appendNewRow(rec Record, replyStatus string, repliedByName, repliedByPhone, replies string) {
	sn := a.nextSN()
	row := []interface{}{
		strconv.Itoa(sn),
		rec.MessageID,
		rec.SenderName,
		rec.SenderPhone,
		rec.GroupName,
		rec.GroupJID,
		rec.Date,
		makeSummary(rec.Message),
		rec.Message,
		rec.MessageType,
		replyStatus,
		repliedByName,
		repliedByPhone,
		replies,
	}
	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	_, err := a.sheetsvc.Spreadsheets.Values.Append(a.sheetID, a.sheetName+"!A1", vr).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Do()
	if err != nil {
		fmt.Printf("[ERROR] Sheet append failed: %v\n", err)
	}
}

// updateReplyInSheet updates reply columns K:N for an existing sheet row.
func (a *Agent) updateReplyInSheet(rowNum int, names, phones, replies string) {
	rangeStr := fmt.Sprintf("%s!K%d:N%d", a.sheetName, rowNum, rowNum)
	vr := &sheets.ValueRange{
		Values: [][]interface{}{{"Replied", names, phones, replies}},
	}
	_, err := a.sheetsvc.Spreadsheets.Values.Update(a.sheetID, rangeStr, vr).
		ValueInputOption("USER_ENTERED").Do()
	if err != nil {
		fmt.Printf("[ERROR] Reply update failed: %v\n", err)
	}
}

// ── Core write logic ───────────────────────────────────────────────────────────

func (a *Agent) writeToSheet(rec Record) {
	if !rec.IsReply {
		// ── Root message ──────────────────────────────────────────────────
		// Save to Supabase immediately (source of truth).
		// Do NOT write to sheet yet — wait until a reply comes in.
		// (Same behaviour as before, but now persisted in Supabase.)
		dbMsg := recordToDBMessage(rec, "Not Replied")
		if err := a.db.UpsertMessage(dbMsg); err != nil {
			fmt.Printf("[ERROR] Supabase upsert failed: %v\n", err)
		}
		fmt.Printf("[%s %s] %s | CACHED: %s (%s): %s\n",
			rec.Date, rec.Time, rec.GroupName, rec.SenderName, rec.SenderPhone, rec.Message)
		return
	}

	// ── Reply message ─────────────────────────────────────────────────────
	root := a.db.GetMessage(rec.RepliedToMessageID)
	if root == nil {
		// Root not in Supabase — create a placeholder using ContextInfo data.
		rootSenderName := rec.QuotedSenderName
		if rootSenderName == "" {
			rootSenderName = rec.QuotedSenderPhone
		}
		if rootSenderName == "" {
			rootSenderName = "Unknown"
		}
		msgText := rec.QuotedMessage
		root = &DBMessage{
			MessageID:   rec.RepliedToMessageID,
			ThreadID:    rec.RepliedToMessageID,
			GroupName:   rec.GroupName,
			GroupJID:    rec.GroupJID,
			SenderName:  rootSenderName,
			SenderPhone: rec.QuotedSenderPhone,
			MessageType: rec.MessageType,
			Message:     msgText,
			Summary:     makeSummary(msgText),
			DateSent:    rec.Date,
			ReplyStatus: "Not Replied",
		}
		// Mark the placeholder as seen so we don't create it again
		a.db.MarkSeen(rec.RepliedToMessageID)
		if err := a.db.UpsertMessage(*root); err != nil {
			fmt.Printf("[ERROR] Supabase placeholder upsert failed: %v\n", err)
		}
	}

	// Update Supabase reply fields
	if err := a.db.AppendReply(rec.RepliedToMessageID, rec.SenderName, rec.SenderPhone, rec.Message); err != nil {
		fmt.Printf("[ERROR] Supabase AppendReply failed: %v\n", err)
	}

	// Now sync to Google Sheet
	rowNum, err := a.findRowByMessageID(rec.RepliedToMessageID)
	if err != nil {
		fmt.Printf("[ERROR] Sheet search failed: %v\n", err)
		return
	}

	// Re-fetch updated root from Supabase (has the merged reply data)
	updatedRoot := a.db.GetMessage(rec.RepliedToMessageID)

	if rowNum == -1 {
		// Root row doesn't exist in sheet yet — write it now with reply data embedded
		rootRec := Record{
			MessageID:   root.MessageID,
			GroupName:   root.GroupName,
			GroupJID:    root.GroupJID,
			SenderName:  root.SenderName,
			SenderPhone: root.SenderPhone,
			MessageType: root.MessageType,
			Message:     root.Message,
			Date:        root.DateSent,
		}
		names, phones, replies := "", "", ""
		if updatedRoot != nil {
			names = updatedRoot.RepliedByNames
			phones = updatedRoot.RepliedByPhones
			replies = updatedRoot.Replies
		}
		a.appendNewRow(rootRec, "Replied", names, phones, replies)
		fmt.Printf("[%s %s] %s | NEW THREAD: %s (%s) replied by %s (%s) → \"%s\"\n",
			rec.Date, rec.Time, root.GroupName, root.SenderName, root.SenderPhone,
			rec.SenderName, rec.SenderPhone, rec.Message)
	} else {
		// Row exists — update the reply columns only
		names, phones, replies := rec.SenderName, rec.SenderPhone, rec.Message
		if updatedRoot != nil {
			names = updatedRoot.RepliedByNames
			phones = updatedRoot.RepliedByPhones
			replies = updatedRoot.Replies
		}
		a.updateReplyInSheet(rowNum, names, phones, replies)
		fmt.Printf("[%s %s] %s | REPLY by %s (%s) → \"%s\"\n",
			rec.Date, rec.Time, rec.GroupName, rec.SenderName, rec.SenderPhone, rec.Message)
	}
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
func newSheetsService(ctx context.Context, credJSON string) (*sheets.Service, error) {
	cfg, err := google.JWTConfigFromJSON([]byte(credJSON), sheets.SpreadsheetsScope)
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
	client.EmitAppStateEventsOnFullSync = false
	client.AutomaticMessageRerequestFromPhone = false
	if client.Store.ID == nil {
		fmt.Println("No linked account. Run `go run ./cmd/link` first.")
		os.Exit(1)
	}

	// Google Sheets
	credJSON := strings.TrimSpace(os.Getenv("GOOGLE_SERVICE_ACCOUNT_JSON"))
	if credJSON == "" {
		fmt.Println("GOOGLE_SERVICE_ACCOUNT_JSON is not set in environment")
		os.Exit(1)
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
	svc, err := newSheetsService(ctx, credJSON)
	if err != nil {
		fmt.Printf("Google Sheets init error: %v\n", err)
		os.Exit(1)
	}

	// Supabase
	supabaseURL := strings.TrimSpace(os.Getenv("SUPABASE_URL"))
	supabaseKey := strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY"))
	if supabaseURL == "" || supabaseKey == "" {
		fmt.Println("SUPABASE_URL and SUPABASE_SERVICE_ROLE_KEY must be set in .env")
		os.Exit(1)
	}
	db := newSupabaseClient(supabaseURL, supabaseKey)

	agent := newAgent(client, svc, sheetID, sheetName, db, log)
	agent.ensureHeaders()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" WhatsApp Group CRM Agent starting...")
	fmt.Printf(" Sheet     : %s (ID: %s)\n", sheetName, sheetID)
	fmt.Printf(" Supabase  : %s\n", supabaseURL)
	fmt.Println(" Scope     : Group messages only")
	fmt.Println(" Press CTRL+C to stop")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	client.AddEventHandler(agent.handleEvent)
	go agent.runWriter(ctx)

	if err = client.Connect(); err != nil {
		fmt.Printf("connect error: %v\n", err)
		os.Exit(1)
	}

	<-ctx.Done()
	fmt.Println("\nShutting down...")
	client.Disconnect()
}
