// Command nettact-server is the self-hosted server: a single binary that
// wires the server-core modules over one SQLite database (architecture §7). It
// is a thin wrapper over the server runtime package — it parses flags,
// installs signal handling, and calls server.Start/Shutdown. All
// orchestration (DB, admin bootstrap, services, workers, listener) lives in
// server, shared with the desktop all-in-one build.
//
// The `passwd` subcommand (nettact-server passwd -db <path>) resets the admin
// password out of band for lost-password recovery, reading the new password
// interactively so it never reaches the shell history or process list.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	// Embedded IANA timezone database. Notifications render their timestamps in
	// this process's local zone, which Go resolves from $TZ against the system
	// zoneinfo files — and the runtime image ships none, so without this a
	// deployment that sets TZ=Asia/Shanghai would silently fall back to UTC and
	// announce outages eight hours off. The embedded copy is only a fallback: a
	// host that does provide zoneinfo (bind mount, distro package) still wins.
	_ "time/tzdata"

	"golang.org/x/term"

	"github.com/nettact/server-core/identity"
	"github.com/nettact/server-core/store"
	"github.com/nettact/server/internal/version"
	"github.com/nettact/server/server"
)

// maxAgents caps enrolled agents. Lite targets a home/SMB fleet well under this,
// and the ceiling is a product decision rather than a deployment knob, so it is
// fixed here instead of being exposed as a flag.
const maxAgents = 50

func main() {
	// Subcommand dispatch before flag parsing: `passwd` runs its own FlagSet and
	// exits without touching the server flag surface.
	if len(os.Args) > 1 && os.Args[1] == "passwd" {
		runPasswd(os.Args[2:])
		return
	}

	addr := flag.String("addr", ":12450", "listen address (a listen address saved in the web console overrides this flag)")
	dbPath := flag.String("db", "./nettact.db", "SQLite database path")
	webuiDir := flag.String("webui-dir", "", "web console download/install directory (default: <db dir>/webui)")
	dev := flag.Bool("dev", false, "dev mode: open CORS for the Vite origin, non-Secure cookie")
	adminUser := flag.String("admin-user", "", "optional; first run only; if omitted an initial password is generated and printed")
	adminPass := flag.String("admin-pass", "", "optional; first run only; if omitted an initial password is generated and printed")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate; with -tls-key serves HTTPS/WSS natively")
	tlsKey := flag.String("tls-key", "", "path to TLS private key; with -tls-cert serves HTTPS/WSS natively")
	// Browsers only send Secure cookies over HTTPS, so a Secure session cookie on a
	// plain-HTTP deployment makes every login silently fail (except on localhost).
	// auto ties the flag to how THIS process serves; true is for TLS-terminating
	// reverse proxies (browser sees https, we serve http).
	secureCookie := flag.String("secure-cookie", "auto",
		"session cookie Secure attribute: auto (set iff TLS is enabled), true (always; use behind a TLS-terminating reverse proxy), false (never)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("nettact-server", version.Version)
		return
	}

	addrFromFlag := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrFromFlag = true
		}
	})

	// TLS is all-or-nothing: with only one flag set (a missing or mistyped mounted
	// secret), silently falling back to plaintext would expose bearer tokens and
	// telemetry. The library validates this again; catching it here gives a
	// friendly message instead of a wrapped error.
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be provided together")
	}

	useTLS := *tlsCert != "" && *tlsKey != ""
	var secure bool
	switch *secureCookie {
	case "auto":
		secure = useTLS
	case "true":
		secure = true
	case "false":
		secure = false
	default:
		log.Fatalf("-secure-cookie must be auto, true or false (got %q)", *secureCookie)
	}
	if *dev {
		secure = false // dev mode always serves the Vite origin over plain http
	}

	srv, err := server.Start(context.Background(), server.Config{
		Addr:         *addr,
		AddrFromFlag: addrFromFlag,
		TLSCert:      *tlsCert,
		TLSKey:       *tlsKey,
		DBPath:       *dbPath,
		WebUIDir:     *webuiDir,
		AdminUser:    *adminUser,
		AdminPass:    *adminPass,
		Dev:          *dev,
		SecureCookie: secure,
		MaxAgents:    maxAgents,
		// Retention stays zero: server fills in the standard windows.
		// Desktop stays nil: standalone Lite exposes no one-time-login endpoint and
		// keeps the default -addr :12450 bind.
	})
	if err != nil {
		log.Fatalf("start: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
		log.Println("shutting down…")
	case err := <-srv.Err():
		log.Printf("serve: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// runPasswd implements `nettact-server passwd -db <path>`: it reads a new password
// interactively (never via a flag, so it stays out of the shell history and the
// process list), then resets the single admin's password directly in the
// database and invalidates every existing session.
func runPasswd(args []string) {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	dbPath := fs.String("db", "./nettact.db", "SQLite database path")
	_ = fs.Parse(args)

	newPass, err := readNewPassword()
	if err != nil {
		log.Fatalf("passwd: %v", err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("passwd: open db %s: %v", *dbPath, err)
	}
	defer db.Close()

	username, err := identity.New(db).ResetAdminPassword(context.Background(), newPass)
	if err != nil {
		log.Fatalf("passwd: %v", err)
	}
	fmt.Printf("password for user %q reset; all sessions have been logged out\n", username)
	fmt.Println("if the server is currently running, restart it so the change takes full effect.")
}

// readNewPassword reads and confirms the new password. On an interactive
// terminal it disables echo (x/term) and reads twice, requiring the entries to
// match; when stdin is piped it reads a single line. The password policy is
// enforced later by ResetAdminPassword.
func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Print("New password: ")
		first, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		fmt.Print("Confirm new password: ")
		second, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if string(first) != string(second) {
			return "", errors.New("passwords do not match")
		}
		return string(first), nil
	}
	// Non-interactive (piped) input: read a single line and strip the newline.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
