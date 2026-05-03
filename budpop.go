package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const timeLayout = "2006-01-02 15:04:05.000"

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	metaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	panelStyle  = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("39")).Padding(0, 1)
)

type packetMsg struct {
	timestamp time.Time
	remote    *net.UDPAddr
	payload   []byte
}

type packetErrMsg struct {
	err error
}

type sendResultMsg struct {
	timestamp time.Time
	sourceIP  string
	body      string
	err       error
}

type model struct {
	conn       *net.UDPConn
	dest       *net.UDPAddr
	logFile    *os.File
	logPath    string
	recv       chan tea.Msg
	input      textinput.Model
	viewport   viewport.Model
	logs       []string
	status     string
	errText    string
	listenPort string
	broadcast  bool
	ready      bool
	width      int
	height     int
}

func main() {
	broadcast := flag.Bool("b", false, "enable broadcast")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 3 {
		usage()
	}

	listenPort := flag.Arg(0)
	destination := flag.Arg(1)
	destPort := flag.Arg(2)

	conn, dest, err := openSocket(listenPort, destination, destPort, *broadcast)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budpop: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	logFile, logPath, err := openAuditLog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "budpop: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	recv := make(chan tea.Msg, 32)
	go readLoop(conn, recv)

	input := textinput.New()
	input.Placeholder = "Type a message and press return to send"
	input.Focus()
	input.CharLimit = 1024
	input.Prompt = "> "

	m := model{
		conn:       conn,
		dest:       dest,
		logFile:    logFile,
		logPath:    logPath,
		recv:       recv,
		input:      input,
		status:     fmt.Sprintf("listening on 0.0.0.0:%s, sending to %s", listenPort, dest.String()),
		listenPort: listenPort,
		broadcast:  *broadcast,
	}
	readyLog := fmt.Sprintf("[%s] system ready src=local dst=%s note=%s", time.Now().Format(timeLayout), dest.String(), sanitizeMessage("TUI started"))
	m.appendLog(readyLog, readyLog)
	m.status = fmt.Sprintf("listening on 0.0.0.0:%s, sending to %s", listenPort, dest.String())

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "budpop: %v\n", err)
		os.Exit(1)
	}

	if err := writeAuditLine(logFile, fmt.Sprintf("[%s] system stop src=local note=%s", time.Now().Format(timeLayout), sanitizeMessage("TUI exited"))); err != nil {
		fmt.Fprintf(os.Stderr, "budpop: write audit log: %v\n", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: budpop [-b] <listen> <destination> <port>")
	os.Exit(1)
}

func openSocket(listenPort, destination, destPort string, broadcast bool) (*net.UDPConn, *net.UDPAddr, error) {
	listenAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort("0.0.0.0", listenPort))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve listen address: %w", err)
	}

	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen udp: %w", err)
	}

	if broadcast {
		if err := enableBroadcast(conn); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("enable broadcast: %w", err)
		}
	}

	dest, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(destination, destPort))
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("resolve destination: %w", err)
	}

	return conn, dest, nil
}

func openAuditLog() (*os.File, string, error) {
	logPath, err := filepath.Abs("budpop.log")
	if err != nil {
		return nil, "", fmt.Errorf("resolve audit log path: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("open audit log: %w", err)
	}

	return logFile, logPath, nil
}

func writeAuditLine(logFile *os.File, entry string) error {
	if logFile == nil {
		return nil
	}

	if _, err := fmt.Fprintln(logFile, entry); err != nil {
		return err
	}

	return nil
}

func enableBroadcast(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var controlErr error
	if err := rawConn.Control(func(fd uintptr) {
		controlErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}

	return controlErr
}

func readLoop(conn *net.UDPConn, recv chan<- tea.Msg) {
	buf := make([]byte, 65535)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			recv <- packetErrMsg{err: err}
			return
		}

		payload := make([]byte, n)
		copy(payload, buf[:n])
		recv <- packetMsg{timestamp: time.Now(), remote: remote, payload: payload}
	}
}

func waitForPacket(recv <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-recv
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, waitForPacket(m.recv))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resize()
		return m, nil

	case packetMsg:
		m.errText = ""
		m.appendLog(formatReceiveLog(msg.timestamp, msg.remote, string(msg.payload), true), formatReceiveLog(msg.timestamp, msg.remote, string(msg.payload), false))
		return m, waitForPacket(m.recv)

	case packetErrMsg:
		m.errText = msg.err.Error()
		return m, nil

	case sendResultMsg:
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		m.errText = ""
		m.appendLog(formatSendLog(msg.timestamp, msg.sourceIP, m.dest, msg.body, true), formatSendLog(msg.timestamp, msg.sourceIP, m.dest, msg.body, false))
		m.status = fmt.Sprintf("sent to %s", m.dest.String())
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "pgup":
			m.viewport.LineUp(max(3, m.viewport.Height-1))
			return m, nil
		case "pgdown":
			m.viewport.LineDown(max(3, m.viewport.Height-1))
			return m, nil
		case "ctrl+l":
			m.logs = nil
			m.viewport.SetContent("")
			m.errText = ""
			m.status = "logs cleared"
			return m, nil
		case "enter":
			body := strings.TrimSpace(m.input.Value())
			if body == "" {
				m.status = "message is empty"
				return m, nil
			}
			m.input.SetValue("")
			m.status = fmt.Sprintf("sending to %s", m.dest.String())
			return m, sendMessage(m.conn, m.dest, body)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) resize() {
	if !m.ready {
		return
	}

	contentWidth := max(20, m.width-4)
	contentHeight := max(8, m.height-9)

	m.viewport.Width = max(10, contentWidth-2)
	m.viewport.Height = contentHeight
	m.input.Width = max(10, contentWidth-6)
	m.refreshViewport()
	m.viewport.GotoBottom()
}

