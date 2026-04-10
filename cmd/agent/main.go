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
// A   B            C           D             E          F             G               H        I      J             K                  L                   M
// SN  Sender Name  Sender Phone  Group Name  Group JID  Date Sent     Message Summary  Message  Type  Reply Status  Replied By Names   Replied By Phones   Replies
var sheetHeaders = []interface{}{
	"SN", "Sender Name", "Sender Phone",
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
	RepliedToMessageID string // ID of the message being replied to
	// QuotedMessage holds the text of the quoted/original message when this
	// record is a reply. It allows us to populate the root row with the
	// original message's content if it's not already known. For non-replies
	// this will be empty.
	QuotedMessage string
	// QuotedSenderName / QuotedSenderPhone hold the original sender details
	// extracted from ContextInfo.Participant. Used when the root message is
	// not in cache so we don't fall back to "Unknown".
	QuotedSenderName  string
	QuotedSenderPhone string
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
	// pendingRoots holds group messages that have not yet been replied to.
	// These are cached so that when a reply arrives we can create the row
	// with the original message details instead of using the reply as the root.
	pendingRoots sync.Map // message ID -> Record
	// myPhone and myName identify the linked device owner so that messages
	// sent by the linked phone (IsFromMe) appear with the correct name/number.
	myPhone string
	myName  string
}

