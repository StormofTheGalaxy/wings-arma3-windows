package windows

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

type Metadata struct {
	Root string
	Stop remote.ProcessStopConfiguration
}

var _ environment.ProcessEnvironment = (*Environment)(nil)

type Environment struct {
	mu sync.RWMutex

	Id            string
	Configuration *environment.Configuration
	meta          *Metadata

	cmd       *exec.Cmd
	hc        []*exec.Cmd
	startedAt time.Time
	exitCode  uint32

	emitter *events.Bus
	st      *system.AtomicString

	logCallbackMx sync.Mutex
	logCallback   func([]byte)
}

func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	if m.Root == "" {
		return nil, errors.New("environment/windows: server root cannot be empty")
	}
	return &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		exitCode:      0,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}, nil
}

func (e *Environment) Type() string { return "windows" }

func (e *Environment) Config() *environment.Configuration { return e.Configuration }

func (e *Environment) Events() *events.Bus { return e.emitter }

func (e *Environment) Exists() (bool, error) { return true, nil }

func (e *Environment) IsRunning(context.Context) (bool, error) {
	e.mu.RLock()
	cmd := e.cmd
	e.mu.RUnlock()
	return cmd != nil && cmd.Process != nil && e.State() != environment.ProcessOfflineState, nil
}

func (e *Environment) InSituUpdate() error { return nil }

func (e *Environment) OnBeforeStart(context.Context) error { return e.Create() }

func (e *Environment) Create() error {
	if err := os.MkdirAll(e.meta.Root, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(e.meta.Root, "logs"), 0o755); err != nil {
		return err
	}
	return e.syncMountLinks()
}

func (e *Environment) Destroy() error {
	_ = e.Terminate(context.Background(), "")
	e.SetState(environment.ProcessOfflineState)
	return nil
}

func (e *Environment) Start(ctx context.Context) error {
	if err := e.OnBeforeStart(ctx); err != nil {
		return err
	}
	if running, _ := e.IsRunning(ctx); running {
		return nil
	}

	e.SetState(environment.ProcessStartingState)
	if err := e.prepareArma(ctx); err != nil {
		e.SetState(environment.ProcessOfflineState)
		return err
	}

	cmd, err := e.command(e.env("SERVER_BINARY", "arma3server_x64.exe"), "-par=startup_params_server.txt")
	if err != nil {
		e.SetState(environment.ProcessOfflineState)
		return err
	}
	if err := e.startProcess(ctx, "server", cmd); err != nil {
		e.SetState(environment.ProcessOfflineState)
		return err
	}

	e.mu.Lock()
	e.cmd = cmd
	e.startedAt = time.Now()
	e.mu.Unlock()

	if err := e.startHeadlessClients(ctx); err != nil {
		e.publishLine("[daemon] failed to start one or more headless clients: " + err.Error())
	}

	go e.waitMain(cmd)
	go e.pollResources(ctx)
	return nil
}

func (e *Environment) Attach(context.Context) error { return nil }

func (e *Environment) Stop(ctx context.Context) error {
	e.SetState(environment.ProcessStoppingState)
	e.killHeadless(ctx, false)
	e.mu.RLock()
	cmd := e.cmd
	e.mu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		e.SetState(environment.ProcessOfflineState)
		return nil
	}
	return killProcessTree(ctx, cmd.Process.Pid, false)
}

func (e *Environment) WaitForStop(ctx context.Context, d time.Duration, terminate bool) error {
	if err := e.Stop(ctx); err != nil && !terminate {
		return err
	}
	deadline := time.After(d)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if running, _ := e.IsRunning(ctx); !running {
			return nil
		}
		select {
		case <-ctx.Done():
			if terminate {
				return e.Terminate(context.Background(), "")
			}
			return ctx.Err()
		case <-deadline:
			if terminate {
				return e.Terminate(context.Background(), "")
			}
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func (e *Environment) Terminate(ctx context.Context, _ string) error {
	e.SetState(environment.ProcessStoppingState)
	e.killHeadless(ctx, true)
	e.mu.RLock()
	cmd := e.cmd
	e.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		_ = killProcessTree(ctx, cmd.Process.Pid, true)
	}
	e.SetState(environment.ProcessOfflineState)
	return nil
}

func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.exitCode, false, nil
}