func (m *model) appendLog(displayEntry, auditEntry string) {
	m.logs = append(m.logs, displayEntry)
	m.refreshViewport()
	m.viewport.GotoBottom()
	m.status = fmt.Sprintf("%d message(s) in log", len(m.logs))
	if err := writeAuditLine(m.logFile, auditEntry); err != nil {
		m.errText = fmt.Sprintf("write audit log: %v", err)
	}
}

func (m *model) refreshViewport() {
	if len(m.logs) == 0 {
		m.viewport.SetContent("")
		return
	}

	wrapped := make([]string, 0, len(m.logs))
	wrapStyle := lipgloss.NewStyle().Width(max(1, m.viewport.Width))
	for _, entry := range m.logs {
		wrapped = append(wrapped, wrapStyle.Render(entry))
	}

	m.viewport.SetContent(strings.Join(wrapped, "\n\n"))
}

func (m model) View() string {
	if !m.ready {
		return "Loading budpop..."
	}

	header := lipgloss.JoinVertical(lipgloss.Left,
		headerStyle.Render("budpop"),
		metaStyle.Render(fmt.Sprintf("listen :%s | dest %s | broadcast %t", m.listenPort, m.dest.String(), m.broadcast)),
		metaStyle.Render(fmt.Sprintf("audit %s", m.logPath)),
		metaStyle.Render("return: send, PgUp/PgDn: scroll log, ^L: clear log, ^C: bye"),
	)

	logPanel := panelStyle.Width(max(20, m.width-2)).Height(m.viewport.Height + 2).Render(m.viewport.View())
	inputPanel := inputStyle.Width(max(20, m.width-2)).Render(m.input.View())

	statusText := m.status
	if m.errText != "" {
		statusText = errorStyle.Render(m.errText)
	} else {
		statusText = metaStyle.Render(statusText)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, logPanel, inputPanel, statusText)
}

func sendMessage(conn *net.UDPConn, dest *net.UDPAddr, body string) tea.Cmd {
	message := body
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return func() tea.Msg {
		if _, err := conn.WriteToUDP([]byte(message), dest); err != nil {
			return sendResultMsg{err: fmt.Errorf("send udp: %w", err)}
		}
		return sendResultMsg{
			timestamp: time.Now(),
			sourceIP:  outboundIP(dest),
			body:      message,
		}
	}
}

func outboundIP(dest *net.UDPAddr) string {
	probe, err := net.DialUDP("udp4", nil, dest)
	if err == nil {
		defer probe.Close()
		if local, ok := probe.LocalAddr().(*net.UDPAddr); ok && local.IP != nil {
			return local.IP.String()
		}
	}

	if local := firstUsableIPv4(); local != "" {
		return local
	}

	return "unknown"
}

func firstUsableIPv4() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip == nil {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
		}
	}

	return ""
}

func formatReceiveLog(ts time.Time, remote *net.UDPAddr, body string, multiline bool) string {
	prefix := fmt.Sprintf("[%s] recv src=%s:%d", ts.Format(timeLayout), remote.IP.String(), remote.Port)
	return formatMessageLog(prefix, body, multiline)
}

func formatSendLog(ts time.Time, sourceIP string, dest *net.UDPAddr, body string, multiline bool) string {
	prefix := fmt.Sprintf("[%s] send src=%s dst=%s:%d", ts.Format(timeLayout), sourceIP, dest.IP.String(), dest.Port)
	return formatMessageLog(prefix, body, multiline)
}

func formatMessageLog(prefix, body string, multiline bool) string {
	if multiline {
		return fmt.Sprintf("%s\nmsg=%s", prefix, sanitizeMessage(body))
	}

	return fmt.Sprintf("%s msg=%s", prefix, sanitizeMessage(body))
}

func sanitizeMessage(body string) string {
	trimmed := strings.TrimRight(body, "\r\n")
	if trimmed == "" {
		return "(empty)"
	}
	replaced := strings.ReplaceAll(trimmed, "\r", "\\r")
	replaced = strings.ReplaceAll(replaced, "\n", "\\n")
	return replaced
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
