package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/joho/godotenv/autoload"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

func main() {
	phoneFlag := flag.String("phone", "", "Phone number to pair with (e.g. 923365114039). If empty, QR code mode is used.")
	flag.Parse()

	dbPath := strings.TrimSpace(os.Getenv("WHATSAPP_DB_PATH"))
	if dbPath == "" {
		dbPath = "./data/whatsapp.db"
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Printf("failed to create db directory: %v\n", err)
		os.Exit(1)
	}

	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		fmt.Printf("failed to resolve db path: %v\n", err)
		os.Exit(1)
	}

	// WAL mode is crucial for sqlite with whatsmeow to prevent locks during heavy initial history sync
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(absDBPath))
	log := waLog.Stdout("WhatsApp", "INFO", true)

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
	client.EmitAppStateEventsOnFullSync = false
	client.AutomaticMessageRerequestFromPhone = false

	if client.Store.ID == nil {
		fmt.Println("No linked account found. Starting pairing...")

		if err = client.Connect(); err != nil {
			fmt.Printf("failed to connect: %v\n", err)
			os.Exit(1)
		}

		phone := strings.TrimSpace(*phoneFlag)
		if phone != "" {
			// ── Phone number pairing mode ──────────────────────────────────────
			fmt.Printf("Requesting pairing code for number: +%s\n", phone)
			code, err := client.PairPhone(context.Background(), phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				fmt.Printf("failed to get pairing code: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("═══════════════════════════════════════")
			fmt.Printf("  Pairing code: %s\n", code)
			fmt.Println("═══════════════════════════════════════")
			fmt.Println("On your phone: Linked Devices > Link a Device > Link with phone number")
			fmt.Println("Enter the code above. Waiting for pairing to complete...")

			// Block until paired (Connected event) or user aborts
			connectedCh := make(chan struct{}, 1)
			hid := client.AddEventHandler(func(rawEvt interface{}) {
				if _, ok := rawEvt.(*events.Connected); ok {
					select {
					case connectedCh <- struct{}{}:
					default:
					}
				}
			})
			defer client.RemoveEventHandler(hid)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			select {
			case <-sigCh:
				fmt.Println("\nAborted by user.")
				client.Disconnect()
				os.Exit(0)
			case <-connectedCh:
				fmt.Println("Paired and connected successfully!")
			}
		} else {
			// ── QR code pairing mode (default) ────────────────────────────────
			qrChan, err := client.GetQRChannel(context.Background())
			if err != nil {
				fmt.Printf("failed to get QR channel: %v\n", err)
				client.Disconnect()
				os.Exit(1)
			}

			fmt.Println("Scan the QR code below from WhatsApp -> Linked Devices -> Link a Device:")
			for evt := range qrChan {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					fmt.Println("Pairing event:", evt.Event)
				}
			}
		}
	} else {
		fmt.Printf("Already linked as: %s\n", client.Store.ID)
		if err = client.Connect(); err != nil {
			fmt.Printf("failed to connect: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("\n========================================================")
	fmt.Println("Client is running and connected! Press CTRL+C to exit.")
	fmt.Println("Wait at least 2 minutes before exiting on the first run")
	fmt.Println("so the phone can fully synchronize history.")
	fmt.Println("========================================================")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\nDisconnecting...")
	client.Disconnect()
}