func (e *Environment) SendCommand(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	e.publishLine("> powershell " + command)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
	cmd.Dir = e.meta.Root
	cmd.Env = append(os.Environ(), e.Configuration.EnvironmentVariables()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { e.scanPipe("powershell", stdout); done <- struct{}{} }()
	go func() { e.scanPipe("powershell", stderr); done <- struct{}{} }()
	err = cmd.Wait()
	<-done
	<-done
	return err
}

func (e *Environment) Readlog(lines int) ([]string, error) {
	f, err := os.Open(filepath.Join(e.meta.Root, "logs", "latest.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		out = append(out, s.Text())
		if len(out) > lines {
			out = out[1:]
		}
	}
	return out, s.Err()
}

func (e *Environment) State() string { return e.st.Load() }

func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState && state != environment.ProcessStartingState && state != environment.ProcessRunningState && state != environment.ProcessStoppingState {
		panic(fmt.Sprintf("invalid server state received: %s", state))
	}
	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

func (e *Environment) Uptime(context.Context) (int64, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.startedAt.IsZero() || e.State() == environment.ProcessOfflineState {
		return 0, nil
	}
	return time.Since(e.startedAt).Milliseconds(), nil
}

func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	e.logCallback = f
	e.logCallbackMx.Unlock()
}

func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.mu.Lock()
	e.meta.Stop = c
	e.mu.Unlock()
}

func (e *Environment) Install(ctx context.Context) error {
	if err := e.Create(); err != nil {
		return err
	}
	steamcmd, err := e.ensureSteamCMD(ctx)
	if err != nil {
		return err
	}
	e.publishLine("[install] Installing/validating Arma 3 Dedicated Server files...")
	serverArgs := []string{"+force_install_dir", e.meta.Root, "+login", e.env("STEAM_USER", "anonymous"), e.env("STEAM_PASS", ""), "+app_update", e.env("STEAMCMD_APPID", "233780")}
	serverArgs = append(serverArgs, e.betaArgs()...)
	if validate := e.validateArg(); validate != "" {
		serverArgs = append(serverArgs, validate)
	}
	serverArgs = append(serverArgs, "+quit")
	if err := e.runSteamCMD(ctx, steamcmd, serverArgs...); err != nil {
		return err
	}
	if err := e.ensureDefaultConfig(ctx, "server.cfg", e.env("SERVER_CFG_URL", "https://raw.githubusercontent.com/pelican-eggs/games-steamcmd/main/arma/arma3/egg-arma3-config/server.cfg")); err != nil {
		return err
	}
	if err := e.ensureDefaultConfig(ctx, "basic.cfg", e.env("BASIC_CFG_URL", "https://raw.githubusercontent.com/pelican-eggs/games-steamcmd/main/arma/arma3/egg-arma3-config/basic.cfg")); err != nil {
		return err
	}
	e.publishLine("[install] Installation completed.")
	return nil
}

func (e *Environment) prepareArma(ctx context.Context) error {
	if err := e.runSteamPreset(ctx); err != nil {
		return err
	}
	if err := e.writeStartupParams(); err != nil {
		return err
	}
	return nil
}

func (e *Environment) runSteamPreset(ctx context.Context) error {
	if e.env("UPDATE_SERVER", "1") != "1" {
		e.publishLine("[update] SteamCMD update disabled by UPDATE_SERVER=0")
		return nil
	}
	steamcmd := e.env("STEAMCMD_PATH", filepath.Join(e.meta.Root, "steamcmd", "steamcmd.exe"))
	if _, err := os.Stat(steamcmd); err != nil {
		e.publishLine("[update] steamcmd.exe not found, skipping update: " + steamcmd)
		return nil
	}
	serverArgs := []string{"+force_install_dir", e.meta.Root, "+login", e.env("STEAM_USER", "anonymous"), e.env("STEAM_PASS", ""), "+app_update", e.env("STEAMCMD_APPID", "233780")}
	serverArgs = append(serverArgs, e.betaArgs()...)
	if validate := e.validateArg(); validate != "" {
		serverArgs = append(serverArgs, validate)
	}
	serverArgs = append(serverArgs, "+quit")
	if err := e.runSteamCMD(ctx, steamcmd, serverArgs...); err != nil {
		return err
	}
	mods := e.allWorkshopMods()
	for _, mod := range mods {
		if err := e.runSteamCMD(ctx, steamcmd, "+force_install_dir", e.meta.Root, "+login", e.env("STEAM_USER", "anonymous"), e.env("STEAM_PASS", ""), "+workshop_download_item", "107410", mod, "+quit"); err != nil {
			return err
		}
		e.linkWorkshopMod(mod)
	}
	return nil
}

