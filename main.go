package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

const (
	apiVersion = "1.16.1"
	clientName = "subsonic-random-player"
)

type Config struct {
	BaseURL  string `yaml:"url"`
	User     string `yaml:"username"`
	Password string `yaml:"password"`
}

type Track struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
}

type apiEnvelope struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RandomSongs struct {
			Song []Track `json:"song"`
		} `json:"randomSongs"`
	} `json:"subsonic-response"`
}

type mpv struct {
	cmd        *exec.Cmd
	socketPath string
	trackEnded chan struct{}
}

type model struct {
	cfg      Config
	player   *mpv
	mpris    *mprisService
	controls chan playerControl
	track    Track
	playing  bool
	loading  bool
	err      string
	volume   int
}

type trackLoaded Track
type loadFailed error
type trackEnded struct{}

func main() {
	cfg, err := readConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	player, err := startMPV()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer player.stop()

	controls := make(chan playerControl, 1)
	mpris, err := startMPRIS(controls)
	if err != nil {
		fmt.Fprintln(os.Stderr, "MPRIS disabled:", err)
	} else {
		defer mpris.stop()
	}

	m := model{
		cfg:      cfg,
		player:   player,
		mpris:    mpris,
		controls: controls,
		loading:  true,
		volume:   70,
	}
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "TUI error:", err)
		os.Exit(1)
	}
}

func readConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("could not read config %s: %w\nCreate it using the example in README.md", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("invalid YAML in %s: %w", path, err)
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.User = strings.TrimSpace(cfg.User)
	if cfg.BaseURL == "" || cfg.User == "" || cfg.Password == "" {
		return Config{}, fmt.Errorf("config %s must define url, username, and password", path)
	}
	u, err := url.ParseRequestURI(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("config url must be an absolute http:// or https:// URL")
	}
	return cfg, nil
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "subsonic.yaml"), nil
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchRandom(m.cfg), m.waitForControl(), tea.SetWindowTitle("Subsonic Random Player"))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case trackLoaded:
		t := Track(msg)
		m.track = t
		m.loading = false
		m.playing = true
		m.err = ""
		m.syncMPRIS()
		streamURL, err := makeStreamURL(m.cfg, t.ID)
		if err != nil {
			m.err = err.Error()
			m.playing = false
			m.syncMPRIS()
			return m, nil
		}
		if err := m.player.command("loadfile", streamURL, "replace"); err != nil {
			m.err = "mpv: " + err.Error()
			m.playing = false
			m.syncMPRIS()
			return m, nil
		}
		return m, m.player.waitForTrackEnd()
	case loadFailed:
		m.loading = false
		m.err = msg.Error()
		return m, nil
	case trackEnded:
		if !m.loading {
			m.loading = true
			m.playing = false
			m.err = ""
			m.syncMPRIS()
			return m, fetchRandom(m.cfg)
		}
		return m, nil
	case playerControl:
		return m.handlePlayerControl(msg)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "n", "N", "r":
			if !m.loading {
				m.loading = true
				m.err = ""
				return m, fetchRandom(m.cfg)
			}
		case " ":
			if err := m.player.command("cycle", "pause"); err != nil {
				m.err = "mpv: " + err.Error()
			} else {
				m.playing = !m.playing
				m.syncMPRIS()
			}
		case "+", "=":
			if m.volume < 100 {
				m.volume += 5
				if err := m.player.command("set_property", "volume", m.volume); err != nil {
					m.err = "mpv: " + err.Error()
				}
				m.syncMPRIS()
			}
		case "-", "_":
			if m.volume > 0 {
				m.volume -= 5
				if err := m.player.command("set_property", "volume", m.volume); err != nil {
					m.err = "mpv: " + err.Error()
				}
				m.syncMPRIS()
			}
		}
	}
	return m, nil
}

func (m model) waitForControl() tea.Cmd {
	return func() tea.Msg {
		return <-m.controls
	}
}

func (m model) handlePlayerControl(control playerControl) (tea.Model, tea.Cmd) {
	listen := m.waitForControl()
	switch control.action {
	case controlQuit:
		return m, tea.Quit
	case controlNext, controlPrevious:
		if !m.loading {
			m.loading = true
			m.err = ""
			return m, tea.Batch(listen, fetchRandom(m.cfg))
		}
	case controlPause:
		if m.playing {
			if err := m.player.command("set_property", "pause", true); err != nil {
				m.err = "mpv: " + err.Error()
			} else {
				m.playing = false
				m.syncMPRIS()
			}
		}
	case controlPlay:
		if !m.playing && !m.loading {
			if err := m.player.command("set_property", "pause", false); err != nil {
				m.err = "mpv: " + err.Error()
			} else {
				m.playing = true
				m.syncMPRIS()
			}
		}
	}
	return m, listen
}

