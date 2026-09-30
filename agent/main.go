package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/auth"
	"github.com/printbridge/printbridge/agent/pkg/config"
	"github.com/printbridge/printbridge/agent/pkg/dashboard"
	"github.com/printbridge/printbridge/agent/pkg/printer"
	"github.com/printbridge/printbridge/agent/pkg/queue"
	"github.com/printbridge/printbridge/agent/pkg/relay"
	"github.com/printbridge/printbridge/agent/pkg/roles"
	"github.com/printbridge/printbridge/agent/pkg/server"
	"github.com/printbridge/printbridge/agent/pkg/service"
	"github.com/printbridge/printbridge/agent/pkg/telemetry"
	"github.com/printbridge/printbridge/agent/pkg/tray"
	"github.com/printbridge/printbridge/agent/pkg/updater"
)

const Version = "0.1.0"

// stringListFlag collects a repeatable string flag (e.g. -allow-origin A -allow-origin B).
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }

func (s *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value != "" {
		*s = append(*s, value)
	}
	return nil
}

func main() {
	portFlag := flag.Int("port", 9567, "Local WebSocket server & Dashboard port")
	dbPathFlag := flag.String("db", "", "Path to SQLite database file")
	configFlag := flag.String("config", "", "Path to config.json (default: next to the database)")
	serviceFlag := flag.String("service", "", "Service control: install, uninstall, start, stop, status, repair")
	headlessFlag := flag.Bool("headless", false, "Run without system tray icon UI")
	autoUpdateFlag := flag.Bool("auto-update", false, "Download and apply agent updates automatically (off by default)")
	relayURLFlag := flag.String("relay-url", "", "Cloudflare Relay URL (e.g. wss://relay.printbridge.dev)")
	agentIDFlag := flag.String("agent-id", "", "Registered Agent ID for Cloud Relay")
	apiKeyFlag := flag.String("api-key", "", "API key / Secret for Cloud Relay authentication")
	tlsCertFlag := flag.String("tls-cert", "", "Path to TLS certificate file for WSS/HTTPS")
	tlsKeyFlag := flag.String("tls-key", "", "Path to TLS private key file for WSS/HTTPS")
	allowOriginFlag := stringListFlag{}
	flag.Var(&allowOriginFlag, "allow-origin", "Additional browser origin allowed to connect (repeatable). Loopback origins are always allowed.")
	trustProxyFlag := flag.Bool("trust-proxy", false, "Trust X-Forwarded-For when attributing client IPs (only behind a local reverse proxy)")
	flag.Parse()

	// Which flags the operator actually typed. Everything else comes from the file, so
	// config.json is a real configuration source rather than documentation.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	fmt.Printf("=== PrintBridge Agent v%s ===\n", Version)

	// Service control verbs run before anything touches the database or the config, so
	// `-service install` works even when the config file is unreadable or not yet
	// created for this user.
	serviceAction := service.Action("")
	if raw := strings.TrimSpace(*serviceFlag); raw != "" {
		parsed, err := service.ParseAction(raw)
		if err != nil {
			exitWithError(err)
		}
		if parsed != service.ActionRun {
			if err := runServiceAction(parsed, *configFlag); err != nil {
				exitWithError(err)
			}
			return
		}
		serviceAction = parsed
	}

	serviceMode := serviceAction == service.ActionRun
	if serviceMode {
		// Open the log file before anything else, so the startup lines that explain a
		// failure are captured as well.
		defer setupLogging(defaultDataDir(true))()
	}

	settings := resolveSettings(settingsInput{
		explicit:     explicit,
		port:         *portFlag,
		dbPath:       *dbPathFlag,
		configPath:   *configFlag,
		tlsCert:      *tlsCertFlag,
		tlsKey:       *tlsKeyFlag,
		allowOrigins: allowOriginFlag,
		trustProxy:   *trustProxyFlag,
		headless:     *headlessFlag,
		autoUpdate:   *autoUpdateFlag,
		relayURL:     *relayURLFlag,
		agentID:      *agentIDFlag,
		apiKey:       *apiKeyFlag,
		serviceMode:  serviceMode,
	})

	// Launched by the Service Control Manager: let it own the lifecycle, and run
	// headless because a service has no desktop to put a tray icon on.
	if serviceMode {
		settings.Headless = true
		log.Printf("Running as the %s service", service.Name)
		if err := service.Run(func(serviceStop <-chan struct{}) error {
			return runAgent(settings, serviceStop)
		}); err != nil {
			log.Printf("Service dispatcher failed: %v", err)
			os.Exit(1)
		}
		return
	}

	stop := make(chan struct{})
	if settings.Headless {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			close(stop)
		}()
	}

	if err := runAgent(settings, stop); err != nil {
		log.Printf("Agent stopped: %v", err)
		os.Exit(1)
	}
}