func (e *Environment) ensureSteamCMD(ctx context.Context) (string, error) {
	steamcmd := e.env("STEAMCMD_PATH", filepath.Join(e.meta.Root, "steamcmd", "steamcmd.exe"))
	if _, err := os.Stat(steamcmd); err == nil {
		return steamcmd, nil
	}
	dir := filepath.Dir(steamcmd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	zipPath := filepath.Join(dir, "steamcmd.zip")
	e.publishLine("[install] Downloading SteamCMD for Windows...")
	if err := downloadFile(ctx, e.env("STEAMCMD_DOWNLOAD_URL", "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"), zipPath); err != nil {
		return "", err
	}
	if err := unzip(zipPath, dir); err != nil {
		return "", err
	}
	_ = os.Remove(zipPath)
	if _, err := os.Stat(steamcmd); err != nil {
		return "", err
	}
	return steamcmd, nil
}

func (e *Environment) ensureDefaultConfig(ctx context.Context, name string, url string) error {
	path := filepath.Join(e.meta.Root, name)
	if _, err := os.Stat(path); err == nil || strings.TrimSpace(url) == "" {
		return nil
	}
	e.publishLine("[install] Downloading default " + name)
	return downloadFile(ctx, url, path)
}

func (e *Environment) betaArgs() []string {
	if beta := e.env("STEAMCMD_BETAID", "public"); beta != "" && beta != "public" {
		return []string{"-beta", beta}
	}
	return nil
}

func (e *Environment) validateArg() string {
	if e.env("VALIDATE_SERVER", "0") == "1" {
		return "validate"
	}
	return ""
}

func (e *Environment) runSteamCMD(ctx context.Context, steamcmd string, args ...string) error {
	var clean []string
	for _, arg := range args {
		if strings.TrimSpace(arg) != "" {
			clean = append(clean, arg)
		}
	}

	// Create a redacted copy of the args for logging so sensitive values
	// (Steam password passed after "+login") are not written to logs.
	redacted := make([]string, len(clean))
	copy(redacted, clean)
	for i := 0; i < len(clean); i++ {
		// If we see a separate "+login" token, the password is the token
		// two positions after it: "+login" <user> <pass>
		if strings.EqualFold(clean[i], "+login") {
			if i+2 < len(redacted) {
				redacted[i+2] = "REDACTED"
			}
			i += 2
			continue
		}
		// If the login/token was passed in a single token (uncommon),
		// mask anything after the "+login" prefix.
		lower := strings.ToLower(clean[i])
		if strings.HasPrefix(lower, "+login") && len(clean[i]) > len("+login") {
			redacted[i] = "+login[REDACTED]"
		}
	}

	e.publishLine("[update] running SteamCMD " + strings.Join(redacted, " "))
	cmd := exec.CommandContext(ctx, steamcmd, clean...)
	cmd.Dir = e.meta.Root
	cmd.Env = append(os.Environ(), e.Configuration.EnvironmentVariables()...)
	return e.runAndStream("steamcmd", cmd)
}

func (e *Environment) writeStartupParams() error {
	clientMods := e.clientMods()
	serverMods := e.env("SERVERMODS", "")
	server := []string{
		"-name=server",
		"-ip=0.0.0.0",
		"-port=" + e.env("SERVER_PORT", "2302"),
		"-cfg=basic.cfg",
		"-config=server.cfg",
		"-mod=" + clientMods,
		"-serverMod=" + serverMods,
		"-limitFPS=" + e.env("PARAM_LIMITFPS", "50"),
	}
	if e.env("PARAM_LOADMISSIONTOMEMORY", "1") == "1" {
		server = append(server, "-loadMissionToMemory")
	}
	if e.env("PARAM_AUTOINIT", "0") == "1" {
		server = append(server, "-autoInit")
	}
	if e.env("PARAM_FILEPATCHING", "0") == "1" {
		server = append(server, "-filePatching")
	}
	if e.env("PARAM_NOLOGS", "1") == "1" {
		server = append(server, "-noLogs")
	}
	if maxMem := e.env("SERVER_MAXMEM", e.env("PARAM_MAXMEM", "")); maxMem != "" {
		server = append(server, "-maxMem="+maxMem)
	}
	hc := []string{
		"-client",
		"-ip=127.0.0.1",
		"-port=" + e.env("SERVER_PORT", "2302"),
		"-password=" + e.env("SERVER_PASSWORD", ""),
		"-mod=" + clientMods,
		"-limitFPS=" + e.env("HC_LIMITFPS", e.env("PARAM_LIMITFPS", "50")),
	}
	if e.env("PARAM_FILEPATCHING", "0") == "1" {
		hc = append(hc, "-filePatching")
	}
	if maxMem := e.env("HC_MAXMEM", ""); maxMem != "" {
		hc = append(hc, "-maxMem="+maxMem)
	}
	if err := os.WriteFile(filepath.Join(e.meta.Root, "startup_params_server.txt"), []byte(strings.Join(server, "\r\n")+"\r\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.meta.Root, "startup_params_hc.txt"), []byte(strings.Join(hc, "\r\n")+"\r\n"), 0o644)
}

func (e *Environment) startHeadlessClients(ctx context.Context) error {
	n, _ := strconv.Atoi(e.env("HC_NUM", "0"))
	if n <= 0 {
		return nil
	}
	var first error
	for i := 1; i <= n; i++ {
		cmd, err := e.command(e.env("SERVER_BINARY", "arma3server_x64.exe"), "-par=startup_params_hc.txt")
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := e.startProcess(ctx, fmt.Sprintf("hc %d", i), cmd); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		e.mu.Lock()
		e.hc = append(e.hc, cmd)
		e.mu.Unlock()
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
	}
	return first
}

func (e *Environment) command(binary string, args ...string) (*exec.Cmd, error) {
	binary = strings.Trim(binary, `"`)
	if filepath.Ext(binary) == "" {
		binary += ".exe"
	}
	if !filepath.IsAbs(binary) {
		binary = filepath.Join(e.meta.Root, binary)
	}
	if _, err := os.Stat(binary); err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = e.meta.Root
	cmd.Env = append(os.Environ(), e.Configuration.EnvironmentVariables()...)
	return cmd, nil
}

func (e *Environment) startProcess(_ context.Context, name string, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	e.publishLine(fmt.Sprintf("[daemon] started %s pid=%d", name, cmd.Process.Pid))
	go e.scanPipe(name, stdout)
	go e.scanPipe(name, stderr)
	return nil
}

func (e *Environment) waitMain(cmd *exec.Cmd) {
	err := cmd.Wait()
	code := uint32(0)
	if cmd.ProcessState != nil {
		code = uint32(cmd.ProcessState.ExitCode())
	}
	e.mu.Lock()
	e.exitCode = code
	e.cmd = nil
	e.mu.Unlock()
	if err != nil {
		e.publishLine(fmt.Sprintf("[daemon] server exited with code %d: %s", code, err.Error()))
	}
	e.killHeadless(context.Background(), true)
	e.SetState(environment.ProcessOfflineState)
}

func (e *Environment) scanPipe(prefix string, r io.Reader) {
	readLowLatency(r, 75*time.Millisecond, func(b []byte) {
		for _, line := range splitOutputChunk(b) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			e.publishLine("[" + prefix + "] " + string(bytes.TrimRight(line, "\r\n")))
		}
	})
}

func (e *Environment) publishLine(line string) {
	line = strings.TrimRight(line, "\r\n")
	_ = os.MkdirAll(filepath.Join(e.meta.Root, "logs"), 0o755)
	if f, err := os.OpenFile(filepath.Join(e.meta.Root, "logs", "latest.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	e.logCallbackMx.Lock()
	cb := e.logCallback
	e.logCallbackMx.Unlock()
	if cb != nil {
		cb([]byte(line))
	}
}

func (e *Environment) pollResources(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var prevCPU uint64
	var prevAt time.Time
	netBaseline, _ := currentNetworkCounters()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e.State() == environment.ProcessOfflineState {
				return
			}
			uptime, _ := e.Uptime(ctx)
			stats, cpu, at := e.resourceStats(prevCPU, prevAt, netBaseline)
			prevCPU = cpu
			prevAt = at
			stats.Uptime = uptime
			stats.MemoryLimit = uint64(e.Configuration.Limits().MemoryLimit * 1024 * 1024)
			e.Events().Publish(environment.ResourceEvent, stats)
		}
	}
}

func (e *Environment) killHeadless(ctx context.Context, force bool) {
	e.mu.Lock()
	hc := e.hc
	e.hc = nil
	e.mu.Unlock()
	for _, cmd := range hc {
		if cmd != nil && cmd.Process != nil {
			_ = killProcessTree(ctx, cmd.Process.Pid, force)
		}
	}
}

func (e *Environment) runAndStream(prefix string, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { e.scanPipe(prefix, stdout); done <- struct{}{} }()
	go func() { e.scanPipe(prefix, stderr); done <- struct{}{} }()
	err = cmd.Wait()
	<-done
	<-done
	return err
}

func readLowLatency(r io.Reader, flushInterval time.Duration, callback func([]byte)) {
	chunks := make(chan []byte, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				chunks <- b
			}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	var pending bytes.Buffer
	flush := func() {
		if pending.Len() == 0 {
			return
		}
		b := make([]byte, pending.Len())
		copy(b, pending.Bytes())
		pending.Reset()
		callback(b)
	}
	for {
		select {
		case b := <-chunks:
			pending.Write(b)
			if bytes.ContainsAny(b, "\r\n") || pending.Len() >= 4096 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-done:
			for {
				select {
				case b := <-chunks:
					pending.Write(b)
				default:
					flush()
					return
				}
			}
		}
	}
}

func splitOutputChunk(b []byte) [][]byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
	return bytes.Split(b, []byte("\n"))
}

func (e *Environment) env(key, fallback string) string {
	prefix := strings.ToUpper(key) + "="
	for _, kv := range e.Configuration.EnvironmentVariables() {
		if strings.HasPrefix(strings.ToUpper(kv), prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(kv, kv[:len(prefix)]))
			if value == "" {
				return fallback
			}
			return value
		}
	}
	return fallback
}

