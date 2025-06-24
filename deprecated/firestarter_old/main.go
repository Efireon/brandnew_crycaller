package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	"github.com/mattn/go-isatty"
)

// ================= LOGGER & GLOBAL CONFIG =================
var bareLog *log.Logger
var debugLog *log.Logger

var globalConfig *Config

// ================= CONFIG STRUCTS =================
type KeysConfig struct {
	Focus   string            `json:"focus,omitempty"`
	Custom  map[string]string `json:"custom,omitempty"`
	Restart string            `json:"restart,omitempty"`
}

type Config struct {
	BackgroundScripts  []ScriptConfig `json:"background_scripts"`
	InteractiveScripts []ScriptConfig `json:"interactive_scripts"`
}

type ScriptConfig struct {
	Path      string     `json:"path"`
	Args      string     `json:"args"`
	Type      string     `json:"type"` // e.g. "binary", "binary, curses", "script, info"
	MaxLogs   int        `json:"max_logs,omitempty"`
	Output    bool       `json:"output"`               // показывать отдельную плитку
	OutputRes string     `json:"output_res,omitempty"` // пример: "10x40"
	Keys      KeysConfig `json:"keys,omitempty"`
	Collapse  bool       `json:"collapse,omitempty"` // сворачивать плитку через 3 секунды после завершения
}

// ================= SCRIPT STATUS =================
type ScriptStatus int

const (
	StatusWaiting ScriptStatus = iota
	StatusRunning
	StatusPassed
	StatusFailed
)

func (s ScriptStatus) String() string {
	switch s {
	case StatusWaiting:
		return "WAITING"
	case StatusRunning:
		return "RUNNING"
	case StatusPassed:
		return "PASSED"
	case StatusFailed:
		return "FAILED"
	}
	return "UNKNOWN"
}

// ================= VIRTUAL TERMINAL BUFFER (for curses programs) =================
type VirtualTerminalBuffer struct {
	rows      int
	cols      int
	buffer    [][]rune
	cursorRow int
	cursorCol int
}

func NewVirtualTerminalBuffer(rows, cols int) *VirtualTerminalBuffer {
	if cols < 1 {
		cols = 1
	}
	buf := make([][]rune, rows)
	for i := 0; i < rows; i++ {
		buf[i] = make([]rune, cols)
		for j := 0; j < cols; j++ {
			buf[i][j] = ' '
		}
	}
	return &VirtualTerminalBuffer{
		rows:      rows,
		cols:      cols,
		buffer:    buf,
		cursorRow: 0,
		cursorCol: 0,
	}
}

func (vt *VirtualTerminalBuffer) clearScreen(mode string) {
	switch mode {
	case "", "0":
		for r := vt.cursorRow; r < vt.rows; r++ {
			for c := 0; c < vt.cols; c++ {
				vt.buffer[r][c] = ' '
			}
		}
	case "1":
		for r := 0; r <= vt.cursorRow; r++ {
			for c := 0; c < vt.cols; c++ {
				vt.buffer[r][c] = ' '
			}
		}
	case "2":
		for r := 0; r < vt.rows; r++ {
			for c := 0; c < vt.cols; c++ {
				vt.buffer[r][c] = ' '
			}
		}
		vt.cursorRow = 0
		vt.cursorCol = 0
	}
}

func (vt *VirtualTerminalBuffer) clearLine(mode string) {
	switch mode {
	case "", "0":
		for c := vt.cursorCol; c < vt.cols; c++ {
			vt.buffer[vt.cursorRow][c] = ' '
		}
	case "1":
		for c := 0; c <= vt.cursorCol; c++ {
			vt.buffer[vt.cursorRow][c] = ' '
		}
	case "2":
		for c := 0; c < vt.cols; c++ {
			vt.buffer[vt.cursorRow][c] = ' '
		}
	}
}

func (vt *VirtualTerminalBuffer) scroll() {
	vt.buffer = append(vt.buffer[1:], make([]rune, vt.cols))
	for i := 0; i < vt.cols; i++ {
		vt.buffer[vt.rows-1][i] = ' '
	}
	if vt.cursorRow > 0 {
		vt.cursorRow--
	}
}

func (vt *VirtualTerminalBuffer) Write(s string) {
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			seq := ""
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				seq += string(s[j])
				j++
			}
			if j < len(s) {
				cmd := s[j]
				params := parseParams(seq)
				switch cmd {
				case 'J':
					vt.clearScreen(params[0])
				case 'K':
					vt.clearLine(params[0])
				case 'A':
					n := atoiParam(params, 0, 1)
					vt.cursorRow = clamp(vt.cursorRow-n, 0, vt.rows-1)
				case 'B':
					n := atoiParam(params, 0, 1)
					vt.cursorRow = clamp(vt.cursorRow+n, 0, vt.rows-1)
				case 'C':
					n := atoiParam(params, 0, 1)
					vt.cursorCol = clamp(vt.cursorCol+n, 0, vt.cols-1)
				case 'D':
					n := atoiParam(params, 0, 1)
					vt.cursorCol = clamp(vt.cursorCol-n, 0, vt.cols-1)
				case 'H':
					r := atoiParam(params, 0, 1) - 1
					c := atoiParam(params, 1, 1) - 1
					vt.cursorRow = clamp(r, 0, vt.rows-1)
					vt.cursorCol = clamp(c, 0, vt.cols-1)
				}
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r == '\n' {
			vt.cursorRow++
			vt.cursorCol = 0
			if vt.cursorRow >= vt.rows {
				vt.scroll()
				vt.cursorRow = vt.rows - 1
			}
		} else if r == '\r' {
			vt.cursorCol = 0
		} else {
			vt.buffer[vt.cursorRow][vt.cursorCol] = r
			vt.cursorCol++
			if vt.cursorCol >= vt.cols {
				vt.cursorCol = 0
				vt.cursorRow++
				if vt.cursorRow >= vt.rows {
					vt.scroll()
					vt.cursorRow = vt.rows - 1
				}
			}
		}
	}
}

func (vt *VirtualTerminalBuffer) RenderVisible() string {
	lines := make([]string, vt.rows)
	for i, line := range vt.buffer {
		lines[i] = string(line)
	}
	return strings.Join(lines, "\n")
}

// ================= BGScript =================
type BgScript struct {
	Path      string
	Args      string
	Type      string
	Info      bool
	Status    ScriptStatus
	Code      int
	RawLog    []string
	MaxLogs   int
	Output    bool
	OutHeight int
	OutWidth  int
	OutputRes string
	Collapse  bool

	vtBuffer *VirtualTerminalBuffer

	StartTime  time.Time
	EndTime    time.Time
	Duration   time.Duration
	FinishedAt time.Time

	cmd         *exec.Cmd
	pty         *os.File
	cancel      context.CancelFunc
	mutex       sync.Mutex
	Keys        KeysConfig
	ConfigIndex int
}

