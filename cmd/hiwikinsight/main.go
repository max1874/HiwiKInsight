// Command hiwikinsight is the HiwiKInsight server.
//
//	hiwikinsight serve [-config config.yaml]   run the server (default command)
//	hiwikinsight demo [-data ./demo-data]      run a demo with generated data (user demo / password demo)
//	hiwikinsight check-config [-config ...]    validate a config file and print the effective settings
//	hiwikinsight hash-password                 hash a dashboard password for config.yaml
//	hiwikinsight version
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/RedHiwiK/HiwiKInsight/internal/auth"
	"github.com/RedHiwiK/HiwiKInsight/internal/config"
	"github.com/RedHiwiK/HiwiKInsight/internal/demo"
	"github.com/RedHiwiK/HiwiKInsight/internal/server"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func init() {
	// Without ldflags (go install module@version), fall back to the module version Go records.
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

const usage = `HiwiKInsight: self-hosted analytics and App Store revenue for indie apps.

Usage:
  hiwikinsight serve [-config PATH]      run the server (default; config defaults to $HIWIKINSIGHT_CONFIG or ./config.yaml)
  hiwikinsight demo [-data DIR] [-listen ADDR]
                                         run a demo with three sample apps and 90 days of generated data;
                                         sign in with demo / demo
  hiwikinsight check-config [-config PATH]
                                         validate a config file
  hiwikinsight hash-password             hash a dashboard password for dashboard.users[].password_hash
  hiwikinsight version
`

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "demo":
		err = runDemo(args)
	case "check-config":
		err = checkConfig(args)
	case "hash-password":
		err = hashPassword()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("HIWIKINSIGHT_CONFIG")
	if def == "" {
		def = "config.yaml"
	}
	return fs.String("config", def, "path to config.yaml")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	return run(cfg)
}

func run(cfg *config.Config) error {
	s, err := server.New(cfg)
	if err != nil {
		return err
	}
	s.Start()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("HiwiKInsight " + version)
	return s.ListenAndServe(ctx)
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dir := fs.String("data", "./demo-data", "directory for the demo database, config and catalogs")
	listen := fs.String("listen", ":8080", "listen address")
	fs.Parse(args)
	cfg, err := demo.Prepare(*dir, *listen)
	if err != nil {
		return err
	}
	slog.Info("demo ready: open http://localhost" + portOf(*listen) + "/dashboard/ and sign in with demo / demo")
	return run(cfg)
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ""
}

func checkConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("config OK: %d apps, timezone %s, currency %s, language %s, database %s\n",
		len(cfg.Apps), cfg.Timezone, cfg.Currency, cfg.Language, cfg.DBPath())
	if cfg.PublicURL != "" {
		fmt.Printf("public URL: %s (SDK endpoint; App Store notifications at %s/v1/appstore/notifications)\n",
			cfg.PublicURL, strings.TrimRight(cfg.PublicURL, "/"))
	}
	fmt.Printf("dashboard users: %d, query tokens: %d, mail: %v, Feishu: %v, App Store Connect: %v\n",
		len(cfg.Dashboard.Users), len(cfg.Query.Tokens), cfg.Mail.Enabled(), cfg.Feishu.Enabled(), cfg.ASC.Enabled())
	for _, a := range cfg.Apps {
		catalog := a.Catalog
		if catalog == "" {
			catalog = "(none)"
		} else if _, err := os.Stat(catalog); err != nil {
			return fmt.Errorf("app %s: catalog %s: %w", a.Key, catalog, err)
		}
		fmt.Printf("  - %s (%s) %s, catalog %s\n", a.Key, a.BundleID, a.Name, catalog)
	}
	return nil
}

func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		pw = string(b)
	} else {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	if len(pw) < 8 {
		return fmt.Errorf("use at least 8 characters")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}