func (e *Environment) clientMods() string {
	mods := e.normalizeModList(e.env("MODIFICATIONS", ""))
	mods = append(mods, e.modsFromLauncherFile(e.env("MOD_FILE", "modlist.html"))...)
	return strings.Join(unique(mods), ";") + trailingSemicolon(mods)
}

func (e *Environment) allWorkshopMods() []string {
	mods := append(e.normalizeModList(e.clientMods()), e.normalizeModList(e.env("SERVERMODS", ""))...)
	mods = append(mods, e.normalizeModList(e.env("OPTIONALMODS", ""))...)
	var ids []string
	for _, mod := range unique(mods) {
		mod = strings.TrimPrefix(mod, "@")
		if _, err := strconv.Atoi(mod); err == nil {
			ids = append(ids, mod)
		}
	}
	return ids
}

func (e *Environment) normalizeModList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (e *Environment) modsFromLauncherFile(name string) []string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(e.meta.Root, name))
	if err != nil || !bytes.Contains(b, []byte("Created by Arma 3 Launcher")) {
		return nil
	}
	re := regexp.MustCompile(`id=(\d+)`)
	var mods []string
	for _, m := range re.FindAllSubmatch(b, -1) {
		mods = append(mods, "@"+string(m[1]))
	}
	return mods
}

