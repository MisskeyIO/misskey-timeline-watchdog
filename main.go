package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Target struct {
		Domain string `yaml:"domain"`
		URL    string `yaml:"url"`
	} `yaml:"target"`
	Timeout  int    `yaml:"timeout"`  // Seconds
	Cooldown int    `yaml:"cooldown"` // Seconds
	Command  string `yaml:"command"`
	Sentry   struct {
		DSN string `yaml:"dsn"`
	} `yaml:"sentry"`
}

const (
	DefaultPath      = "/streaming"
	SubscribePayload = `{"type":"connect","body":{"channel":"globalTimeline","id":"1","params":{"withRenotes":true,"minimize":true}}}`

	DefaultConfigTemplate = `target:
  domain: '' # Required (e.g., misskey.io)
  # url: '' # Optional: Overrides domain if set (e.g., wss://misskey.io/streaming)
timeout: 10
cooldown: 300 # Seconds to wait before reconnecting after a failure
command: ./script.sh
sentry:
  dsn: '' # e.g. https://public@sentry.example.com/1
`
)

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func getTargetURL(cfg *Config) (string, error) {
	if cfg.Target.URL != "" {
		return cfg.Target.URL, nil
	}
	if cfg.Target.Domain != "" {
		cleanDomain := strings.TrimSuffix(strings.TrimPrefix(cfg.Target.Domain, "https://"), "/")
		return fmt.Sprintf("wss://%s%s", cleanDomain, DefaultPath), nil
	}
	return "", fmt.Errorf("target.domain or target.url must be specified in the configuration file")
}

func logPrintf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	log.Println(msg)

	// CHANGED: Use CaptureMessage instead of Breadcrumb
	sentry.CaptureMessage(msg)
}

func logFatalf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	log.Println("FATAL:", msg)

	sentry.CaptureMessage("FATAL: " + msg)
	sentry.Flush(5 * time.Second)

	os.Exit(1)
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		_ = os.WriteFile(*configPath, []byte(DefaultConfigTemplate), 0644)
		log.Fatalf("Configuration file not found. Created sample at: %s", *configPath)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	if cfg.Sentry.DSN != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.Sentry.DSN,
			TracesSampleRate: 1.0,
			AttachStacktrace: true,
		})
		if err != nil {
			log.Printf("Sentry initialization failed: %v", err)
		} else {
			logPrintf("Sentry initialized successfully.")
			defer sentry.Flush(2 * time.Second)
		}
	}

	targetURL, err := getTargetURL(cfg)
	if err != nil {
		logFatalf("Configuration Error: %v", err)
	}

	cooldownDuration := time.Duration(cfg.Cooldown) * time.Second
	if cfg.Cooldown <= 0 {
		cooldownDuration = 5 * time.Minute
	}

	logPrintf("Configuration Loaded. Target: %s, Timeout: %ds, Cooldown: %s", targetURL, cfg.Timeout, cooldownDuration)

	for {
		// A. Start Monitoring
		err := startMonitoringSession(targetURL, cfg)

		// B. Report Crash to Sentry (Error Level)
		logPrintf("Monitor session ended with error: %v", err)
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelError)
			sentry.CaptureException(err)
		})

		// C. Execute command
		logPrintf("Attempting to execute command...")
		executeCommandAndReport(cfg.Command)

		// D. Cooldown
		logPrintf(">>> Waiting %s before reconnecting...", cooldownDuration)
		sentry.Flush(5 * time.Second)

		time.Sleep(cooldownDuration)

		logPrintf(">>> Cooldown finished. Retrying connection...")
	}
}

func startMonitoringSession(url string, cfg *Config) error {
	logPrintf("Connecting to Misskey Streaming API...")

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage, []byte(SubscribePayload)); err != nil {
		return fmt.Errorf("subscribe request failed: %w", err)
	}

	logPrintf("Monitoring started (Listening for messages)...")

	timeoutDuration := time.Duration(cfg.Timeout) * time.Second

	for {
		if err := c.SetReadDeadline(time.Now().Add(timeoutDuration)); err != nil {
			return fmt.Errorf("failed to set read deadline: %w", err)
		}

		_, _, err := c.ReadMessage()
		if err != nil {
			return fmt.Errorf("read timeout or disconnection: %w", err)
		}
	}
}

func executeCommandAndReport(commandStr string) {
	parts := strings.Fields(commandStr)
	if len(parts) == 0 {
		logPrintf("Error: Recovery command string is empty")
		return
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)

	log.Printf("Command Output:\n%s", output)

	if err != nil {
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelFatal)
			scope.SetExtra("command_output", output)
			sentry.CaptureException(fmt.Errorf("command failed: %w", err))
		})

		log.Printf("command failed: %v", err)
	} else {
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelInfo)
			scope.SetExtra("command_output", output)
			sentry.CaptureMessage(fmt.Sprintf("command executed successfully: %s", parts[0]))
		})
		log.Println("command executed successfully.")
	}
}