func (b *BgScript) Start(wg *sync.WaitGroup, notifyFn func()) {
	defer wg.Done()
	b.Status = StatusRunning
	b.StartTime = time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	args := parseArgs(b.Args)
	isCurses := strings.Contains(strings.ToLower(b.Type), "curses")
	var cmd *exec.Cmd
	if b.Type == "script" || (!isCurses && b.Type == "binary") {
		if b.Type == "script" {
			cmd = exec.CommandContext(ctx, "bash", append([]string{b.Path}, args...)...)
		} else {
			cmd = exec.CommandContext(ctx, b.Path, args...)
		}
	} else if isCurses {
		cmd = exec.CommandContext(ctx, b.Path, args...)
	} else {
		b.Status = StatusFailed
		b.Code = -1
		notifyFn()
		return
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	b.cmd = cmd

	ptmx, err := pty.Start(cmd)
	if err != nil {
		b.Status = StatusFailed
		b.Code = -1
		notifyFn()
		return
	}
	b.pty = ptmx
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 1000, Cols: 2000})
	if isCurses {
		b.vtBuffer = NewVirtualTerminalBuffer(b.OutHeight, b.OutWidth)
	}

	go func() {
		reader := bufio.NewReader(ptmx)
		for {
			buf := make([]byte, 1024)
			n, err := reader.Read(buf)
			if n > 0 {
				text := string(buf[:n])
				bareLog.Println("BG raw:", text)
				if isCurses && b.vtBuffer != nil {
					b.vtBuffer.Write(text)
				} else {
					text = strings.ReplaceAll(text, "\r\n", "\n")
					text = strings.ReplaceAll(text, "\r", "\n")
					lines := strings.Split(text, "\n")
					b.RawLog = append(b.RawLog, lines...)
				}
				notifyFn()
			}
			if err != nil {
				if err != io.EOF {
					bareLog.Println("BG read error:", err)
				}
				break
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		b.Status = StatusFailed
		if exitErr, ok := err.(*exec.ExitError); ok {
			b.Code = exitErr.ExitCode()
		} else {
			b.Code = -1
		}
	} else {
		b.Status = StatusPassed
		b.Code = 0
	}
	b.EndTime = time.Now()
	b.Duration = b.EndTime.Sub(b.StartTime)
	b.FinishedAt = time.Now()
	notifyFn()
}

func (b *BgScript) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.pty != nil {
		b.pty.Close()
	}
}

// ================= INTERACTIVE SCRIPT =================
type IntScript struct {
	Path      string
	Args      string
	Type      string
	Info      bool
	Status    ScriptStatus
	Code      int
	RawLog    []string
	MaxLogs   int
	Output    bool
	OutHeight int
	OutWidth  int
	OutputRes string
	Collapse  bool

	vtBuffer *VirtualTerminalBuffer

	StartTime  time.Time
	EndTime    time.Time
	Duration   time.Duration
	FinishedAt time.Time

	cmd         *exec.Cmd
	pty         *os.File
	mutex       sync.Mutex
	Keys        KeysConfig
	ConfigIndex int
}

func (i *IntScript) Start(wg *sync.WaitGroup, notifyFn func()) {
	defer wg.Done()
	i.Status = StatusRunning
	i.StartTime = time.Now()

	args := parseArgs(i.Args)
	isCurses := strings.Contains(strings.ToLower(i.Type), "curses")
	var cmd *exec.Cmd
	if i.Type == "script" || (!isCurses && i.Type == "binary") {
		if i.Type == "script" {
			cmd = exec.CommandContext(context.Background(), "bash", append([]string{i.Path}, args...)...)
		} else {
			cmd = exec.CommandContext(context.Background(), i.Path, args...)
		}
	} else if isCurses {
		cmd = exec.CommandContext(context.Background(), i.Path, args...)
	} else {
		i.Status = StatusFailed
		i.Code = -1
		notifyFn()
		return
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	i.cmd = cmd

	ptmx, err := pty.Start(cmd)
	if err != nil {
		i.Status = StatusFailed
		i.Code = -1
		notifyFn()
		return
	}
	i.pty = ptmx
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 1000, Cols: 2000})
	if isCurses {
		i.vtBuffer = NewVirtualTerminalBuffer(i.OutHeight, i.OutWidth)
	}

	go func() {
		reader := bufio.NewReader(ptmx)
		for {
			buf := make([]byte, 1024)
			n, err := reader.Read(buf)
			if n > 0 {
				text := string(buf[:n])
				bareLog.Println("INT raw:", text)
				if isCurses && i.vtBuffer != nil {
					i.vtBuffer.Write(text)
				} else {
					text = strings.ReplaceAll(text, "\r\n", "\n")
					text = strings.ReplaceAll(text, "\r", "\n")
					lines := strings.Split(text, "\n")
					i.RawLog = append(i.RawLog, lines...)
				}
				notifyFn()
			}
			if err != nil {
				if err != io.EOF {
					bareLog.Println("INT read error:", err)
				}
				break
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		i.Status = StatusFailed
		if exitErr, ok := err.(*exec.ExitError); ok {
			i.Code = exitErr.ExitCode()
		} else {
			i.Code = -1
		}
	} else {
		i.Status = StatusPassed
		i.Code = 0
	}
	i.EndTime = time.Now()
	i.Duration = i.EndTime.Sub(i.StartTime)
	i.FinishedAt = time.Now()
	notifyFn()
}

func (i *IntScript) Stop() {
	if i.cmd != nil && i.cmd.Process != nil {
		i.cmd.Process.Kill()
	}
	if i.pty != nil {
		i.pty.Close()
	}
}

// Функция для получения последних N строк из слайса строк
func getLastLines(lines []string, count int) []string {
	if len(lines) <= count {
		return lines
	}
	return lines[len(lines)-count:]
}

// ================= INDIVIDUAL RESTART HELPERS =================
func restartBgTest(old *BgScript, notifyFn func()) *BgScript {
	config := globalConfig.BackgroundScripts[old.ConfigIndex]
	maxLogs := config.MaxLogs
	if maxLogs <= 0 {
		maxLogs = 5
	}
	isCurses := strings.Contains(strings.ToLower(config.Type), "curses")
	h, w, err := parseOutputRes(config.OutputRes, isCurses)
	if err != nil {
		bareLog.Printf("Config error for %s: %v, using defaults", config.Path, err)
		h, w = 10, 40
	}
	parts := strings.Split(config.Type, ",")
	baseType := strings.TrimSpace(parts[0])
	infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
	newTest := &BgScript{
		Path:        config.Path,
		Args:        config.Args,
		Type:        baseType,
		Info:        infoFlag,
		Status:      StatusWaiting,
		Code:        -1,
		RawLog:      []string{},
		MaxLogs:     maxLogs,
		Output:      config.Output,
		OutHeight:   h,
		OutWidth:    w,
		OutputRes:   config.OutputRes,
		Collapse:    config.Collapse,
		Keys:        config.Keys,
		ConfigIndex: old.ConfigIndex,
	}
	go func() {
		var wg sync.WaitGroup
		wg.Add(1)
		newTest.Start(&wg, notifyFn)
	}()
	return newTest
}

func restartIntTest(old *IntScript, notifyFn func()) *IntScript {
	config := globalConfig.InteractiveScripts[old.ConfigIndex]
	maxLogs := config.MaxLogs
	if maxLogs <= 0 {
		maxLogs = 5
	}
	isCurses := strings.Contains(strings.ToLower(config.Type), "curses")
	h, w, err := parseOutputRes(config.OutputRes, isCurses)
	if err != nil {
		bareLog.Printf("Config error for %s: %v, using defaults", config.Path, err)
		h, w = 10, 40
	}
	parts := strings.Split(config.Type, ",")
	baseType := strings.TrimSpace(parts[0])
	infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
	newTest := &IntScript{
		Path:        config.Path,
		Args:        config.Args,
		Type:        baseType,
		Info:        infoFlag,
		Status:      StatusWaiting,
		Code:        -1,
		RawLog:      []string{},
		MaxLogs:     maxLogs,
		Output:      config.Output,
		OutHeight:   h,
		OutWidth:    w,
		OutputRes:   config.OutputRes,
		Collapse:    config.Collapse,
		Keys:        config.Keys,
		ConfigIndex: old.ConfigIndex,
	}
	go func() {
		var wg sync.WaitGroup
		wg.Add(1)
		newTest.Start(&wg, notifyFn)
	}()
	return newTest
}

// ================= BUBBLE TEA MODEL & UI =================
type uiMode int

const (
	modeMain uiMode = iota
	modeFinal
)

type doneAllMsg struct{}
type refreshMsg struct{}
type selectTileMsg struct{ index int }

type outputTile struct {
	isBackground bool
	index        int
}

type model struct {
	bgScripts  []*BgScript
	intScripts []*IntScript
	mode       uiMode
	quitting   bool
	exitCode   int
	width      int
	height     int

	outputTiles     []outputTile
	selectedTileIdx int
	ctrlPressed     bool
}

func (m model) Init() tea.Cmd {
	// Запускаем периодическую команду обновления состояния
	return tickCmd()
}

// tickCmd отправляет refreshMsg каждую секунду
func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return refreshMsg{}
	})
}