// runAgent starts every subsystem and returns once stop is closed (or the tray is
// quit in interactive mode). Teardown runs through the deferred calls.
func runAgent(settings agentSettings, stop <-chan struct{}) error {
	log.Printf("Initializing SQLite persistent queue at %s", settings.DBPath)
	store, err := queue.NewStore(settings.DBPath)
	if err != nil {
		return fmt.Errorf("failed to initialize SQLite store: %w", err)
	}
	defer store.Close()

	authMgr, err := auth.NewManager(store)
	if err != nil {
		return fmt.Errorf("failed to initialize security auth manager: %w", err)
	}

	dash := dashboard.NewDashboard(store, authMgr)

	telMonitor := telemetry.NewMonitor(store, 30*time.Second)
	telMonitor.Start()
	defer telMonitor.Stop()

	up := updater.NewUpdater(settings.ManifestURL, 24*time.Hour)

	// Self-update replaces the running binary, so it is opt-in: an unattended till is
	// updated by an installer, not by itself.
	if settings.AutoUpdate {
		log.Println("[Updater] Automatic updates are enabled")
		up.StartPeriodicCheck(func() bool {
			pending, err := store.GetPendingJobs()
			return err == nil && len(pending) == 0
		})
		defer up.StopPeriodicCheck()
	}

	// Network printers — queues whose spooler port is a TCP address — are written to
	// directly over their RAW port, which removes the spooler (and its stale offline
	// flags) from the path. Everything else keeps going through the OS print queue,
	// and a network connection that cannot be established at all falls back to it.
	router := printer.NewRouter(printer.ListPrintersCached, 10*time.Second)

	q, err := queue.NewQueue(store,
		queue.WithPrintFunc(router.Print),
		queue.WithMaxAttempts(settings.QueueMaxAttempts),
		queue.WithMaxDepth(settings.QueueMaxDepth),
		queue.WithMaxConcurrentJobs(settings.QueueMaxConcurrent),
	)
	if err != nil {
		return fmt.Errorf("failed to initialize queue worker: %w", err)
	}
	defer q.Stop()

	if settings.RelayURL != "" && settings.AgentID != "" {
		log.Printf("Starting outbound Cloud Relay client for Agent ID: %s", settings.AgentID)
		relayClient := relay.NewClient(settings.RelayURL, settings.AgentID, settings.APIKey, q)
		relayClient.SetVersion(Version)
		relayClient.Start()
		defer relayClient.Stop()
	}

	rolesMgr := roles.NewManager(store)

	serverAddr := fmt.Sprintf("localhost:%d", settings.Port)
	srv := server.NewServer(serverAddr, q, authMgr, telMonitor, dash, up,
		server.WithTLSCert(settings.TLSCertFile, settings.TLSKeyFile),
		server.WithAllowedOrigins(settings.AllowedOrigins...),
		server.WithTrustedProxy(settings.TrustedProxy),
		server.WithAgentVersion(Version),
		server.WithRoleManager(rolesMgr),
	)

	if err := srv.Start(); err != nil {
		return fmt.Errorf("failed to start WebSocket server: %w", err)
	}
	defer srv.Stop()

	fmt.Printf("PrintBridge Agent running on ws://%s (Dashboard at http://%s/dashboard)\n", serverAddr, serverAddr)
	log.Printf("Allowed browser origins: %s", strings.Join(srv.AllowedOrigins(), ", "))

	if settings.Headless {
		fmt.Println("Running in HEADLESS mode (Press Ctrl+C to exit)")
		<-stop
		fmt.Println("\nShutting down PrintBridge Agent cleanly...")
		return nil
	}

	// The system tray owns the main thread (an OS GUI requirement) and blocks here
	// until the operator quits it.
	t := tray.NewTray(q, settings.Port, up, func() {
		fmt.Println("\nShutting down PrintBridge Agent cleanly...")
	})
	t.Run()
	return nil
}

// Log file settings for service mode.
const (
	logFilename     = "printbridge.log"
	maxLogSizeBytes = 5 << 20 // 5 MB
)

// setupLogging tees the agent's log into a file next to its data.
//
// A Windows service has no console, so without this the lines that explain a failure
// are written nowhere anyone can read. The file is kept bounded by rotating once at
// maxLogSizeBytes: a till runs for years and nobody rotates log files by hand.
func setupLogging(dataDir string) func() {
	path := filepath.Join(dataDir, logFilename)

	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSizeBytes {
		_ = os.Rename(path, path+".1")
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[Logging] Could not open %s: %v; logging to stderr only", path, err)
		return func() {}
	}

	log.SetOutput(io.MultiWriter(os.Stderr, file))
	log.Printf("[Logging] Also writing to %s", path)

	return func() { _ = file.Close() }
}

// exitWithError prints a failure and exits non-zero, so scripts and installers can
// detect it.
func exitWithError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

