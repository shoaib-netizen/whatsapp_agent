package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/joho/godotenv/autoload"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		// Extract basic info
		senderName := v.Info.PushName
		senderPhone := v.Info.Sender.User
		msgText := v.Message.GetConversation()
		
		// If it's an extended text message, the text might be here instead
		if msgText == "" && v.Message.GetExtendedTextMessage() != nil {
			msgText = v.Message.GetExtendedTextMessage().GetText()
		}

		// Reply threading
		var replyToMsgID string
		var replyToSender string
		contextInfo := v.Message.GetExtendedTextMessage().GetContextInfo()
		if contextInfo != nil && contextInfo.GetQuotedMessage() != nil {
			replyToMsgID = contextInfo.GetStanzaID()
			replyToSender = contextInfo.GetParticipant()
		}

		// Determine chat context
		chatType := "Direct"
		if v.Info.IsGroup {
			chatType = "Group"
		}

		// Format output
		line := fmt.Sprintf("[%s] [%s] %s (+%s): %s",
			v.Info.Timestamp.Format("2006-01-02 15:04:05"),
			chatType,
			senderName,
			senderPhone,
			msgText,
		)

		if replyToMsgID != "" {
			line += fmt.Sprintf(" (Replied to: %s by %s)", replyToMsgID, replyToSender)
		}

		fmt.Println("New Message:", line)

		// Append to messages.txt
		f, err := os.OpenFile("messages.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString(line + "\n")
			f.Close()
		}

	case *events.Connected:
		fmt.Println("Agent connected to WhatsApp servers.")
	case *events.LoggedOut:
		fmt.Println("Agent was logged out from phone.")
	}
}

func main() {
	dbPath := strings.TrimSpace(os.Getenv("WHATSAPP_DB_PATH"))
	if dbPath == "" {
		dbPath = "./data/whatsapp.db"
	}

	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		fmt.Printf("failed to resolve db path: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(absDBPath))
	log := waLog.Stdout("WhatsAppAgent", "WARN", true) // Set to WARN so it doesn't flood terminal with debug logs

	container, err := sqlstore.New(context.Background(), "sqlite", dsn, log)
	if err != nil {
		fmt.Printf("failed to initialize sqlstore: %v\n", err)
		os.Exit(1)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		fmt.Printf("failed to get device store: %v\n", err)
		os.Exit(1)
	}

	client := whatsmeow.NewClient(deviceStore, log)

	if client.Store.ID == nil {
		fmt.Println("No linked account found. Please run the `link` command first.")
		os.Exit(1)
	}

	client.AddEventHandler(eventHandler)

	fmt.Println("Starting WhatsApp Agent listener...")
	err = client.Connect()
	if err != nil {
		fmt.Printf("failed to connect: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Listening for new messages. Send a message to this WhatsApp number to test.")
	fmt.Println("Press CTRL+C to exit.")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
	fmt.Println("Agent disconnected.")
}