// ================= UPDATE (TEA) =================
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, tickCmd()
	case doneAllMsg:
		// Когда все тесты завершены – переходим в финальный режим
		for _, b := range m.bgScripts {
			if b.Info && b.Status == StatusRunning {
				b.Stop()
			}
		}
		for _, i := range m.intScripts {
			if i.Info && i.Status == StatusRunning {
				i.Stop()
			}
		}
		m.mode = modeFinal
		m.exitCode = computeExitCode(m.bgScripts, m.intScripts)
		return m, tickCmd()
	case selectTileMsg:
		if msg.index >= 0 && msg.index < len(m.outputTiles) {
			m.selectedTileIdx = msg.index
		}
		return m, tickCmd()
	case refreshMsg:
		m.outputTiles = buildOutputTiles(m.bgScripts, m.intScripts)
		if len(m.outputTiles) > 0 && m.selectedTileIdx >= len(m.outputTiles) {
			m.selectedTileIdx = len(m.outputTiles) - 1
		}
		return m, tickCmd()
	case tea.KeyMsg:
		m, cmd := handleKeyMsg(m, msg)
		return m, tea.Batch(cmd, tickCmd())
	}
	return m, tickCmd()
}

// Выделили обработку клавиш в отдельную функцию для удобства
func handleKeyMsg(m model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// Если нажата комбинация ctrl+<X>, извлекаем X
	ctrlKey := ""
	if strings.HasPrefix(k, "ctrl+") {
		ctrlKey = strings.TrimPrefix(k, "ctrl+")
	}

	// Сначала обрабатываем custom-обработку: она глобальна и работает даже без фокуса
	if ctrlKey != "" {
		sent := false
		for _, b := range m.bgScripts {
			if b.Status == StatusRunning && b.pty != nil {
				if mapped, ok := b.Keys.Custom[ctrlKey]; ok {
					b.mutex.Lock()
					sendKeyToPty(b.pty, mapped)
					b.mutex.Unlock()
					sent = true
				}
			}
		}
		for _, i := range m.intScripts {
			if i.Status == StatusRunning && i.pty != nil {
				if mapped, ok := i.Keys.Custom[ctrlKey]; ok {
					i.mutex.Lock()
					sendKeyToPty(i.pty, mapped)
					i.mutex.Unlock()
					sent = true
				}
			}
		}
		if sent {
			return m, nil
		}
	}

	// Индивидуальный рестарт теста: ctrl+e или ctrl+<restart>
	if ctrlKey == "e" || (ctrlKey != "" && len(m.outputTiles) > 0) {
		if len(m.outputTiles) > 0 && m.selectedTileIdx < len(m.outputTiles) {
			tile := m.outputTiles[m.selectedTileIdx]
			var keys KeysConfig
			if tile.isBackground {
				keys = m.bgScripts[tile.index].Keys
			} else {
				keys = m.intScripts[tile.index].Keys
			}
			if ctrlKey == "e" || (keys.Restart != "" && keys.Restart == ctrlKey) {
				if tile.isBackground {
					newTest := restartBgTest(m.bgScripts[tile.index], func() { prog.Send(refreshMsg{}) })
					m.bgScripts[tile.index] = newTest
				} else {
					newTest := restartIntTest(m.intScripts[tile.index], func() { prog.Send(refreshMsg{}) })
					m.intScripts[tile.index] = newTest
				}
				return m, nil
			}
		}
	}

	// Фокусировка по ctrl+<focus>
	if ctrlKey != "" {
		for idx, tile := range m.outputTiles {
			var keys KeysConfig
			if tile.isBackground {
				keys = m.bgScripts[tile.index].Keys
			} else {
				keys = m.intScripts[tile.index].Keys
			}
			if keys.Focus != "" && keys.Focus == ctrlKey {
				m.selectedTileIdx = idx
				return m, nil
			}
		}
	}

	// Глобальный рестарт всех тестов по ctrl+r
	if k == "ctrl+r" {
		restartTests(&m)
		return m, nil
	}

	// Выход из программы по ctrl+q или ESC
	if k == "ctrl+q" || k == "escape" {
		m.quitting = true
		for _, b := range m.bgScripts {
			b.Stop()
		}
		for _, i := range m.intScripts {
			i.Stop()
		}
		return m, tea.Quit
	}

	// Навигация между плитками (ctrl+right / ctrl+left)
	if k == "ctrl+right" && m.mode == modeMain {
		if len(m.outputTiles) > 0 {
			m.selectedTileIdx = (m.selectedTileIdx + 1) % len(m.outputTiles)
			return m, nil
		}
	}
	if k == "ctrl+left" && m.mode == modeMain {
		if len(m.outputTiles) > 0 {
			m.selectedTileIdx = (m.selectedTileIdx - 1 + len(m.outputTiles)) % len(m.outputTiles)
			return m, nil
		}
	}

	// Передача обычных клавиш в PTY активного окна (если оно есть)
	if !strings.HasPrefix(k, "ctrl+") && m.mode == modeMain && len(m.outputTiles) > 0 && m.selectedTileIdx < len(m.outputTiles) {
		tile := m.outputTiles[m.selectedTileIdx]
		if tile.isBackground {
			b := m.bgScripts[tile.index]
			if b.Status == StatusRunning && b.pty != nil {
				b.mutex.Lock()
				sendKeyToPty(b.pty, k)
				b.mutex.Unlock()
				return m, nil
			}
		} else {
			i := m.intScripts[tile.index]
			if i.Status == StatusRunning && i.pty != nil {
				i.mutex.Lock()
				sendKeyToPty(i.pty, k)
				i.mutex.Unlock()
				return m, nil
			}
		}
	}

	return m, nil
}

