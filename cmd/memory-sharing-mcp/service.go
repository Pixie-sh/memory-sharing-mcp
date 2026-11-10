package main

import (
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// serviceLabel names the LaunchAgent or systemd user unit that runs http serve.
const serviceLabel = "sh.pixie.memory-sharing-mcp"

// install copies this executable into a bin directory and runs http serve as a
// macOS LaunchAgent or a Linux systemd user unit that starts at login and
// restarts if it exits.
func install(args []string, cfgPath string, cfg config, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("install", flag.ContinueOnError)
	fset.SetOutput(stderr)
	binDir := fset.String("bin-dir", "~/.local/bin", "directory on PATH to install the binary into")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintln(stderr, "install takes no positional arguments")
		return 2
	}
	return report(stderr, doInstall(*binDir, cfgPath, cfg, stdout))
}

func doInstall(binDir, cfgPath string, cfg config, stdout io.Writer) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return errors.New("install supports macOS (LaunchAgent) and Linux (systemd user unit); elsewhere, run http serve under your own service manager")
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	_, db, err := openProvider(cfg, io.Discard, false)
	if err != nil {
		return err
	}
	if err := checkStore(db); err != nil {
		return err
	}
	dir, err := absPath(binDir)
	if err != nil {
		return err
	}
	bin := filepath.Join(dir, "memory-sharing-mcp")
	if err := copyExecutable(bin); err != nil {
		return err
	}
	unit, logFile, err := servicePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	// 0700: on Linux the log dir is the default state_dir, which keys.OwnerDir requires private.
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		err = startSystemd(unit, bin, cfgPath, logFile)
	} else {
		err = startLaunchd(unit, bin, cfgPath, logFile)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s\nservice %s, log %s\n", bin, unit, logFile)
	if !slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir) {
		fmt.Fprintf(stdout, "note: %s is not on PATH\n", dir)
	}
	return nil
}

// startLaunchd writes the LaunchAgent plist and (re)loads it.
func startLaunchd(plist, bin, cfgPath, logFile string) error {
	if err := os.WriteFile(plist, []byte(plistXML(bin, cfgPath, logFile)), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+serviceLabel).Run() // fails when not loaded; fine
	// ponytail: bootout finishes asynchronously, so bootstrap is retried for a few
	// seconds. Upgrade path: poll `launchctl print` if this proves flaky.
	var out []byte
	var err error
	for range 10 {
		if out, err = exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
}

// startSystemd writes the systemd user unit, enables it, and (re)starts it.
func startSystemd(unit, bin, cfgPath, logFile string) error {
	if err := os.WriteFile(unit, []byte(systemdUnit(bin, cfgPath, logFile)), 0o644); err != nil {
		return err
	}
	name := filepath.Base(unit)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", name}, {"restart", name}} {
		if out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// uninstall stops and removes the service and the installed binary. The
// config, store, and keys are kept.
func uninstall(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fset.SetOutput(stderr)
	binDir := fset.String("bin-dir", "~/.local/bin", "directory the binary was installed into")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintln(stderr, "uninstall takes no positional arguments")
		return 2
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return report(stderr, errors.New("uninstall manages a macOS LaunchAgent or a Linux systemd user unit"))
	}
	dir, err := absPath(*binDir)
	if err != nil {
		return report(stderr, err)
	}
	unit, _, err := servicePaths()
	if err != nil {
		return report(stderr, err)
	}
	// Stopping fails when the service is not loaded; fine.
	if runtime.GOOS == "linux" {
		exec.Command("systemctl", "--user", "disable", "--now", filepath.Base(unit)).Run()
	} else {
		exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel)).Run()
	}
	for _, p := range []string{unit, filepath.Join(dir, "memory-sharing-mcp")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return report(stderr, err)
		}
	}
	if runtime.GOOS == "linux" {
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	fmt.Fprintln(stdout, "service and binary removed; config, store, and keys kept")
	return 0
}

// servicePaths returns the service definition (LaunchAgent plist or systemd
// user unit) and its log file.
func servicePaths() (unit, logFile string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	if runtime.GOOS == "linux" {
		cfgDir, err := os.UserConfigDir()
		if err != nil {
			return "", "", err
		}
		return filepath.Join(cfgDir, "systemd", "user", serviceLabel+".service"),
			filepath.Join(home, ".local", "state", "memory-sharing-mcp", "memory-sharing-mcp.log"), nil
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"),
		filepath.Join(home, "Library", "Logs", "memory-sharing-mcp.log"), nil
}

// copyExecutable installs the running executable at dst, replacing it atomically.
func copyExecutable(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return err
	}
	if cur, err := filepath.EvalSymlinks(dst); err == nil && cur == src {
		return nil // already running the installed copy
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// plistXML is the LaunchAgent definition.
// ponytail: the log is not rotated; it gets one short line per call. Upgrade
// path: newsyslog or size-based rotation if it grows too large.
func plistXML(bin, cfgPath, logFile string) string {
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var args strings.Builder
	for _, a := range []string{bin, "--config", cfgPath, "http", "serve"} {
		fmt.Fprintf(&args, "\t\t<string>%s</string>\n", esc(a))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, serviceLabel, args.String(), esc(logFile), esc(logFile))
}

// systemdUnit is the systemd user unit definition. Each ExecStart argument is
// quoted with % and $ doubled so systemd does not expand them; the log path
// only needs % doubled.
func systemdUnit(bin, cfgPath, logFile string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	pct := strings.NewReplacer("%", "%%").Replace
	var args []string
	for _, a := range []string{bin, "--config", cfgPath, "http", "serve"} {
		args = append(args, `"`+q.Replace(a)+`"`)
	}
	return fmt.Sprintf(`[Unit]
Description=memory-sharing-mcp gateway

[Service]
ExecStart=%s
Restart=always
RestartSec=2
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, strings.Join(args, " "), pct(logFile), pct(logFile))
}
