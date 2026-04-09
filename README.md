# WhatsApp Agent (Go)

This folder contains the Go implementation plan for the WhatsApp group CRM agent.

## Goal

Build a passive WhatsApp group logger that:

- Listens to group messages using whatsmeow
- Captures sender name, sender number, message text, timestamp, group
- Detects reply/quote links to parent messages
- Writes structured records to Google Sheets
- Generates optional LLM summaries in controlled batches

## Analysis of Provided Files

- The file in GOLANG/pkgfile.md confirms whatsmeow has the required event APIs for:
	- receiving group messages
	- message key handling used for replies/reactions
	- group info and participant metadata
- The file in GOLANG/context7.md points to useful context7 references for whatsmeow docs/skills/benchmarks.
- This folder initially had only a default GitLab template README and no Go project scaffolding yet.

## Prerequisites (Windows)

### Mandatory

1. Go 1.22 or newer
2. Git
3. A dedicated WhatsApp number for automation
4. Google Cloud project with Google Sheets API enabled
5. Service account JSON credentials for Google Sheets write access

### Strongly Recommended

1. GCC toolchain (MinGW-w64) only if using CGO sqlite3 driver (not needed for current pure-Go sqlite path)
2. VS Code Go extension
3. A stable machine that stays online for session continuity

## External Accounts and Keys Required

1. WhatsApp account used for QR linking as a companion device
2. Google Sheet target and share permission granted to service account email
3. Optional LLM API key (Anthropic/OpenAI) for periodic summaries

## Environment Variables (planned)

Use the .env.example file in this folder as reference.

Core values you will set:

- APP_ENV
- LOG_LEVEL
- WHATSAPP_DB_PATH
- GOOGLE_SHEET_ID
- GOOGLE_SHEET_NAME
- GOOGLE_SERVICE_ACCOUNT_JSON
- SUMMARY_ENABLED
- ANTHROPIC_API_KEY
- OPENAI_API_KEY

## Preflight Script

Use preflight.ps1 to validate local prerequisites before scaffolding and coding.

It checks:

- Go installation and version
- Git availability
- GCC availability (for sqlite3/cgo path)
- Presence of .env file

## Next Setup Step

After prerequisites pass, we will initialize:

1. go.mod
2. cmd/agent entrypoint
3. internal packages for config, logger, whatsapp, sheets, storage, summarizer
4. graceful shutdown and retry logic

## Link WhatsApp Account First

We have added a first command that handles WhatsApp QR pairing and persists the linked session in the local sqlite database.

Run:

1. `go mod tidy`
2. `go run ./cmd/link`

Then on mobile WhatsApp:

1. Open Linked Devices
2. Tap Link a Device
3. Scan the QR shown in terminal

If it says already linked, delete the local db file set in `WHATSAPP_DB_PATH` and rerun.

## Important Note

whatsmeow is based on the WhatsApp Web protocol and is unofficial with respect to Meta business APIs. Use a spare account and avoid using a primary business/personal number.