// ================= buildOutputTiles =================
// Для скриптов со статусом PASSED и FAILED плитки всегда добавляются
func buildOutputTiles(bgs []*BgScript, ints []*IntScript) []outputTile {
	var tiles []outputTile
	// Добавляем интерактивные скрипты, если Output == true
	// Показаны, если:
	//   - статус Running (вывод полного лога)
	//   - статус Failed или Passed (после 3 секунд сворачиваются)
	for i, s := range ints {
		if s.Output {
			switch s.Status {
			case StatusRunning:
				tiles = append(tiles, outputTile{isBackground: false, index: i})
			case StatusFailed, StatusPassed:
				tiles = append(tiles, outputTile{isBackground: false, index: i})
			}
		}
	}

	// Добавляем фоновые скрипты, если Output == true
	for i, s := range bgs {
		if s.Output {
			switch s.Status {
			case StatusRunning:
				tiles = append(tiles, outputTile{isBackground: true, index: i})
			case StatusFailed, StatusPassed:
				tiles = append(tiles, outputTile{isBackground: true, index: i})
			}
		}
	}

	return tiles
}

// ================= RENDER FUNCS =================
func (m model) View() string {
	if m.quitting {
		return ""
	}
	if m.mode == modeFinal {
		return renderFinalScreen(m)
	}
	return renderMainScreen(m)
}

var mainBorder = lipgloss.NewStyle().
	Border(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("244"))

func renderMainScreen(m model) string {
	clear := "\033[2J\033[H"
	leftWidth := (m.width * 40) / 100
	if leftWidth < 20 {
		leftWidth = 20
	}
	rightWidth := m.width - leftWidth - 5
	if rightWidth < 10 {
		rightWidth = 10
	}

	leftPanelContent := renderLeftPanel(m)
	leftPanel := lipgloss.NewStyle().
		Width(leftWidth).
		Height(m.height - 2).
		Render(leftPanelContent)

	rightPanelContent := renderOutputPanel(m)
	rightPanel := lipgloss.NewStyle().
		Width(rightWidth).
		Height(m.height - 2).
		Render(rightPanelContent)

	combined := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, " | ", rightPanel)
	return clear + mainBorder.Render(combined)
}
func renderLeftPanel(m model) string {
	title := asciiBannerMain()
	passed := renderCollapsedByStatus(m, StatusPassed, "PASSED (Collapsed)", passedStyle)
	failed := renderCollapsedByStatus(m, StatusFailed, "FAILED (Collapsed)", failedStyle)
	running := renderRunningList(m)
	hint := footerStyle.Render("\nPress [ctrl+q] or [ESC] to quit | Press [ctrl+r] to restart ALL tests\n" +
		"Press [ctrl+←]/[ctrl+→] to navigate between terminals\n" +
		"Press [ctrl+e] or [ctrl+<restart>] to restart focused test\n")
	// Новый стиль для подсказки custom keys
	customText := aggregateCustomKeys(m)
	customAll := ""
	if customText != "" {
		customAll = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1).
			Render("Custom keys:\n" + customText)
	}

	return strings.Join([]string{
		title,
		"",
		passed,
		failed,
		running,
		hint,
		customAll,
	}, "\n")
}

// ================= STYLES =================
var (
	runningColor = lipgloss.Color("220")
	failedColor  = lipgloss.Color("196")
	passedColor  = lipgloss.Color("42")
	waitColor    = lipgloss.Color("244")
	focusColor   = lipgloss.Color("51")

	failedStyle = lipgloss.NewStyle().Foreground(failedColor).Bold(true)
	passedStyle = lipgloss.NewStyle().Foreground(passedColor).Bold(true)
	focusStyle  = lipgloss.NewStyle().Foreground(focusColor).Bold(true)

	footerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	bannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51"))
)

