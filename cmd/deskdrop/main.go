package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
	"github.com/tvaroska/deskdrop/internal/driver/cdp"
)

var version = "v1.1.0"

func main() {
	unblockExecutableOnWindows()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func unblockExecutableOnWindows() {
	if runtime.GOOS != "windows" {
		return
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return
	}
	_ = os.Remove(exe + ":Zone.Identifier")
	_ = os.Remove(exe + ".old")
	dir := filepath.Dir(exe)
	_ = os.Remove(filepath.Join(dir, "deskdrop.json:Zone.Identifier"))
}

func run(args []string) error {
	if len(args) == 0 {
		if defaultShowsTrayOnNoArgs() {
			return cmdTray(nil)
		}
		printUsage()
		return nil
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage()
		return nil
	case "version", "--version":
		fmt.Printf("deskdrop %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "tray":
		return cmdTray(args[1:])
	case "recipes":
		return cmdRecipes(args[1:])
	case "open-browser":
		return cmdOpenBrowser(args[1:])
	case "tabs":
		return cmdTabs(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "sync":
		return cmdSync(args[1:])
	case "schedule":
		return cmdSchedule(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	default:
		return fmt.Errorf("unknown command %q (run 'deskdrop help')", args[0])
	}
}

func printUsage() {
	fmt.Println(`deskdrop — Zero-dependency desktop & browser declarative extraction CLI

Usage:
  deskdrop tray         [--interval 20s] [--fallback-gate auto|tray] [--cdp <url>] [--config <path>] [--console]
  deskdrop sync         [--interval 0s] [--max-tasks 0] [--fallback-gate auto|tray] [--quiet-cdp] [--hide-console] [--cdp <url>] [--config <path>]
  deskdrop schedule     <install|uninstall|status> [--every 15m] [--mode sync|logon] [--task-name DeskDropSync] [--dry-run]
  deskdrop run          <recipe-file.json|approved-name> [--var key=val ...] [--to <agents>] [--dry-run]
  deskdrop recipes      [--config <path>]
  deskdrop status       [--cdp <url>] [--config <path>]
  deskdrop tabs         [--cdp <url>] [--config <path>]
  deskdrop open-browser [--browser chrome|edge] [--port 9222] [--profile <dir>]
  deskdrop update       [--config <path>]
  deskdrop version`)
}

func validateFallbackGate(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return "auto", nil
	}
	if v != "auto" && v != "tray" {
		return "", fmt.Errorf("invalid --fallback-gate %q (expected 'auto' or 'tray')", raw)
	}
	return v, nil
}

func cmdTray(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	cdpURL := fs.String("cdp", "", "Explicit CDP ws:// or http:// URL")
	interval := fs.Duration("interval", 20*time.Second, "Background relay queue poll interval")
	fallbackGate := fs.String("fallback-gate", "", "Tier-2 fallback gate (auto|tray)")
	keepConsole := fs.Bool("console", false, "Keep console window visible on Windows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	gate := ""
	if *fallbackGate != "" {
		g, err := validateFallbackGate(*fallbackGate)
		if err != nil {
			return err
		}
		gate = g
	}
	return runPlatformTray(*cfgPath, *cdpURL, *interval, gate, *keepConsole)
}

type kvSlice map[string]string

func (m kvSlice) String() string { return "" }
func (m kvSlice) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	m[strings.TrimSpace(parts[0])] = parts[1]
	return nil
}

func cmdRecipes(args []string) error {
	fs := flag.NewFlagSet("recipes", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ServiceURL) != "" && strings.TrimSpace(cfg.ServiceToken) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		svc := bus.NewDesktopClient(cfg)
		svc.ClientVersion = version
		list, lerr := svc.ListApprovedRecipes(ctx)
		if lerr != nil {
			return lerr
		}
		for _, r := range list {
			fmt.Printf("%-26s [%s] %s\n", r.Name, r.Topic, r.Description)
		}
		if len(list) == 0 {
			fmt.Println("No approved recipes registered in relay RECIPES_URI.")
		}
		return nil
	}
	if strings.TrimSpace(cfg.RecipesURI) != "" {
		list, lerr := loadLocalRecipesFile(cfg.RecipesURI)
		if lerr != nil {
			return lerr
		}
		for _, r := range list {
			fmt.Printf("%-26s [%s] %s\n", r.Name, r.Topic, r.Description)
		}
		return nil
	}
	return errors.New("configure service_url + service_token or recipes_uri to list approved recipes")
}

func cmdOpenBrowser(args []string) error {
	fs := flag.NewFlagSet("open-browser", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	browser := fs.String("browser", "", "Browser binary preference (chrome or edge)")
	port := fs.Int("port", 9222, "Local CDP port")
	profile := fs.String("profile", "", "Dedicated user-data-dir path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _ := loadConfig(*cfgPath)
	b := *browser
	if b == "" {
		b = cfg.Browser
	}
	p := *profile
	if p == "" {
		p = cfg.UserDataDir
	}
	bin, dir, err := cdp.LaunchDedicatedBrowser(b, *port, p)
	if err != nil {
		return err
	}
	fmt.Printf("Launched %s on http://127.0.0.1:%d (profile: %s)\n", bin, *port, dir)
	return nil
}

func cmdTabs(args []string) error {
	fs := flag.NewFlagSet("tabs", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	cdpURL := fs.String("cdp", "", "Explicit CDP ws:// or http:// URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *cdpURL != "" {
		cfg.CDPURL = *cdpURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsURL, err := cdp.DiscoverBrowserWS(ctx, cfg.CDPURL, cfg.UserDataDir)
	if err != nil {
		return err
	}
	client, err := cdp.Connect(ctx, wsURL)
	if err != nil {
		return err
	}
	defer client.Close()
	client.SetPreferredSession(cfg.Browser, cfg.UserDataDir, cfg.PreferredChromeSession())

	pages, err := client.ListPages(ctx)
	if err != nil {
		return err
	}
	for i, p := range pages {
		fmt.Printf("[%d] %s\n    %s (targetId=%s, contextId=%s)\n", i+1, p.Title, p.URL, p.TargetID, p.BrowserContextID)
	}
	if len(pages) == 0 {
		fmt.Println("No open page tabs found.")
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	cdpURL := fs.String("cdp", "", "Explicit CDP ws:// or http:// URL")
	to := fs.String("to", "", "Target agent(s) on relay message board")
	dryRun := fs.Bool("dry-run", false, "Print extracted JSON to stdout without uploading")
	vars := make(kvSlice)
	fs.Var(vars, "var", "Template variable key=value (repeatable)")

	recipeArg, flagArgs, err := splitPositionalFirst(args)
	if err != nil {
		return errors.New("usage: deskdrop run <recipe-file.json|approved-name> [--var key=val] [--dry-run]")
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *cdpURL != "" {
		cfg.CDPURL = *cdpURL
	}

	rawRecipe, err := loadRecipeFromArgOrRelay(recipeArg, cfg)
	if err != nil {
		return err
	}
	resolved, err := contract.ResolveRecipe(rawRecipe, vars)
	if err != nil {
		return err
	}
	return executeRecipeAndDeliver(resolved, cfg, *to, *dryRun)
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	cdpURL := fs.String("cdp", "", "Explicit CDP ws:// or http:// URL")
	interval := fs.Duration("interval", 0, "Poll interval for continuous worker mode (0 = drain once)")
	maxTasks := fs.Int("max-tasks", 0, "Maximum tasks to execute before exiting (0 = unlimited)")
	fallbackGate := fs.String("fallback-gate", "", "Tier-2 fallback gate (auto|tray)")
	quietCDP := fs.Bool("quiet-cdp", false, "Never prompt Chrome 144+ DevToolsActivePort; exit 0 cleanly if browser is closed")
	hideConsole := fs.Bool("hide-console", false, "Hide console window on Windows (for Task Scheduler runs)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hideConsole {
		hideConsoleWindow()
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *cdpURL != "" {
		cfg.CDPURL = *cdpURL
	}
	if *fallbackGate != "" {
		g, gerr := validateFallbackGate(*fallbackGate)
		if gerr != nil {
			return gerr
		}
		cfg.FallbackGate = g
	}
	if strings.TrimSpace(cfg.ServiceURL) == "" {
		return errors.New("sync requires service_url and service_token in deskdrop.json or env")
	}

	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if err := svcClient.ValidateAuth(); err != nil {
		return err
	}

	hostLabel := resolveHostLabel(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool := cdp.NewSessionPool()
	defer pool.Close()
	drv := cdp.NewDriver(pool, cfg)
	approver := makeFallbackApprover(cfg.NormalizedFallbackGate())

	if *quietCDP {
		conn, _ := pool.ProbeQuiet(ctx, cfg.CDPURL)
		if !conn && *interval <= 0 {
			ws := collectWorkerStatus(ctx, drv, cfg, hostLabel, false, "scheduled-sync", "")
			_ = svcClient.ReportWorkerStatus(ctx, ws)
			fmt.Println("Browser not connected on quiet CDP endpoint; reported idle heartbeat.")
			return nil
		}
	} else {
		// Single startup attempt for explicit interactive CLI sync.
		_, _ = pool.Acquire(ctx, cfg.CDPURL, cfg.UserDataDir)
	}

	processed := 0
	for {
		drained, derr := drainQueue(ctx, drv, cfg, svcClient, hostLabel, *maxTasks-processed, *maxTasks > 0, approver)
		processed += drained
		if derr != nil {
			if errors.Is(derr, context.Canceled) {
				return nil
			}
			fmt.Fprintf(os.Stderr, "WARN sync cycle: %v\n", derr)
		}
		if *maxTasks > 0 && processed >= *maxTasks {
			fmt.Printf("Completed %d task(s) (reached --max-tasks).\n", processed)
			return nil
		}
		if *interval <= 0 {
			if processed == 0 {
				fmt.Println("No pending desktop tasks in queue.")
			} else {
				fmt.Printf("Drained %d desktop task(s).\n", processed)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	cdpURL := fs.String("cdp", "", "Explicit CDP ws:// or http:// URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, cfgErr := loadConfig(*cfgPath)
	if *cdpURL != "" {
		cfg.CDPURL = *cdpURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := cdp.NewSessionPool()
	defer pool.Close()
	_, _ = pool.Acquire(ctx, cfg.CDPURL, cfg.UserDataDir)
	drv := cdp.NewDriver(pool, cfg)

	lastErr := ""
	if cfgErr != nil {
		lastErr = cfgErr.Error()
	}
	ws := collectWorkerStatus(ctx, drv, cfg, resolveHostLabel(cfg), false, "status", lastErr)
	fmt.Printf("Local Client:      deskdrop %s (%s/%s)\n", ws.Version, ws.OS, ws.Arch)
	fmt.Printf("Host Label:        %s (hostname=%s)\n", ws.Host, ws.Hostname)
	fmt.Printf("Fallback Gate:     %s\n", ws.FallbackGate)
	if cfgErr != nil {
		fmt.Printf("Config Status:     ERROR: %v\n", cfgErr)
	} else {
		fmt.Printf("Config Status:     OK (service_url=%s)\n", cfg.ServiceURL)
	}
	fmt.Printf("Chrome Session:    %q\n", ws.ChromeSession)
	fmt.Printf("Browser Connected: %v (%s)\n", ws.BrowserConnected, ws.BrowserStatus)
	fmt.Printf("Ready To Accept:   %v\n", ws.ReadyToAccept)

	if strings.TrimSpace(cfg.ServiceURL) != "" && strings.TrimSpace(cfg.ServiceToken) != "" {
		svcClient := bus.NewDesktopClient(cfg)
		svcClient.ClientVersion = version
		_ = svcClient.ReportWorkerStatus(ctx, ws)
		remote, rerr := svcClient.GetDesktopStatus(ctx)
		if rerr != nil {
			fmt.Printf("Relay Backend:     ERROR: %v\n", rerr)
		} else {
			raw, _ := json.MarshalIndent(remote, "", "  ")
			fmt.Printf("\nRelay Backend Status (%s):\n%s\n", cfg.ServiceURL, string(raw))
		}
	}
	return cfgErr
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Optional config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if err := svcClient.ValidateAuth(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	newVer, err := performSelfUpdate(ctx, svcClient)
	if err != nil {
		return err
	}
	fmt.Printf("Updated deskdrop from %s -> %s.\n", version, newVer)
	return nil
}

func executeRecipeAndDeliver(recipe contract.Recipe, cfg contract.Config, to string, dryRun bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	drv := cdp.NewDriver(nil, cfg)
	defer drv.Close()

	hostname, _ := os.Hostname()
	report := contract.DropReport{
		SchemaVersion: "2.0",
		Mode:          contract.DropModeDeterministic,
		Recipe:        recipe.Name,
		Topic:         recipe.Topic,
		Host:          hostname,
		CapturedAtUTC: time.Now().UTC().Format(time.RFC3339),
	}

	transport := bus.NewTransport(cfg)
	for _, step := range recipe.Steps {
		res, stepErr := drv.ExecuteStep(ctx, step)
		if stepErr != nil {
			var execErr *driver.StepExecError
			if errors.As(stepErr, &execErr) {
				zipBytes, zerr := driver.BuildDiagnosticZip(recipe.Name, execErr, cfg.ForbiddenTerms)
				if zerr == nil {
					if savedPath, serr := driver.SaveDiagnosticZip(cfg.DebugDir, recipe.Topic, zipBytes); serr == nil {
						fmt.Fprintf(os.Stderr, "Saved diagnostic bundle: %s\n", savedPath)
					}
					if !dryRun && cfg.ShouldSendErrorBundle() && transport.ValidateAuth() == nil {
						meta := contract.FailReportMetadata{
							From:      cfg.From,
							To:        to,
							Topic:     recipe.Topic,
							Recipe:    recipe.Name,
							StepID:    step.ID,
							LandedURL: execErr.Partial.URL,
							Domain:    contract.InferRecipeDomain(recipe),
							Error:     execErr.Error(),
						}
						if sent, berr := transport.SendErrorWithContext(ctx, to, meta, zipBytes); berr == nil {
							fmt.Fprintf(os.Stderr, "Escalated error bundle (%s, ref=%s)\n", sent.ShortID, sent.Attachment)
						}
					}
				}
			}
			return stepErr
		}
		report.Steps = append(report.Steps, res)
	}

	pretty, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := contract.EnforceAirGap(string(pretty), cfg.ForbiddenTerms); err != nil {
		return err
	}
	if dryRun {
		fmt.Println(string(pretty))
		return nil
	}

	sent, err := transport.SendReport(ctx, to, report)
	if err != nil {
		return err
	}
	fmt.Printf("Uploaded drop to relay [bus:%s] (drop_id=%s, message_id=%s)\n", sent.ShortID, sent.Attachment, sent.MessageID)
	return nil
}

func loadRecipeFromArgOrRelay(arg string, cfg contract.Config) (contract.Recipe, error) {
	if st, err := os.Stat(arg); err == nil && !st.IsDir() {
		raw, rerr := os.ReadFile(arg)
		if rerr != nil {
			return contract.Recipe{}, rerr
		}
		var r contract.Recipe
		if perr := contract.ParseJSONStrict(raw, &r); perr != nil {
			return contract.Recipe{}, fmt.Errorf("invalid recipe file %s: %w", arg, perr)
		}
		return r, nil
	}
	if strings.TrimSpace(cfg.RecipesURI) != "" {
		if list, err := loadLocalRecipesFile(cfg.RecipesURI); err == nil {
			for _, r := range list {
				if strings.EqualFold(r.Name, arg) {
					return r, nil
				}
			}
		}
	}
	if strings.TrimSpace(cfg.ServiceURL) != "" && strings.TrimSpace(cfg.ServiceToken) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		svc := bus.NewDesktopClient(cfg)
		svc.ClientVersion = version
		list, err := svc.ListApprovedRecipes(ctx)
		if err == nil {
			for _, r := range list {
				if strings.EqualFold(r.Name, arg) {
					return r, nil
				}
			}
		}
	}
	return contract.Recipe{}, fmt.Errorf("recipe %q not found locally or in relay RECIPES_URI", arg)
}

func loadLocalRecipesFile(path string) ([]contract.Recipe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bundle struct {
		SchemaVersion string            `json:"schema_version,omitempty"`
		UpdatedAtUTC  string            `json:"updated_at_utc,omitempty"`
		Recipes       []contract.Recipe `json:"recipes"`
	}
	if err := contract.ParseJSONStrict(raw, &bundle); err == nil && len(bundle.Recipes) > 0 {
		return bundle.Recipes, nil
	}
	var list []contract.Recipe
	if err := contract.ParseJSONStrict(raw, &list); err == nil {
		return list, nil
	}
	var single contract.Recipe
	if err := contract.ParseJSONStrict(raw, &single); err != nil {
		return nil, err
	}
	return []contract.Recipe{single}, nil
}

func loadConfig(explicitPath string) (contract.Config, error) {
	var cfg contract.Config
	paths := candidateConfigPaths(explicitPath)
	for _, p := range paths {
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			if explicitPath != "" && p == explicitPath {
				return cfg, err
			}
			continue
		}
		hadDup, perr := contract.ParseConfigJSON(raw, &cfg)
		if perr != nil {
			return cfg, fmt.Errorf("failed parsing config %s: %w", p, perr)
		}
		if hadDup {
			_ = saveConfigChromeSession(p, cfg.PreferredChromeSession())
		}
		break
	}

	applyEnv(&cfg.ServiceURL, "DESKDROP_SERVICE_URL", "BROWSER_DROP_SERVICE_URL")
	applyEnv(&cfg.ServiceToken, "DESKDROP_TOKEN", "BROWSER_DROP_TOKEN")
	applyEnv(&cfg.From, "DESKDROP_FROM", "AGENT_BUS_FROM")
	applyEnv(&cfg.DefaultTo, "DESKDROP_TO", "AGENT_BUS_TO")
	applyEnv(&cfg.CDPURL, "DESKDROP_CDP_URL", "BROWSER_DROP_CDP_URL")
	applyEnv(&cfg.ChromeSession, "DESKDROP_CHROME_SESSION", "BROWSER_DROP_CHROME_SESSION")
	applyEnv(&cfg.RecipesURI, "DESKDROP_RECIPES_URI", "RECIPES_URI")
	applyEnv(&cfg.FallbackGate, "DESKDROP_FALLBACK_GATE", "")
	return cfg, nil
}

func applyEnv(dst *string, primary string, legacy string) {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		*dst = v
		return
	}
	if legacy != "" {
		if v := strings.TrimSpace(os.Getenv(legacy)); v != "" {
			*dst = v
		}
	}
}

func saveConfigChromeSession(explicitPath string, sessionName string) error {
	paths := candidateConfigPaths(explicitPath)
	var targetPath string
	var rawMap map[string]any
	for _, p := range paths {
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m != nil {
			targetPath = p
			rawMap = m
			break
		}
	}
	if targetPath == "" {
		targetPath = "deskdrop.json"
		rawMap = make(map[string]any)
	}
	delete(rawMap, "chrome-session")
	delete(rawMap, "chrome_profile")
	delete(rawMap, "chrome-profile")
	rawMap["chrome_session"] = strings.TrimSpace(sessionName)
	encoded, err := json.MarshalIndent(rawMap, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(targetPath, encoded, 0o600)
}

func candidateConfigPaths(explicit string) []string {
	if strings.TrimSpace(explicit) != "" {
		return []string{explicit}
	}
	var paths []string
	paths = append(paths, "deskdrop.local.json", "deskdrop.json", "browser-drop.json")
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths,
			filepath.Join(dir, "deskdrop.local.json"),
			filepath.Join(dir, "deskdrop.json"),
			filepath.Join(dir, "browser-drop.json"),
		)
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		paths = append(paths, filepath.Join(appData, "deskdrop", "config.json"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".config", "deskdrop", "config.json"))
	}
	return paths
}

func resolveHostLabel(cfg contract.Config) string {
	if strings.TrimSpace(cfg.From) != "" {
		return strings.TrimSpace(cfg.From)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "desktop-worker"
}

func splitPositionalFirst(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, errors.New("missing argument")
	}
	if !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:], nil
	}
	boolFlags := map[string]bool{"--dry-run": true, "-dry-run": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && !boolFlags[a] && i+1 < len(args) {
				i++
			}
			continue
		}
		rest := append([]string{}, args[:i]...)
		rest = append(rest, args[i+1:]...)
		return a, rest, nil
	}
	return "", nil, errors.New("missing target argument")
}
