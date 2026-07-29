package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/relay"
	"github.com/printbridge/printbridge/agent/pkg/server"
	"github.com/printbridge/printbridge/agent/pkg/telemetry"
	"github.com/printbridge/printbridge/agent/pkg/tray"
	"github.com/printbridge/printbridge/agent/pkg/updater"
)

const Version = "0.1.0"

func main() {
	portFlag := flag.Int("port", 9567, "Local WebSocket server & Dashboard port")
	dbPathFlag := flag.String("db", "", "Path to SQLite database file")
	headlessFlag := flag.Bool("headless", false, "Run without system tray icon UI")
	relayURLFlag := flag.String("relay-url", "", "Cloudflare Relay URL (e.g. wss://relay.printbridge.dev)")
	agentIDFlag := flag.String("agent-id", "", "Registered Agent ID for Cloud Relay")
	apiKeyFlag := flag.String("api-key", "", "API key / Secret for Cloud Relay authentication")
	tlsCertFlag := flag.String("tls-cert", "", "Path to TLS certificate file for WSS/HTTPS")
	tlsKeyFlag := flag.String("tls-key", "", "Path to TLS private key file for WSS/HTTPS")
	flag.Parse()

	dbPath := *dbPathFlag
	if dbPath == "" {
		appDataDir, err := os.UserConfigDir()
		if err != nil {
			appDataDir = "."
		}
		dbPath = filepath.Join(appDataDir, "PrintBridge", "printbridge.db")
	}

	fmt.Printf("=== PrintBridge Agent v%s ===\n", Version)
	log.Printf("Initializing SQLite persistent queue at %s", dbPath)

	store, err := queue.NewStore(dbPath)
	if err != nil {
		log.Fatalf("Failed to initialize SQLite store: %v", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		log.Fatalf("Failed to initialize security auth manager: %v", err)
	}

	dash := dashboard.NewDashboard(store, authMgr)

	telMonitor := telemetry.NewMonitor(store, 30*time.Second)
	telMonitor.Start()
	defer telMonitor.Stop()

	up := updater.NewUpdater("", 24*time.Hour)
	up.StartPeriodicCheck(func() bool {
		pending, err := store.GetPendingJobs()
		return err == nil && len(pending) == 0
	})
	defer up.StopPeriodicCheck()

	q, err := queue.NewQueue(store)
	if err != nil {
		log.Fatalf("Failed to initialize queue worker: %v", err)
	}
	defer q.Stop()

	if *relayURLFlag != "" && *agentIDFlag != "" {
		log.Printf("Starting outbound Cloud Relay client for Agent ID: %s", *agentIDFlag)
		relayClient := relay.NewClient(*relayURLFlag, *agentIDFlag, *apiKeyFlag, q)
		relayClient.Start()
		defer relayClient.Stop()
	}

	serverAddr := fmt.Sprintf("localhost:%d", *portFlag)
	srv := server.NewServer(serverAddr, q, authMgr, telMonitor, dash, up,
		server.WithTLSCert(*tlsCertFlag, *tlsKeyFlag),
	)

	if err := srv.Start(); err != nil {
		log.Fatalf("Failed to start WebSocket server: %v", err)
	}
	defer srv.Stop()

	fmt.Printf("PrintBridge Agent running on ws://%s (Dashboard at http://%s/dashboard)\n", serverAddr, serverAddr)

	if *headlessFlag {
		fmt.Println("Running in HEADLESS mode (Press Ctrl+C to exit)")
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\nShutting down PrintBridge Agent cleanly...")
	} else {
		// Run System Tray App (blocking main thread as required by OS GUI event loops)
		t := tray.NewTray(q, *portFlag, up, func() {
			fmt.Println("\nShutting down PrintBridge Agent cleanly...")
		})
		t.Run()
	}
}