// Функция для агрегации custom keys в виде дерева по тестам
func aggregateCustomKeys(m model) string {
	var lines []string
	// Фоновые скрипты
	if len(m.bgScripts) > 0 {
		lines = append(lines, "Background Scripts:")
		for _, s := range m.bgScripts {
			if len(s.Keys.Custom) > 0 {
				lines = append(lines, fmt.Sprintf("  %s:", s.Path))
				var keys []string
				for k := range s.Keys.Custom {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					lines = append(lines, fmt.Sprintf("    ctrl+%s => %s", k, s.Keys.Custom[k]))
				}
			}
		}
	}
	// Интерактивные скрипты
	if len(m.intScripts) > 0 {
		lines = append(lines, "Interactive Scripts:")
		for _, s := range m.intScripts {
			if len(s.Keys.Custom) > 0 {
				lines = append(lines, fmt.Sprintf("  %s:", s.Path))
				var keys []string
				for k := range s.Keys.Custom {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					lines = append(lines, fmt.Sprintf("    ctrl+%s => %s", k, s.Keys.Custom[k]))
				}
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func renderCollapsedByStatus(m model, st ScriptStatus, caption string, style lipgloss.Style) string {
	var names []string
	for _, b := range m.bgScripts {
		if b.Status == st {
			names = append(names, b.Path)
		}
	}
	for _, i := range m.intScripts {
		if i.Status == st {
			names = append(names, i.Path)
		}
	}
	if len(names) == 0 {
		return ""
	}
	header := asciiSep(caption)
	body := columnizeCheckBoxes(style.Render("[x]"), names, 4, style)
	return header + "\n" + body
}

func renderRunningList(m model) string {
	var lines []string
	lines = append(lines, asciiSep("RUNNING TESTS"))
	var runningBg, runningInt []string
	for _, b := range m.bgScripts {
		if b.Status == StatusRunning {
			runningBg = append(runningBg, b.Path)
		}
	}
	for _, i := range m.intScripts {
		if i.Status == StatusRunning {
			runningInt = append(runningInt, i.Path)
		}
	}
	lines = append(lines, "===> BG running:")
	if len(runningBg) > 0 {
		lines = append(lines, columnizeCheckBoxes("[ ]", runningBg, 4, lipgloss.NewStyle()))
	}
	lines = append(lines, "===> INT running:")
	if len(runningInt) > 0 {
		lines = append(lines, columnizeCheckBoxes("[ ]", runningInt, 4, lipgloss.NewStyle()))
	}
	return strings.Join(lines, "\n")
}

// ================= TILE BLOCK =================
type tileBlock struct {
	title  string
	lines  []string
	height int
	width  int
}

func makeTileBlock(title string, rawLines []string, maxLogs, outHeight, width int) tileBlock {
	if maxLogs < 1 {
		maxLogs = 1
	}
	if outHeight < 1 {
		outHeight = maxLogs
	}
	return tileBlock{
		title:  title,
		lines:  rawLines,
		height: outHeight,
		width:  width,
	}
}

func (tb tileBlock) render() string {
	var contentLines []string
	if len(tb.lines) > tb.height {
		contentLines = tb.lines[len(tb.lines)-tb.height:]
	} else {
		contentLines = tb.lines
	}
	extra := 4
	maxContentWidth := tb.width - extra
	if maxContentWidth < 1 {
		maxContentWidth = tb.width
	}
	var trimmed []string
	for _, ln := range contentLines {
		if lipgloss.Width(ln) > maxContentWidth {
			runes := []rune(ln)
			ln = string(runes[:maxContentWidth])
		}
		trimmed = append(trimmed, ln)
	}
	for len(trimmed) < tb.height {
		trimmed = append(trimmed, "")
	}
	body := strings.Join(trimmed, "\n")
	border := lipgloss.Border{
		TopLeft:     "┌",
		TopRight:    "┐",
		BottomLeft:  "└",
		BottomRight: "┘",
	}
	borderColor := "240"
	if strings.Contains(tb.title, "SELECTED") {
		borderColor = "51"
	}
	boxStyle := lipgloss.NewStyle().
		Border(border).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1)
	if lipgloss.Width(tb.title) > maxContentWidth {
		runes := []rune(tb.title)
		tb.title = string(runes[:maxContentWidth])
	}
	titleStyle := lipgloss.NewStyle().Bold(true)
	if strings.Contains(tb.title, "SELECTED") {
		titleStyle = titleStyle.Foreground(lipgloss.Color("51"))
	}
	titleStyled := titleStyle.Render(tb.title)
	return titleStyled + "\n" + boxStyle.Render(body)
}

func renderRow(row []tileBlock) string {
	var renderedBlocks []string
	for _, blk := range row {
		renderedBlocks = append(renderedBlocks, blk.render())
	}
	splitLines := make([][]string, len(renderedBlocks))
	maxLines := 0
	for i, blk := range renderedBlocks {
		lines := strings.Split(blk, "\n")
		splitLines[i] = lines
		if len(lines) > maxLines {
			maxLines = len(lines)
		}
	}
	var combined []string
	// Используем пробелы между плитками
	separator := strings.Repeat(" ", 2)
	for i := 0; i < maxLines; i++ {
		var lineParts []string
		for j := range row {
			if i < len(splitLines[j]) {
				lineParts = append(lineParts, splitLines[j][i])
			} else {
				lineParts = append(lineParts, strings.Repeat(" ", lipgloss.Width(splitLines[j][0])))
			}
		}
		combined = append(combined, strings.Join(lineParts, separator))
	}
	return strings.Join(combined, "\n")
}

// ================= FINAL SCREEN =================
func renderFinalScreen(m model) string {
	clear := "\033[2J\033[H"
	banner := asciiBannerFinal()

	// Левая колонка с таблицей результатов
	head := finalTableHeader()
	bgRows := finalRowsBg(m.bgScripts)
	intRows := finalRowsInt(m.intScripts)
	body := strings.Join(append(bgRows, intRows...), "\n")
	foot := finalTableFooter()

	leftPart := strings.Join([]string{head, body, foot}, "\n")

	// Параметры лейаута
	tableWidth := 90 // Примерная ширина левой таблицы

	// Правая колонка с выводом проваленных тестов
	rightPart := renderFailedOutputBlocks(m.bgScripts, m.intScripts, tableWidth)

	// Общий лейаут
	var combinedContent string

	if rightPart != "" {
		// Есть проваленные тесты - используем двухколоночную раскладку
		combinedContent = lipgloss.JoinHorizontal(lipgloss.Top, leftPart, "   ", rightPart)
	} else {
		// Нет проваленных тестов - используем только таблицу
		combinedContent = leftPart
	}

	info := fmt.Sprintf("\nPress [ctrl+q] or [ESC] to quit (exitCode=%d) | Press [ctrl+r] to restart ALL tests\n", m.exitCode)
	return clear + strings.Join([]string{banner, "", combinedContent, info}, "\n")
}

func finalTableHeader() string {
	return `=======================================================
 SCRIPT                | STATUS     | TIME     
=======================================================`
}

func finalTableFooter() string {
	return "======================================================="
}

func finalRowsBg(bgs []*BgScript) []string {
	sort.Slice(bgs, func(i, j int) bool {
		return bgs[i].Duration < bgs[j].Duration
	})
	var out []string
	for _, b := range bgs {
		name := padRight(b.Path, 22)
		statusStr := padRight(statusColorByCode(b.Code), 10)
		tm := fmt.Sprintf("%v", b.Duration.Truncate(100*time.Millisecond))
		out = append(out, fmt.Sprintf(" %s | %s | %s", name, statusStr, tm))
	}
	return out
}

func finalRowsInt(ints []*IntScript) []string {
	sort.Slice(ints, func(i, j int) bool {
		return ints[i].Duration < ints[j].Duration
	})
	var out []string
	for _, i := range ints {
		name := padRight(i.Path, 22)
		statusStr := padRight(statusColorByCode(i.Code), 10)
		tm := fmt.Sprintf("%v", i.Duration.Truncate(100*time.Millisecond))
		out = append(out, fmt.Sprintf(" %s | %s | %s", name, statusStr, tm))
	}
	return out
}

// Функция для рендеринга блоков с выводом проваленных тестов
func renderFailedOutputBlocks(bgs []*BgScript, ints []*IntScript, tableWidth int) string {
	var blocks []string

	// Вычисляем ширину блока на основе ширины таблицы
	blockWidth := tableWidth - 10
	if blockWidth < 40 {
		blockWidth = 40 // Минимальная ширина блока
	}

	// Добавляем блоки для проваленных фоновых скриптов
	for _, b := range bgs {
		if b.Status == StatusFailed && !strings.Contains(strings.ToLower(b.Type), "curses") {
			blocks = append(blocks, renderFailedScriptBlock(b.Path, b.RawLog, blockWidth))
		}
	}

	// Добавляем блоки для проваленных интерактивных скриптов
	for _, i := range ints {
		if i.Status == StatusFailed && !strings.Contains(strings.ToLower(i.Type), "curses") {
			blocks = append(blocks, renderFailedScriptBlock(i.Path, i.RawLog, blockWidth))
		}
	}

	if len(blocks) == 0 {
		return ""
	}

	return strings.Join(blocks, "\n\n")
}

// Функция для рендеринга одного блока с выводом проваленного теста
func renderFailedScriptBlock(path string, rawLog []string, maxWidth int) string {
	lastLines := getLastLines(rawLog, 8) // Берем последние 8 строк вывода
	var nonEmptyLines []string
	for _, line := range lastLines {
		if line != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		}
	}

	if len(nonEmptyLines) == 0 {
		return ""
	}

	// Создаем заголовок и тело блока
	title := failedStyle.Render(fmt.Sprintf(" %s (FAILED) ", path))
	body := strings.Join(nonEmptyLines, "\n")

	// Обрезаем длинные строки
	var trimmedLines []string
	for _, line := range strings.Split(body, "\n") {
		if lipgloss.Width(line) > maxWidth {
			runes := []rune(line)
			if len(runes) > maxWidth {
				line = string(runes[:maxWidth-3]) + "..."
			}
		}
		trimmedLines = append(trimmedLines, line)
	}

	// Собираем содержимое блока
	content := strings.Join(trimmedLines, "\n")

	// Создаем стиль для блока с рамкой
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(failedColor).
		Padding(0, 1).
		Width(maxWidth + 4) // +4 для рамки и отступов

	return title + "\n" + boxStyle.Render(content)
}

func statusColorByCode(code int) string {
	if code == 0 {
		return passedStyle.Render("[PASSED]")
	}
	return failedStyle.Render(fmt.Sprintf("[FAILED=%d]", code))
}

func asciiBannerMain() string {
	return bannerStyle.Render(strings.Join([]string{
		"  __ _               _             _            ",
		" / _(_)             | |           | |           ",
		"| |_ _ _ __ ___  ___| |_ __ _ _ __| |_ ___ _ __ ",
		"|  _| | '__/ _ \\/ __| __/ _` | '__| __/ _ \\ '__|",
		"| | | | | |  __/\\__ \\ || (_| | |  | ||  __/ |   ",
		"|_| |_|_|  \\___||___/\\__\\__,_|_|   \\__\\___|_|   ",
		"",
		"*****---------  Light it all up  ---------*****",
	}, "\n"))
}

func asciiBannerFinal() string {
	return strings.Join([]string{
		"________ ___  ________   ________  ___",
		"|\\  _____\\\\  \\|\\   ___  \\|\\   __  \\|\\  \\",
		"\\ \\  \\__/\\ \\  \\ \\  \\\\ \\  \\ \\  \\|\\  \\ \\  \\",
		" \\ \\   __\\\\ \\  \\ \\  \\\\ \\  \\ \\   __  \\ \\  \\",
		"  \\ \\  \\_| \\ \\  \\ \\  \\\\ \\  \\ \\  \\ \\  \\ \\  \\____",
		"   \\ \\__\\   \\ \\__\\ \\__\\\\ \\__\\ \\__\\ \\__\\ \\_______\\",
		"    \\|__|    \\|__|\\|__| \\|__|\\|__|\\|__|\\|_______|",
		"",
		"*****-----------  FINAL RESULTS  -----------*****",
	}, "\n")
}

func asciiSep(label string) string {
	return fmt.Sprintf("==== %s ====", label)
}

func columnizeCheckBoxes(prefix string, names []string, maxPerCol int, style lipgloss.Style) string {
	if len(names) == 0 {
		return ""
	}
	var columns [][]string
	var col []string
	for i, name := range names {
		col = append(col, fmt.Sprintf("%s %s", prefix, style.Render(name)))
		if (i+1)%maxPerCol == 0 {
			columns = append(columns, col)
			col = []string{}
		}
	}
	if len(col) > 0 {
		columns = append(columns, col)
	}
	var lines []string
	maxH := 0
	for _, c := range columns {
		if len(c) > maxH {
			maxH = len(c)
		}
	}
	for row := 0; row < maxH; row++ {
		var parts []string
		for _, c := range columns {
			if row < len(c) {
				parts = append(parts, c[row])
			} else {
				parts = append(parts, "")
			}
		}
		lines = append(lines, strings.Join(parts, "    "))
	}
	return strings.Join(lines, "\n")
}

func padRight(s string, width int) string {
	n := lipgloss.Width(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

func computeExitCode(bgs []*BgScript, ints []*IntScript) int {
	for _, b := range bgs {
		if !b.Info && b.Status == StatusFailed {
			return 1
		}
	}
	for _, i := range ints {
		if !i.Info && i.Status == StatusFailed {
			return 1
		}
	}
	return 0
}

func allScriptsDone(bgs []*BgScript, ints []*IntScript) bool {
	for _, b := range bgs {
		if !b.Info && (b.Status == StatusWaiting || b.Status == StatusRunning) {
			return false
		}
	}
	for _, i := range ints {
		if !i.Info && (i.Status == StatusWaiting || i.Status == StatusRunning) {
			return false
		}
	}
	return true
}

// ================= SEND KEY (PTY) =================
func sendKeyToPty(pty *os.File, k string) {
	switch k {
	case "enter":
		pty.Write([]byte("\r"))
	case "backspace":
		pty.Write([]byte("\b"))
	case "tab":
		pty.Write([]byte("\t"))
	case "escape":
		pty.Write([]byte("\x1b"))
	case "up", "down", "left", "right":
		arrowMap := map[string]string{
			"up":    "\x1b[A",
			"down":  "\x1b[B",
			"right": "\x1b[C",
			"left":  "\x1b[D",
		}
		pty.Write([]byte(arrowMap[k]))
	case "home":
		pty.Write([]byte("\x1b[H"))
	case "end":
		pty.Write([]byte("\x1b[F"))
	case "pgup":
		pty.Write([]byte("\x1b[5~"))
	case "pgdown":
		pty.Write([]byte("\x1b[6~"))
	case "delete":
		pty.Write([]byte("\x1b[3~"))
	case "insert":
		pty.Write([]byte("\x1b[2~"))
	case "f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12":
		fkeyMap := map[string]string{
			"f1":  "\x1bOP",
			"f2":  "\x1bOQ",
			"f3":  "\x1bOR",
			"f4":  "\x1bOS",
			"f5":  "\x1b[15~",
			"f6":  "\x1b[17~",
			"f7":  "\x1b[18~",
			"f8":  "\x1b[19~",
			"f9":  "\x1b[20~",
			"f10": "\x1b[21~",
			"f11": "\x1b[23~",
			"f12": "\x1b[24~",
		}
		pty.Write([]byte(fkeyMap[k]))
	case "space":
		pty.Write([]byte(" "))
	default:
		if len(k) == 1 {
			pty.Write([]byte(k))
		} else {
			bareLog.Printf("Unhandled key: %s", k)
		}
	}
}

// ================= PARSERS & HELPERS =================
func parseArgs(s string) []string {
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Split(bufio.ScanWords)
	var args []string
	for scanner.Scan() {
		args = append(args, scanner.Text())
	}
	return args
}

func parseParams(seq string) []string {
	if seq == "" {
		return []string{""}
	}
	return strings.Split(seq, ";")
}

func atoiParam(params []string, index int, defaultVal int) int {
	if index >= len(params) || params[index] == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(params[index])
	if err != nil {
		return defaultVal
	}
	return n
}

func clamp(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

// ================= MAIN (ENTRY POINT) =================
var prog *tea.Program

func main() {
	// Определяем флаг командной строки для указания пути к конфигурационному файлу
	configPath := flag.String("config", "config.json", "Path to configuration file")
	flag.Parse()

	// Загружаем конфигурацию из указанного файла
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("Error reading %s: %v", *configPath, err)
		os.Exit(1)
	}
	if cfg == nil {
		log.Printf("%s is empty or invalid", *configPath)
		os.Exit(1)
	}
	globalConfig = cfg

	// Логи
	fBare, err := os.OpenFile("bare_log.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Error opening bare_log.log: %v", err)
	}
	bareLog = log.New(fBare, "", log.LstdFlags)

	fDebug, err := os.OpenFile("debug_log.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Error opening debug_log.log: %v", err)
	}
	debugLog = log.New(fDebug, "DEBUG: ", log.LstdFlags)

	width, height := 80, 24

	// Инициализируем массивы скриптов
	var bgScripts []*BgScript
	for i, sc := range cfg.BackgroundScripts {
		maxLogs := sc.MaxLogs
		if maxLogs <= 0 {
			maxLogs = 5
		}
		isCurses := strings.Contains(strings.ToLower(sc.Type), "curses")
		h, w, err := parseOutputRes(sc.OutputRes, isCurses)
		if err != nil {
			bareLog.Printf("Config error for %s: %v, using defaults", sc.Path, err)
			h, w = 10, 40
		}
		parts := strings.Split(sc.Type, ",")
		baseType := strings.TrimSpace(parts[0])
		infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
		bgScripts = append(bgScripts, &BgScript{
			Path:        sc.Path,
			Args:        sc.Args,
			Type:        baseType,
			Info:        infoFlag,
			Status:      StatusWaiting,
			Code:        -1,
			RawLog:      []string{},
			MaxLogs:     maxLogs,
			Output:      sc.Output,
			OutHeight:   h,
			OutWidth:    w,
			OutputRes:   sc.OutputRes,
			Collapse:    sc.Collapse,
			Keys:        sc.Keys,
			ConfigIndex: i,
		})
	}

	var intScripts []*IntScript
	for i, sc := range cfg.InteractiveScripts {
		maxLogs := sc.MaxLogs
		if maxLogs <= 0 {
			maxLogs = 5
		}
		isCurses := strings.Contains(strings.ToLower(sc.Type), "curses")
		h, w, err := parseOutputRes(sc.OutputRes, isCurses)
		if err != nil {
			bareLog.Printf("Config error for %s: %v, using defaults", sc.Path, err)
			h, w = 10, 40
		}
		parts := strings.Split(sc.Type, ",")
		baseType := strings.TrimSpace(parts[0])
		infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
		intScripts = append(intScripts, &IntScript{
			Path:        sc.Path,
			Args:        sc.Args,
			Type:        baseType,
			Info:        infoFlag,
			Status:      StatusWaiting,
			Code:        -1,
			RawLog:      []string{},
			MaxLogs:     maxLogs,
			Output:      sc.Output,
			OutHeight:   h,
			OutWidth:    w,
			OutputRes:   sc.OutputRes,
			Collapse:    sc.Collapse,
			Keys:        sc.Keys,
			ConfigIndex: i,
		})
	}

	// Модель Bubble Tea
	m := model{
		bgScripts:       bgScripts,
		intScripts:      intScripts,
		mode:            modeMain,
		exitCode:        0,
		width:           width,
		height:          height,
		outputTiles:     []outputTile{},
		selectedTileIdx: 0,
	}

	// Запуск Bubble Tea
	var opts []tea.ProgramOption
	if isatty.IsTerminal(os.Stdin.Fd()) {
		opts = append(opts, tea.WithAltScreen())
	}
	prog = tea.NewProgram(m, opts...)

	go func() {
		if _, err := prog.Run(); err != nil {
			log.Printf("BubbleTea error: %v", err)
		}
		os.Exit(m.exitCode)
	}()

	// Запускаем все скрипты
	notifyFn := func() {
		if allScriptsDone(m.bgScripts, m.intScripts) {
			prog.Send(doneAllMsg{})
		} else {
			prog.Send(refreshMsg{})
		}
	}

	var wgAll sync.WaitGroup
	for _, b := range m.bgScripts {
		wgAll.Add(1)
		go b.Start(&wgAll, notifyFn)
	}
	for _, i := range m.intScripts {
		wgAll.Add(1)
		go i.Start(&wgAll, notifyFn)
	}

	// Блокируемся
	select {}
}

// ================= RESTART TESTS (ALL) =================
func restartTests(m *model) {
	newBg := []*BgScript{}
	for i, sc := range globalConfig.BackgroundScripts {
		maxLogs := sc.MaxLogs
		if maxLogs <= 0 {
			maxLogs = 5
		}
		isCurses := strings.Contains(strings.ToLower(sc.Type), "curses")
		h, w, err := parseOutputRes(sc.OutputRes, isCurses)
		if err != nil {
			bareLog.Printf("Config error for %s: %v, using defaults", sc.Path, err)
			h, w = 10, 40
		}
		parts := strings.Split(sc.Type, ",")
		baseType := strings.TrimSpace(parts[0])
		infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
		newBg = append(newBg, &BgScript{
			Path:        sc.Path,
			Args:        sc.Args,
			Type:        baseType,
			Info:        infoFlag,
			Status:      StatusWaiting,
			Code:        -1,
			RawLog:      []string{},
			MaxLogs:     maxLogs,
			Output:      sc.Output,
			OutHeight:   h,
			OutWidth:    w,
			OutputRes:   sc.OutputRes,
			Collapse:    sc.Collapse,
			Keys:        sc.Keys,
			ConfigIndex: i,
		})
	}
	newInt := []*IntScript{}
	for i, sc := range globalConfig.InteractiveScripts {
		maxLogs := sc.MaxLogs
		if maxLogs <= 0 {
			maxLogs = 5
		}
		isCurses := strings.Contains(strings.ToLower(sc.Type), "curses")
		h, w, err := parseOutputRes(sc.OutputRes, isCurses)
		if err != nil {
			bareLog.Printf("Config error for %s: %v, using defaults", sc.Path, err)
			h, w = 10, 40
		}
		parts := strings.Split(sc.Type, ",")
		baseType := strings.TrimSpace(parts[0])
		infoFlag := len(parts) > 1 && strings.TrimSpace(parts[1]) == "info"
		newInt = append(newInt, &IntScript{
			Path:        sc.Path,
			Args:        sc.Args,
			Type:        baseType,
			Info:        infoFlag,
			Status:      StatusWaiting,
			Code:        -1,
			RawLog:      []string{},
			MaxLogs:     maxLogs,
			Output:      sc.Output,
			OutHeight:   h,
			OutWidth:    w,
			OutputRes:   sc.OutputRes,
			Collapse:    sc.Collapse,
			Keys:        sc.Keys,
			ConfigIndex: i,
		})
	}
	m.bgScripts = newBg
	m.intScripts = newInt
	m.mode = modeMain
	m.exitCode = 0
	m.outputTiles = []outputTile{}
	m.selectedTileIdx = 0

	var wgAll sync.WaitGroup
	notifyFn := func() {
		if allScriptsDone(m.bgScripts, m.intScripts) {
			prog.Send(doneAllMsg{})
		} else {
			prog.Send(refreshMsg{})
		}
	}
	for _, b := range m.bgScripts {
		wgAll.Add(1)
		go b.Start(&wgAll, notifyFn)
	}
	for _, i := range m.intScripts {
		wgAll.Add(1)
		go i.Start(&wgAll, notifyFn)
	}
}

func loadConfig(fname string) (*Config, error) {
	data, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Улучшенный парсинг размеров с учётом пробелов
func parseOutputRes(val string, isCurses bool) (int, int, error) {
	val = strings.TrimSpace(val)
	val = strings.ReplaceAll(val, " ", "")
	defaultH, defaultW := 10, 40

	// Если строка пустая, возвращаем значения по умолчанию
	if val == "" {
		return defaultH, defaultW, nil
	}

	parts := strings.Split(val, "x")
	if len(parts) != 2 {
		return defaultH, defaultW, fmt.Errorf("invalid output_res format: %s", val)
	}

	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return defaultH, defaultW, fmt.Errorf("invalid height in %s: %v", val, err)
	}

	wPart := parts[1]

	// Для curses-приложений всегда требуется точный размер
	if isCurses {
		w, err := strconv.Atoi(wPart)
		if err != nil {
			return h, defaultW, fmt.Errorf("curses requires numeric width in %s: %v", val, err)
		}
		return h, w, nil
	}

	// Для не-curses обрабатываем специальные значения
	if wPart == "*" {
		return h, defaultW, nil
	}
	if strings.ToUpper(wPart) == "S" {
		return h, 0, nil
	}

	// Пробуем преобразовать в число
	w, err := strconv.Atoi(wPart)
	if err != nil {
		// Если не число, используем значение по умолчанию
		return h, defaultW, nil
	}

	return h, w, nil
}

// Больше не нужен из-за нового подхода - фиксированное число плиток в строке
func arrangeBlocksInRows(blocks []tileBlock, availableWidth int) [][]tileBlock {
	if len(blocks) == 0 {
		return nil
	}

	// Используем фиксированное количество плиток в строке
	tilesPerRow := 2
	if availableWidth > 120 {
		tilesPerRow = 3
	}

	var rows [][]tileBlock
	for i := 0; i < len(blocks); i += tilesPerRow {
		end := i + tilesPerRow
		if end > len(blocks) {
			end = len(blocks)
		}
		rows = append(rows, blocks[i:end])
	}

	return rows
}

// Полностью переработанный подход к рендерингу вывода
// Выравнивает плитки строго по левому краю с фиксированной шириной для каждой плитки в ряду
func renderOutputPanel(m model) string {
	var blocks []tileBlock
	if len(m.outputTiles) == 0 {
		m.outputTiles = buildOutputTiles(m.bgScripts, m.intScripts)
	}

	availableWidth := m.width - (m.width*40)/100 - 10
	if availableWidth < 20 {
		availableWidth = 20
	}

	// Сбор информации о плитках
	for idx, tile := range m.outputTiles {
		var script interface{}
		if tile.isBackground {
			script = m.bgScripts[tile.index]
		} else {
			script = m.intScripts[tile.index]
		}

		var (
			path      string
			content   string
			isCurses  bool
			maxLogs   int
			outHeight int
			outWidth  int
			collapse  bool
		)

		if bg, ok := script.(*BgScript); ok {
			path = bg.Path
			isCurses = strings.Contains(strings.ToLower(bg.Type), "curses")
			collapse = bg.Collapse
			if bg.Status != StatusRunning {
				if !collapse || time.Since(bg.FinishedAt) < 3*time.Second {
					if isCurses && bg.vtBuffer != nil {
						content = bg.vtBuffer.RenderVisible()
					} else {
						content = strings.Join(bg.RawLog, "\n")
					}
					outHeight = bg.OutHeight
				} else {
					content = fmt.Sprintf("Скрипт завершён: %s", bg.Status.String())
					outHeight = 1
				}
			} else if isCurses && bg.vtBuffer != nil {
				content = bg.vtBuffer.RenderVisible()
				outHeight = bg.OutHeight
			} else {
				content = strings.Join(bg.RawLog, "\n")
				outHeight = bg.OutHeight
			}
			maxLogs = bg.MaxLogs
			outWidth = bg.OutWidth
		} else if in, ok := script.(*IntScript); ok {
			path = in.Path
			isCurses = strings.Contains(strings.ToLower(in.Type), "curses")
			collapse = in.Collapse
			if in.Status != StatusRunning {
				if !collapse || time.Since(in.FinishedAt) < 3*time.Second {
					if isCurses && in.vtBuffer != nil {
						content = in.vtBuffer.RenderVisible()
					} else {
						content = strings.Join(in.RawLog, "\n")
					}
					outHeight = in.OutHeight
				} else {
					content = fmt.Sprintf("Скрипт завершён: %s", in.Status.String())
					outHeight = 1
				}
			} else if isCurses && in.vtBuffer != nil {
				content = in.vtBuffer.RenderVisible()
				outHeight = in.OutHeight
			} else {
				content = strings.Join(in.RawLog, "\n")
				outHeight = in.OutHeight
			}
			maxLogs = in.MaxLogs
			outWidth = in.OutWidth
		}

		title := path
		if idx == m.selectedTileIdx {
			title = fmt.Sprintf("[SELECTED] %s", path)
		}

		// Базовая ширина для плитки
		tileWidth := availableWidth / 2
		if isCurses {
			tileWidth = outWidth + 4
		}

		tb := makeTileBlock(title, strings.Split(content, "\n"), maxLogs, outHeight, tileWidth)
		blocks = append(blocks, tb)
	}

	if len(blocks) == 0 {
		return ""
	}

	// Количество плиток в строке (фиксированное значение для стабильности)
	tilesPerRow := 2
	if availableWidth > 120 {
		tilesPerRow = 3
	}

	// Группируем плитки по строкам
	var rows [][]tileBlock
	for i := 0; i < len(blocks); i += tilesPerRow {
		end := i + tilesPerRow
		if end > len(blocks) {
			end = len(blocks)
		}
		rows = append(rows, blocks[i:end])
	}

	// Результат вывода
	var result []string

	// Для каждого ряда плиток
	for _, row := range rows {
		// Отрендерим каждую плитку отдельно
		renderedTiles := make([]string, len(row))
		for i, tile := range row {
			renderedTiles[i] = renderRawTile(tile)
		}

		// Разбиваем отрендеренные плитки на строки
		tileLines := make([][]string, len(renderedTiles))
		for i, rendered := range renderedTiles {
			tileLines[i] = strings.Split(rendered, "\n")
		}

		// Находим максимальную ширину строки в каждой плитке
		tileWidths := make([]int, len(row))
		for i, lines := range tileLines {
			maxWidth := 0
			for _, line := range lines {
				width := lipgloss.Width(line)
				if width > maxWidth {
					maxWidth = width
				}
			}
			tileWidths[i] = maxWidth
		}

		// Находим максимальное количество строк в плитках ряда
		maxLines := 0
		for _, lines := range tileLines {
			if len(lines) > maxLines {
				maxLines = len(lines)
			}
		}

		// Формируем строки вывода для этого ряда
		rowOutput := make([]string, maxLines)
		for i := 0; i < maxLines; i++ {
			var lineParts []string

			for j, lines := range tileLines {
				var part string
				if i < len(lines) {
					// Берем существующую строку
					part = lines[i]
				} else {
					// Заполняем пробелами, если строки нет
					part = strings.Repeat(" ", tileWidths[j])
				}

				// Выравниваем ширину каждой части
				currentWidth := lipgloss.Width(part)
				if currentWidth < tileWidths[j] {
					part += strings.Repeat(" ", tileWidths[j]-currentWidth)
				}

				lineParts = append(lineParts, part)
			}

			// Собираем строку с отступами между плитками
			rowOutput[i] = strings.Join(lineParts, "  ")
		}

		// Добавляем ряд в результат
		result = append(result, strings.Join(rowOutput, "\n"))
	}

	return strings.Join(result, "\n\n")
}

// Функция для рендеринга сырой плитки без дополнительного выравнивания
func renderRawTile(tb tileBlock) string {
	// Подготовка содержимого
	var contentLines []string
	if len(tb.lines) > tb.height {
		contentLines = tb.lines[len(tb.lines)-tb.height:]
	} else {
		contentLines = tb.lines
	}

	// Максимальная ширина для содержимого
	maxContentWidth := tb.width - 4 // Учитываем рамку и отступы
	if maxContentWidth < 1 {
		maxContentWidth = 1
	}

	// Обрезаем длинные строки, но не добавляем пробелы
	var processedLines []string
	for _, line := range contentLines {
		if lipgloss.Width(line) > maxContentWidth {
			runes := []rune(line)
			if len(runes) > maxContentWidth {
				line = string(runes[:maxContentWidth])
			}
		}
		processedLines = append(processedLines, line)
	}

	// Дополняем пустыми строками до нужной высоты
	for len(processedLines) < tb.height {
		processedLines = append(processedLines, "")
	}

	// Цвет рамки зависит от выбранности
	borderColor := lipgloss.Color("240")
	if strings.Contains(tb.title, "SELECTED") {
		borderColor = lipgloss.Color("51")
	}

	// Создаем стиль рамки
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)

	// Рендерим тело
	body := boxStyle.Render(strings.Join(processedLines, "\n"))

	// Стиль для заголовка
	titleStyle := lipgloss.NewStyle().Bold(true)
	if strings.Contains(tb.title, "SELECTED") {
		titleStyle = titleStyle.Foreground(lipgloss.Color("51"))
	}

	// Обрезаем длинный заголовок
	title := tb.title
	if lipgloss.Width(title) > maxContentWidth {
		runes := []rune(title)
		if len(runes) > maxContentWidth {
			title = string(runes[:maxContentWidth])
		}
	}

	// Рендерим заголовок
	titleRendered := titleStyle.Render(title)

	// Возвращаем готовую плитку
	return titleRendered + "\n" + body
}