func newAgent(client *whatsmeow.Client, svc *sheets.Service, sheetID, sheetName string, log waLog.Logger) *Agent {
	return &Agent{
		client:       client,
		sheetsvc:     svc,
		sheetID:      sheetID,
		sheetName:    sheetName,
		queue:        make(chan Record, 512),
		log:          log,
		pendingRoots: sync.Map{},
		myPhone:      "14699816010", // +1(469)981-6010 — digits only, no + or spaces
		myName:       "P.E Team",
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
		fmt.Printf("[Agent] Linked as: %s (%s)\n", a.myName, a.myPhone)
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
		var replyMsgID string
		var isReply bool
		var quotedText, quotedSenderName, quotedSenderPhone string
		if ci := contextInfo(v.Message); ci != nil && ci.GetQuotedMessage() != nil {
			isReply = true
			replyMsgID = ci.GetStanzaID()
			// Extract quoted/original message text so we can populate the root row later if needed
			qMsgType, qMsgText := extractText(ci.GetQuotedMessage())
			// Use only textual quotes; ignore attachments that have no caption
			if qMsgType != "unknown" && qMsgText != "" {
				quotedText = qMsgText
			}
			// Extract original sender from ContextInfo.Participant (always a phone JID like 923001234567@s.whatsapp.net)
			if participant := ci.GetParticipant(); participant != "" {
				// Participant is a full JID; strip the server suffix to get the phone number
				quotedSenderPhone = strings.Split(participant, "@")[0]
				// ContextInfo has no PushName field — use the phone number as the name.
				// The real name will be present if the root message was cached in pendingRoots.
				quotedSenderName = quotedSenderPhone
			}
		}

		// Thread ID: root of the thread
		threadID := v.Info.ID
		if replyMsgID != "" {
			threadID = replyMsgID
		}

		name := v.Info.PushName
		phone := senderPhone(v)
		// For messages sent from the linked phone, PushName and Sender are
		// empty — use the hardcoded owner identity instead.
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

// ── Sheet helpers ──────────────────────────────────────────────────────────────

func cellStr(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func appendCSV(existing, val string) string {
	if existing == "" {
		return val
	}
	return existing + ", " + val
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensureHeaders writes the header row if not already present.
func (a *Agent) ensureHeaders() {
	// Adjust range to the length of new headers (A to M).
	rangeStr := a.sheetName + "!A1:M1"
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, rangeStr).Do()
	if err != nil || len(resp.Values) == 0 {
		vr := &sheets.ValueRange{Values: [][]interface{}{sheetHeaders}}
		_, _ = a.sheetsvc.Spreadsheets.Values.Update(a.sheetID, rangeStr, vr).
			ValueInputOption("RAW").Do()
	}

	// Apply formatting: freeze header row and set bold grey background.
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
			// Build batch update requests to style the header row and set column widths.
			reqs := []*sheets.Request{
				{
					UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
						Properties: &sheets.SheetProperties{
							SheetId: sid,
							GridProperties: &sheets.GridProperties{
								FrozenRowCount: 1,
							},
						},
						Fields: "gridProperties.frozenRowCount",
					},
				},
				{
					// Set row height for the header row to give breathing space
					UpdateDimensionProperties: &sheets.UpdateDimensionPropertiesRequest{
						Range: &sheets.DimensionRange{
							SheetId:    sid,
							Dimension:  "ROWS",
							StartIndex: 0,
							EndIndex:   1,
						},
						Properties: &sheets.DimensionProperties{
							PixelSize: 32,
						},
						Fields: "pixelSize",
					},
				},
				{
					// Set column widths to a uniform size for better readability
					UpdateDimensionProperties: &sheets.UpdateDimensionPropertiesRequest{
						Range: &sheets.DimensionRange{
							SheetId:    sid,
							Dimension:  "COLUMNS",
							StartIndex: 0,
							EndIndex:   int64(len(sheetHeaders)),
						},
						Properties: &sheets.DimensionProperties{
							PixelSize: 160,
						},
						Fields: "pixelSize",
					},
				},
				{
					// Apply dark blue background, white bold text and centering to header row
					RepeatCell: &sheets.RepeatCellRequest{
						Range: &sheets.GridRange{
							SheetId:          sid,
							StartRowIndex:    0,
							EndRowIndex:      1,
							StartColumnIndex: 0,
							EndColumnIndex:   int64(len(sheetHeaders)),
						},
						Cell: &sheets.CellData{
							UserEnteredFormat: &sheets.CellFormat{
								BackgroundColor: &sheets.Color{
									Red:   0.17,
									Green: 0.45,
									Blue:  0.71,
								},
								TextFormat: &sheets.TextFormat{
									Bold: true,
									ForegroundColor: &sheets.Color{
										Red:   1.0,
										Green: 1.0,
										Blue:  1.0,
									},
									FontSize: 12,
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

// findRowByMessageID searches column B for the given message ID.
// Returns 1-based row number or -1 if not found.
func (a *Agent) findRowByMessageID(msgID string) (int, error) {
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!B:B").Do()
	if err != nil {
		return -1, err
	}
	for i, row := range resp.Values {
		if len(row) > 0 && cellStr(row[0]) == msgID {
			return i + 1, nil // 1-based
		}
	}
	return -1, nil
}

// appendReply finds the original message row and appends the reply info into
// columns J (Reply Status), K (Replied By Names), L (Replied By Phones), M (Replies).
func (a *Agent) appendReply(rec Record) {
	rowNum, err := a.findRowByMessageID(rec.RepliedToMessageID)
	if err != nil {
		fmt.Printf("[ERROR] Sheet search failed: %v\n", err)
		return
	}
	if rowNum == -1 {
		// If the original message isn't recorded yet, handle accordingly.
		// If we've already created a root row (seenIDs has the ID) but the row isn't visible yet,
		// skip to avoid infinite recursion. It will be updated in a subsequent run.
		if _, exists := a.seenIDs.Load(rec.RepliedToMessageID); exists {
			fmt.Printf("[WARN] Row for original msg %s not yet available in sheet, skipping reply for now\n", rec.RepliedToMessageID)
			return
		}
		// Try using cached root details, otherwise create a minimal placeholder.
		if val, ok := a.pendingRoots.Load(rec.RepliedToMessageID); ok {
			root := val.(Record)
			a.appendNewRootWithReply(root, rec)
		} else {
			fmt.Printf("[WARN] Original msg %s not found in sheet or cache, writing reply using reply info\n", rec.RepliedToMessageID)
			// Build root from ContextInfo data extracted in handleEvent (QuotedSenderName/Phone).
			// This fixes the "Unknown" sender problem for old messages not in the agent's cache.
			rootSenderName := rec.QuotedSenderName
			if rootSenderName == "" {
				rootSenderName = rec.QuotedSenderPhone
			}
			if rootSenderName == "" {
				rootSenderName = "Unknown"
			}
			a.appendNewRootWithReply(Record{
				MessageID:   rec.RepliedToMessageID,
				ThreadID:    rec.RepliedToMessageID,
				GroupName:   rec.GroupName,
				GroupJID:    rec.GroupJID,
				SenderName:  rootSenderName,
				SenderPhone: rec.QuotedSenderPhone,
				MessageType: rec.MessageType,
				Message:     "", // will be replaced by quoted message if available
				Date:        rec.Date,
				Time:        rec.Time,
			}, rec)
		}
		return
	}

	// Read current J:M values (Reply Status, Replied By Names, Replied By Phones, Replies)
	rangeStr := fmt.Sprintf("%s!J%d:M%d", a.sheetName, rowNum, rowNum)
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, rangeStr).Do()

	var existingStatus, existingNames, existingPhones, existingReplies string
	if err == nil && len(resp.Values) > 0 {
		row := resp.Values[0]
		if len(row) > 0 {
			existingStatus = cellStr(row[0])
		}
		if len(row) > 1 {
			existingNames = cellStr(row[1])
		}
		if len(row) > 2 {
			existingPhones = cellStr(row[2])
		}
		if len(row) > 3 {
			existingReplies = cellStr(row[3])
		}
	}
	_ = existingStatus

	updatedNames := appendCSV(existingNames, rec.SenderName)
	updatedPhones := appendCSV(existingPhones, rec.SenderPhone)
	updatedReplies := appendCSV(existingReplies, rec.Message)
	vr := &sheets.ValueRange{
		Values: [][]interface{}{{
			"Replied",
			updatedNames,
			updatedPhones,
			updatedReplies,
		}},
	}
	_, err = a.sheetsvc.Spreadsheets.Values.Update(a.sheetID, rangeStr, vr).
		ValueInputOption("USER_ENTERED").Do()
	if err != nil {
		fmt.Printf("[ERROR] Reply update failed: %v\n", err)
		return
	}
	fmt.Printf("[%s %s] %s | REPLY by %s (%s) → \"%s\"\n",
		rec.Date, rec.Time, rec.GroupName, rec.SenderName, rec.SenderPhone, rec.Message)
}

// appendNewRow writes a brand-new message as a new sheet row.
// Column order: SN | Sender Name | Sender Phone | Group Name | Group JID |
//
//	Date Sent | Message Summary | Message | Type |
//	Reply Status | Replied By Names | Replied By Phones | Replies
func (a *Agent) appendNewRow(rec Record) {
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!A:A").Do()
	sn := 1
	if err == nil && len(resp.Values) > 0 {
		sn = len(resp.Values)
	}

	// Message Summary: first 80 chars of the message text (like the Email Summary column in the sample)
	summary := rec.Message
	if len([]rune(summary)) > 80 {
		runes := []rune(summary)
		summary = string(runes[:80]) + "..."
	}

	row := []interface{}{
		strconv.Itoa(sn), // A: SN
		rec.SenderName,   // B: Sender Name
		rec.SenderPhone,  // C: Sender Phone
		rec.GroupName,    // D: Group Name
		rec.GroupJID,     // E: Group JID
		rec.Date,         // F: Date Sent
		summary,          // G: Message Summary
		rec.Message,      // H: Message (full)
		rec.MessageType,  // I: Type
		"Not Replied",    // J: Reply Status
		"",               // K: Replied By Names
		"",               // L: Replied By Phones
		"",               // M: Replies
	}

	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	_, err = a.sheetsvc.Spreadsheets.Values.Append(a.sheetID, a.sheetName+"!A1", vr).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Do()
	if err != nil {
		fmt.Printf("[ERROR] Sheet append failed: %v\n", err)
		return
	}

	fmt.Printf("[%s %s] %s | %s (%s): %s\n",
		rec.Date, rec.Time, rec.GroupName, rec.SenderName, rec.SenderPhone, rec.Message)
}

// appendNewRootWithReply writes a new row for a root message together with the first reply.
// It sets the row as replied and populates the first reply details. This is used when
// a reply arrives for a message whose root has not been recorded yet. The root details
// come from either the pending cache or from a minimal placeholder.
func (a *Agent) appendNewRootWithReply(root Record, reply Record) {
	// Avoid duplicates: if the message ID already exists, simply append the reply.
	if _, exists := a.seenIDs.Load(root.MessageID); exists {
		// Do not attempt to append a new root again. Instead, call appendReply once.
		a.appendReply(reply)
		return
	}
	// Determine the next serial number (SN)
	resp, err := a.sheetsvc.Spreadsheets.Values.Get(a.sheetID, a.sheetName+"!A:A").Do()
	sn := 1
	if err == nil && len(resp.Values) > 0 {
		sn = len(resp.Values)
	}
	// Compose the row. If the root message text is empty, but the reply record contains
	// a quoted/original message, use that quoted text as the message. This ensures
	// that when a reply arrives for an old message not seen before, the original
	// message appears in the "Message" column.
	messageText := root.Message
	if messageText == "" && reply.QuotedMessage != "" {
		messageText = reply.QuotedMessage
	}

	// Message Summary: first 80 chars
	summary := messageText
	if len([]rune(summary)) > 80 {
		runes := []rune(summary)
		summary = string(runes[:80]) + "..."
	}

	row := []interface{}{
		strconv.Itoa(sn),  // A: SN
		root.SenderName,   // B: Sender Name
		root.SenderPhone,  // C: Sender Phone
		root.GroupName,    // D: Group Name
		root.GroupJID,     // E: Group JID
		root.Date,         // F: Date Sent
		summary,           // G: Message Summary
		messageText,       // H: Message (full)
		root.MessageType,  // I: Type
		"Replied",         // J: Reply Status
		reply.SenderName,  // K: Replied By Names
		reply.SenderPhone, // L: Replied By Phones
		reply.Message,     // M: Replies
	}
	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	// Mark as seen before writing to prevent concurrent duplicate creation
	a.seenIDs.Store(root.MessageID, true)

	_, err = a.sheetsvc.Spreadsheets.Values.Append(a.sheetID, a.sheetName+"!A1", vr).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Do()
	if err != nil {
		fmt.Printf("[ERROR] Sheet append failed: %v\n", err)
		return
	}
	fmt.Printf("[%s %s] %s | NEW THREAD: %s (%s) replied by %s (%s) → \"%s\"\n",
		reply.Date, reply.Time, root.GroupName, root.SenderName, root.SenderPhone,
		reply.SenderName, reply.SenderPhone, messageText)
}

func (a *Agent) writeToSheet(rec Record) {
	if rec.IsReply {
		a.appendReply(rec)
		// Also cache this reply record so that if someone replies to it later,
		// we have the correct sender name and phone available (fixes nested reply name issue).
		if _, exists := a.seenIDs.Load(rec.MessageID); !exists {
			a.pendingRoots.Store(rec.MessageID, rec)
		}
	} else {
		// Cache non-reply messages until they receive replies. Do not write them immediately.
		if _, exists := a.seenIDs.Load(rec.MessageID); !exists {
			a.pendingRoots.Store(rec.MessageID, rec)
		}
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
	// Preload existing message IDs from the sheet so that previously recorded
	// messages are not processed again on restart. We read column B (Message ID)
	// starting from the second row and populate the seenIDs map.
	func() {
		// In a closure to limit scope of err/res variables
		resp, err := svc.Spreadsheets.Values.Get(sheetID, sheetName+"!B2:B").Do()
		if err == nil {
			for _, row := range resp.Values {
				if len(row) > 0 {
					id := fmt.Sprintf("%v", row[0])
					if id != "" {
						agent.seenIDs.Store(id, true)
					}
				}
			}
		}
	}()

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