// runServiceAction performs a service control verb.
func runServiceAction(action service.Action, configPath string) error {
	switch action {
	case service.ActionInstall:
		if err := service.Install("", service.RunArgs(configPath)); err != nil {
			return err
		}
		fmt.Printf("Installed the %s service (%s).\n", service.DisplayName, service.Name)
		fmt.Println("It starts automatically, depends on the print spooler, and restarts itself if it fails.")
		return nil

	case service.ActionUninstall:
		if err := service.Uninstall(); err != nil {
			return err
		}
		fmt.Println("Removed the PrintBridge service. The database and config file were left in place.")
		return nil

	case service.ActionStart:
		if err := service.Start(); err != nil {
			return err
		}
		fmt.Println("Service start requested")
		return nil

	case service.ActionStop:
		if err := service.Stop(); err != nil {
			return err
		}
		fmt.Println("Service stopped")
		return nil

	case service.ActionStatus:
		status, err := service.Query()
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", service.Name, status)
		return nil

	default:
		return fmt.Errorf("unhandled service action %q", action)
	}
}

// agentSettings is the effective configuration: the file, with flags layered on top.
type agentSettings struct {
	Port               int
	DBPath             string
	ConfigPath         string
	TLSCertFile        string
	TLSKeyFile         string
	AllowedOrigins     []string
	TrustedProxy       bool
	Headless           bool
	AutoUpdate         bool
	ManifestURL        string
	RelayURL           string
	AgentID            string
	APIKey             string
	QueueMaxAttempts   int
	QueueMaxDepth      int
	QueueMaxConcurrent int
}

// settingsInput carries the parsed flags and which of them the operator typed.
type settingsInput struct {
	explicit     map[string]bool
	port         int
	dbPath       string
	configPath   string
	tlsCert      string
	tlsKey       string
	allowOrigins []string
	trustProxy   bool
	headless     bool
	autoUpdate   bool
	relayURL     string
	agentID      string
	apiKey       string
	// serviceMode is set when the Service Control Manager launched us, which changes
	// where the database and config live.
	serviceMode bool
}

// defaultDataDir is where a Windows service keeps its state.
//
// A service runs as LocalSystem, whose per-user config directory is the system
// profile — not a place anyone looks for a till's database. %ProgramData% is the
// documented location for service data, and it is also what an installer can
// pre-create.
func defaultDataDir(serviceMode bool) string {
	if serviceMode {
		if programData := strings.TrimSpace(os.Getenv("ProgramData")); programData != "" {
			return filepath.Join(programData, "PrintBridge")
		}
		return filepath.Join(`C:\ProgramData`, "PrintBridge")
	}

	appDataDir, err := os.UserConfigDir()
	if err != nil {
		appDataDir = "."
	}
	return filepath.Join(appDataDir, "PrintBridge")
}

// resolveSettings loads the configuration file and merges the flags over it: a flag
// the operator did not type never overrides the file.
func resolveSettings(in settingsInput) agentSettings {
	dataDir := defaultDataDir(in.serviceMode)

	dbPath := strings.TrimSpace(in.dbPath)
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "printbridge.db")
	}

	configPath := strings.TrimSpace(in.configPath)
	if configPath == "" {
		// An explicit database path also says where the configuration belongs, which is
		// what an operator passing -db expects.
		configDir := dataDir
		if strings.TrimSpace(in.dbPath) != "" {
			configDir = filepath.Dir(dbPath)
		}
		configPath = config.DefaultPath(configDir)
	}

	cfg, result := config.Load(configPath)
	if result.Warning != "" {
		log.Printf("[Config] %s", result.Warning)
	} else if result.Path != "" {
		log.Printf("[Config] %s: %s", result.Source, result.Path)
	}
	for _, note := range result.Notes {
		log.Printf("[Config] %s", note)
	}

	settings := agentSettings{
		Port:               cfg.Server.Port,
		DBPath:             dbPath,
		ConfigPath:         configPath,
		TLSCertFile:        cfg.Server.TLSCertFile,
		TLSKeyFile:         cfg.Server.TLSKeyFile,
		TrustedProxy:       cfg.Security.TrustedProxy,
		AutoUpdate:         cfg.Updater.AutoUpdate,
		ManifestURL:        cfg.Updater.ManifestURL,
		RelayURL:           cfg.Relay.URL,
		AgentID:            cfg.Relay.AgentID,
		APIKey:             cfg.Relay.APIKey,
		QueueMaxAttempts:   cfg.Queue.MaxAttempts,
		QueueMaxDepth:      cfg.Queue.MaxDepth,
		QueueMaxConcurrent: cfg.Queue.MaxConcurrentJobs,
		Headless:           in.headless,
	}

	if in.explicit["port"] {
		settings.Port = in.port
	}
	if in.explicit["tls-cert"] {
		settings.TLSCertFile = in.tlsCert
	}
	if in.explicit["tls-key"] {
		settings.TLSKeyFile = in.tlsKey
	}
	if in.explicit["trust-proxy"] {
		settings.TrustedProxy = in.trustProxy
	}
	if in.explicit["auto-update"] {
		settings.AutoUpdate = in.autoUpdate
	}
	if in.explicit["relay-url"] {
		settings.RelayURL = in.relayURL
	}
	if in.explicit["agent-id"] {
		settings.AgentID = in.agentID
	}
	if in.explicit["api-key"] {
		settings.APIKey = in.apiKey
	}

	// Origins accumulate: the file holds stable ones, flags add ad-hoc ones.
	settings.AllowedOrigins = append(settings.AllowedOrigins, in.allowOrigins...)

	return settings
}