func (e *Environment) linkWorkshopMod(id string) {
	source := filepath.Join(e.meta.Root, "steamapps", "workshop", "content", "107410", id)
	if _, err := os.Stat(source); err != nil {
		return
	}
	target := filepath.Join(e.meta.Root, "@"+id)
	_ = os.RemoveAll(target)
	_ = exec.Command("cmd.exe", "/C", "mklink", "/D", target, source).Run()
	_ = filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".bikey") {
			return nil
		}
		_ = os.MkdirAll(filepath.Join(e.meta.Root, "keys"), 0o755)
		data, err := os.ReadFile(path)
		if err == nil {
			_ = os.WriteFile(filepath.Join(e.meta.Root, "keys", filepath.Base(path)), data, 0o644)
		}
		return nil
	})
}

func unique(in []string) []string {
	set := map[string]struct{}{}
	for _, v := range in {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func trailingSemicolon(mods []string) string {
	if len(mods) == 0 {
		return ""
	}
	return ";"
}

func (e *Environment) syncMountLinks() error {
	root := filepath.Join(e.meta.Root, "_mounts")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, m := range e.Configuration.Mounts() {
		if m.Default || m.Source == "" {
			continue
		}
		name := filepath.Base(filepath.Clean(m.Target))
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = filepath.Base(filepath.Clean(m.Source))
		}
		target := filepath.Join(root, name)
		_ = os.RemoveAll(target)
		if err := exec.Command("cmd.exe", "/C", "mklink", "/D", target, m.Source).Run(); err != nil {
			log.WithError(err).WithField("source", m.Source).Warn("failed to create mount junction")
		}
	}
	return nil
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("download failed for %s: %s", url, res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, res.Body)
	return err
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		path := filepath.Join(dst, f.Name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, f.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