func (m model) syncMPRIS() {
	if m.mpris != nil {
		m.mpris.update(m.track, m.playing, m.volume)
	}
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString("\n  ♪  SUBSONIC RANDOM PLAYER\n")
	b.WriteString("  ─────────────────────────────────────────\n\n")

	if m.loading {
		b.WriteString("  Finding a random track…\n")
	} else if m.track.ID != "" {
		b.WriteString("  " + m.track.Title + "\n")
		if m.track.Artist != "" {
			b.WriteString("  Artist: " + m.track.Artist + "\n")
		}
		if m.track.Album != "" {
			b.WriteString("  Album:  " + m.track.Album + "\n")
		}
		if m.track.Duration > 0 {
			b.WriteString(fmt.Sprintf("  Length: %d:%02d\n", m.track.Duration/60, m.track.Duration%60))
		}
		state := "▶ Playing"
		if !m.playing {
			state = "Ⅱ Paused"
		}
		b.WriteString(fmt.Sprintf("\n  %s   Volume: %d%%\n", state, m.volume))
	} else {
		b.WriteString("  No track loaded yet.\n")
	}

	if m.err != "" {
		b.WriteString("\n  Error: " + m.err + "\n")
	}
	b.WriteString("\n  n  next random track       space  pause/resume\n")
	b.WriteString("  + / -  volume              q      quit\n\n")
	return b.String()
}

func fetchRandom(cfg Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		u, err := makeAPIURL(cfg, "getRandomSongs", url.Values{"size": {"1"}})
		if err != nil {
			return loadFailed(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return loadFailed(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return loadFailed(fmt.Errorf("Subsonic request failed: %w", err))
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return loadFailed(fmt.Errorf("Subsonic returned HTTP %s", resp.Status))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return loadFailed(err)
		}
		var envelope apiEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			return loadFailed(fmt.Errorf("invalid Subsonic JSON response: %w", err))
		}
		r := envelope.Response
		if r.Status != "ok" {
			if r.Error != nil {
				return loadFailed(fmt.Errorf("Subsonic API error %d: %s", r.Error.Code, r.Error.Message))
			}
			return loadFailed(fmt.Errorf("Subsonic API status: %s", r.Status))
		}
		if len(r.RandomSongs.Song) == 0 {
			return loadFailed(errors.New("Subsonic returned no songs"))
		}
		return trackLoaded(r.RandomSongs.Song[0])
	}
}

func makeAPIURL(cfg Config, endpoint string, extra url.Values) (string, error) {
	u, err := url.Parse(cfg.BaseURL + "/rest/" + endpoint + ".view")
	if err != nil {
		return "", err
	}
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(saltBytes)
	sum := md5.Sum([]byte(cfg.Password + salt))

	q := u.Query()
	q.Set("u", cfg.User)
	q.Set("t", hex.EncodeToString(sum[:]))
	q.Set("s", salt)
	q.Set("v", apiVersion)
	q.Set("c", clientName)
	q.Set("f", "json")
	for k, vals := range extra {
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func makeStreamURL(cfg Config, id string) (string, error) {
	return makeAPIURL(cfg, "stream", url.Values{"id": {id}})
}

func startMPV() (*mpv, error) {
	if _, err := exec.LookPath("mpv"); err != nil {
		return nil, errors.New("mpv not found; install mpv and ensure it is on PATH")
	}
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("subsonic-mpv-%d.sock", os.Getpid()))
	_ = os.Remove(socketPath)
	cmd := exec.Command("mpv", "--idle=yes", "--no-video", "--force-window=no",
		"--input-ipc-server="+socketPath, "--terminal=no")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start mpv: %w", err)
	}
	p := &mpv{cmd: cmd, socketPath: socketPath, trackEnded: make(chan struct{}, 1)}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			if err := p.command("set_property", "volume", 70); err == nil {
				if err := p.startTrackEndListener(); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					_ = os.Remove(socketPath)
					return nil, fmt.Errorf("could not listen for mpv playback events: %w", err)
				}
				return p, nil
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, errors.New("mpv IPC socket did not become ready; check that mpv can start")
}

func (p *mpv) waitForTrackEnd() tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-p.trackEnded; ok {
			return trackEnded{}
		}
		return nil
	}
}

func (p *mpv) startTrackEndListener() error {
	conn, err := net.DialTimeout("unix", p.socketPath, time.Second)
	if err != nil {
		return err
	}
	go p.listenForTrackEnd(conn)
	return nil
}

func (p *mpv) listenForTrackEnd(conn net.Conn) {
	defer conn.Close()
	defer close(p.trackEnded)

	decoder := json.NewDecoder(conn)
	for {
		var event struct {
			Event  string `json:"event"`
			Reason string `json:"reason"`
		}
		if err := decoder.Decode(&event); err != nil {
			return
		}
		if event.Event == "end-file" && event.Reason == "eof" {
			p.trackEnded <- struct{}{}
		}
	}
}

func (p *mpv) command(args ...any) error {
	conn, err := net.DialTimeout("unix", p.socketPath, time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to mpv IPC: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	payload := map[string]any{"command": args}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	_, err = conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (p *mpv) stop() {
	if p == nil {
		return
	}
	_ = p.command("quit")
	if p.cmd != nil && p.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}
	_ = os.Remove(p.socketPath)
}
