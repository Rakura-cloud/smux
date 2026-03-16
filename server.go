package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/creack/pty"
)

func runServer(session string) error {
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		return err
	}
	path := socketPathForSession(session)
	if _, err := os.Stat(path); err == nil {
		if serverRunning(path) {
			return fmt.Errorf("session already running: %s", session)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	srv := newServer(session, path, ln)
	if err := srv.bootstrap(); err != nil {
		return err
	}
	return srv.run()
}

func newServer(session, path string, ln net.Listener) *Server {
	settings := loadSettings()
	presetIdx := settings.PanePresetIdx
	if presetIdx < 0 || presetIdx >= len(paneWidthPresets) {
		presetIdx = 0
	}

	currentUser, _ := user.Current()
	username := "user"
	homeDir := ""
	if currentUser != nil {
		username = currentUser.Username
		homeDir = currentUser.HomeDir
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}

	cols, rows := getTerminalSize()
	viewportW := cols - SidebarWidth - 2
	if viewportW < 20 {
		viewportW = 80
	}
	viewportH := rows - 6
	if viewportH < 10 {
		viewportH = 24
	}

	return &Server{
		session:       session,
		path:          path,
		listener:      ln,
		state:         &AppState{Workspaces: make([]*Workspace, 0), ActiveWSIdx: 0},
		panePresetIdx: presetIdx,
		viewportWidth: viewportW,
		viewportHgt:   viewportH,
		clients:       make(map[int]*serverClient),
		username:      username,
		hostname:      hostname,
		homeDir:       homeDir,
		broadcastCh:   make(chan struct{}, 1),
		stoppedCh:     make(chan struct{}),
	}
}

func (s *Server) bootstrap() error {
	initialColor := autoWorkspaceColor(s.state.Workspaces)
	ws := &Workspace{
		Name:          "Workspace 1",
		Color:         initialColor.Color,
		ColorName:     initialColor.Name,
		Panes:         make([]*Pane, 0),
		ActivePaneIdx: 0,
	}
	s.state.Workspaces = append(s.state.Workspaces, ws)
	if s.spawnPaneLocked(ws, 0) == nil {
		return fmt.Errorf("failed to create initial pane")
	}
	s.applyLayoutLocked()
	return nil
}

func (s *Server) run() error {
	defer close(s.stoppedCh)
	go s.frameLoop()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			stopping := s.stopping
			s.mu.Unlock()
			if stopping {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		go s.handleClient(conn)
	}
}

func (s *Server) frameLoop() {
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.broadcastSnapshot()
		case <-s.broadcastCh:
			s.broadcastSnapshot()
		case <-s.stoppedCh:
			return
		}
	}
}

func (s *Server) requestBroadcast() {
	select {
	case s.broadcastCh <- struct{}{}:
	default:
	}
}

func (s *Server) broadcastSnapshot() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	msg := s.buildSnapshotLocked()
	clients := make([]*serverClient, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		select {
		case c.sendCh <- msg:
		default:
		}
	}
}

func (s *Server) buildSnapshotLocked() ServerMsg {
	msg := ServerMsg{
		Type:           "snapshot",
		Workspaces:     make([]WorkspaceInfo, 0, len(s.state.Workspaces)),
		ActiveWSIdx:    s.state.ActiveWSIdx,
		PanePresetIdx:  s.panePresetIdx,
		PanePresetName: paneWidthPresets[s.panePresetIdx].Name,
	}
	for i, w := range s.state.Workspaces {
		if w == nil {
			continue
		}
		msg.Workspaces = append(msg.Workspaces, WorkspaceInfo{
			Name:       w.Name,
			PaneCount:  len(w.Panes),
			IsActive:   i == s.state.ActiveWSIdx,
			ColorName:  w.ColorName,
			ColorValue: int64(w.Color),
		})
	}
	if len(s.state.Workspaces) == 0 || s.state.ActiveWSIdx >= len(s.state.Workspaces) {
		return msg
	}
	ws := s.state.Workspaces[s.state.ActiveWSIdx]
	if ws == nil {
		return msg
	}
	msg.Panes = make([]PaneSnapshot, 0, len(ws.Panes))
	for i, p := range ws.Panes {
		if p == nil {
			continue
		}
		cells := p.VT.GetCells(p.ScrollOff)
		wireCells := make([][]WireCell, len(cells))
		for r := range cells {
			wireCells[r] = make([]WireCell, len(cells[r]))
			for c := range cells[r] {
				wireCells[r][c] = cellToWire(cells[r][c])
			}
		}
		cr, cc, cv := p.VT.CursorPos()
		msg.Panes = append(msg.Panes, PaneSnapshot{
			Cells:         wireCells,
			Title:         getPaneTitle(p, i, s.username, s.hostname, s.homeDir),
			VirtualX:      p.VirtualX,
			Width:         p.Width,
			Cols:          p.Cols,
			Rows:          p.Rows,
			IsActive:      i == ws.ActivePaneIdx,
			CursorRow:     cr,
			CursorCol:     cc,
			CursorVisible: cv,
			ScrollOff:     p.ScrollOff,
			AppCursorKeys: p.VT.ApplicationCursorKeys(),
		})
	}
	return msg
}

func (s *Server) handleClient(conn net.Conn) {
	s.mu.Lock()
	id := s.nextClientID
	s.nextClientID++
	client := &serverClient{id: id, conn: conn, sendCh: make(chan ServerMsg, 8)}
	s.clients[id] = client
	s.mu.Unlock()

	go func(c *serverClient) {
		for msg := range c.sendCh {
			c.mu.Lock()
			err := sendMsg(c.conn, msg)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}(client)

	s.requestBroadcast()

	for {
		var msg ClientMsg
		if err := recvMsg(conn, &msg); err != nil {
			break
		}
		detach, stop := s.handleClientMsg(id, msg)
		if stop || detach {
			break
		}
	}

	s.mu.Lock()
	delete(s.clients, id)
	s.mu.Unlock()
	_ = conn.Close()
	close(client.sendCh)
}

func (s *Server) activeWorkspaceLocked() *Workspace {
	if len(s.state.Workspaces) == 0 {
		return nil
	}
	if s.state.ActiveWSIdx < 0 {
		s.state.ActiveWSIdx = 0
	}
	if s.state.ActiveWSIdx >= len(s.state.Workspaces) {
		s.state.ActiveWSIdx = len(s.state.Workspaces) - 1
	}
	return s.state.Workspaces[s.state.ActiveWSIdx]
}

func (s *Server) activePaneLocked() *Pane {
	ws := s.activeWorkspaceLocked()
	if ws == nil || len(ws.Panes) == 0 {
		return nil
	}
	if ws.ActivePaneIdx < 0 {
		ws.ActivePaneIdx = 0
	}
	if ws.ActivePaneIdx >= len(ws.Panes) {
		ws.ActivePaneIdx = len(ws.Panes) - 1
	}
	return ws.Panes[ws.ActivePaneIdx]
}

func (s *Server) applyLayoutLocked() {
	viewportWidth := s.viewportWidth
	if viewportWidth <= 0 {
		viewportWidth = 80
	}
	viewportHeight := s.viewportHgt
	if viewportHeight <= 0 {
		viewportHeight = 24
	}
	for _, ws := range s.state.Workspaces {
		if ws == nil {
			continue
		}
		virtualX := 0
		for _, pane := range ws.Panes {
			paneWidth := DefaultPaneWidth + pane.WidthDelta
			if len(ws.Panes) == 1 {
				paneWidth = viewportWidth
			}
			if paneWidth < 20 {
				paneWidth = 20
			}
			pane.VirtualX = virtualX
			pane.Width = paneWidth
			newCols := max(10, paneWidth-2)
			newRows := max(5, viewportHeight-2)
			if newCols != pane.Cols || newRows != pane.Rows {
				pane.Cols = newCols
				pane.Rows = newRows
				pane.VT.Resize(newRows, newCols)
				_ = pty.Setsize(pane.PTY, &pty.Winsize{Rows: uint16(newRows), Cols: uint16(newCols)})
			}
			virtualX += paneWidth
		}
	}
}

func (s *Server) spawnPaneLocked(ws *Workspace, widthDelta int) *Pane {
	if ws == nil {
		return nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	initCols := max(10, DefaultPaneWidth-2)
	initRows := max(5, s.viewportHgt-2)
	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(initRows), Cols: uint16(initCols)})
	if err != nil {
		return nil
	}
	vt := NewVTerm(initRows, initCols)
	vt.responseWriter = ptmx
	pane := &Pane{
		ID:         fmt.Sprintf("pane-%d", time.Now().UnixNano()),
		PTY:        ptmx,
		Cmd:        cmd,
		VT:         vt,
		ScrollOff:  0,
		Width:      DefaultPaneWidth,
		WidthDelta: widthDelta,
		Rows:       initRows,
		Cols:       initCols,
	}
	ws.Panes = append(ws.Panes, pane)
	ws.ActivePaneIdx = len(ws.Panes) - 1
	go s.readPaneOutput(pane)
	return pane
}

func (s *Server) readPaneOutput(p *Pane) {
	buf := make([]byte, 4096)
	for {
		n, err := p.PTY.Read(buf)
		if err != nil {
			s.closePaneAndMaybeStop(p)
			return
		}
		if n > 0 {
			p.VT.Write(buf[:n])
			s.requestBroadcast()
		}
	}
}

func (s *Server) closePaneAndMaybeStop(p *Pane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	_ = p.PTY.Close()

	ownerIdx := -1
	paneIdx := -1
	for wi, w := range s.state.Workspaces {
		for pi, wp := range w.Panes {
			if wp == p {
				ownerIdx = wi
				paneIdx = pi
				break
			}
		}
		if ownerIdx >= 0 {
			break
		}
	}
	if ownerIdx < 0 || paneIdx < 0 {
		return
	}
	ws := s.state.Workspaces[ownerIdx]
	ws.Panes = append(ws.Panes[:paneIdx], ws.Panes[paneIdx+1:]...)
	if len(ws.Panes) > 0 {
		if ws.ActivePaneIdx >= len(ws.Panes) {
			ws.ActivePaneIdx = len(ws.Panes) - 1
		}
		s.applyLayoutLocked()
		s.requestBroadcast()
		return
	}
	if len(s.state.Workspaces) > 1 {
		s.state.Workspaces = append(s.state.Workspaces[:ownerIdx], s.state.Workspaces[ownerIdx+1:]...)
		if s.state.ActiveWSIdx >= len(s.state.Workspaces) {
			s.state.ActiveWSIdx = len(s.state.Workspaces) - 1
		}
		s.applyLayoutLocked()
		s.requestBroadcast()
		return
	}

	go s.stopWithQuit()
}

func (s *Server) stopWithQuit() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	clients := make([]*serverClient, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	workspaces := s.state.Workspaces
	s.mu.Unlock()

	quitMsg := ServerMsg{Type: "quit"}
	for _, c := range clients {
		c.mu.Lock()
		_ = sendMsg(c.conn, quitMsg)
		_ = c.conn.Close()
		c.mu.Unlock()
	}
	for _, w := range workspaces {
		for _, p := range w.Panes {
			if p != nil {
				_ = p.PTY.Close()
			}
		}
	}
	_ = s.listener.Close()
}

func (s *Server) findColorByName(name string) (workspaceColorOption, bool) {
	for _, c := range workspaceColors {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return workspaceColorOption{}, false
}

func (s *Server) handleClientMsg(clientID int, msg ClientMsg) (detach bool, stop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false, true
	}

	switch msg.Type {
	case "resize":
		if msg.ViewportWidth > 0 {
			s.viewportWidth = msg.ViewportWidth
		}
		if msg.ViewportHeight > 0 {
			s.viewportHgt = msg.ViewportHeight
		}
		s.applyLayoutLocked()
		s.requestBroadcast()
		return false, false
	case "input":
		pane := s.activePaneLocked()
		if pane != nil && len(msg.InputData) > 0 {
			_, _ = pane.PTY.Write(msg.InputData)
		}
		return false, false
	case "command":
		ws := s.activeWorkspaceLocked()
		if ws == nil {
			return false, false
		}
		switch msg.Command {
		case "new-pane":
			preset := paneWidthPresets[s.panePresetIdx]
			targetWidth := paneWidthForPreset(s.viewportWidth, preset)
			widthDelta := targetWidth - DefaultPaneWidth
			s.spawnPaneLocked(ws, widthDelta)
			s.applyLayoutLocked()
		case "close-pane":
			if len(ws.Panes) > 0 {
				go s.closePaneAndMaybeStop(ws.Panes[ws.ActivePaneIdx])
				return false, false
			}
		case "new-workspace":
			newColor := autoWorkspaceColor(s.state.Workspaces)
			newWs := &Workspace{
				Name:          fmt.Sprintf("Workspace %d", len(s.state.Workspaces)+1),
				Color:         newColor.Color,
				ColorName:     newColor.Name,
				Panes:         make([]*Pane, 0),
				ActivePaneIdx: 0,
			}
			s.state.Workspaces = append(s.state.Workspaces, newWs)
			s.state.ActiveWSIdx = len(s.state.Workspaces) - 1
			s.spawnPaneLocked(newWs, 0)
			s.applyLayoutLocked()
		case "close-workspace":
			if len(s.state.Workspaces) > 1 {
				idx := s.state.ActiveWSIdx
				for _, p := range ws.Panes {
					if p != nil {
						_ = p.PTY.Close()
					}
				}
				s.state.Workspaces = append(s.state.Workspaces[:idx], s.state.Workspaces[idx+1:]...)
				if s.state.ActiveWSIdx >= len(s.state.Workspaces) {
					s.state.ActiveWSIdx = len(s.state.Workspaces) - 1
				}
				s.applyLayoutLocked()
			}
		case "rename-workspace":
			name := strings.TrimSpace(msg.WorkspaceName)
			if name != "" {
				ws.Name = name
			}
		case "set-workspace-color":
			if color, ok := s.findColorByName(msg.ColorName); ok {
				ws.Color = color.Color
				ws.ColorName = color.Name
			}
		case "set-pane-preset":
			if msg.PanePresetIdx >= 0 && msg.PanePresetIdx < len(paneWidthPresets) {
				s.panePresetIdx = msg.PanePresetIdx
				go updateSettings(func(cfg *AppSettings) {
					cfg.PanePresetIdx = s.panePresetIdx
				})
			}
		case "next-pane":
			if len(ws.Panes) > 1 {
				ws.ActivePaneIdx = (ws.ActivePaneIdx + 1) % len(ws.Panes)
			}
		case "prev-pane":
			if len(ws.Panes) > 1 {
				ws.ActivePaneIdx = (ws.ActivePaneIdx - 1 + len(ws.Panes)) % len(ws.Panes)
			}
		case "next-workspace":
			if len(s.state.Workspaces) > 1 {
				s.state.ActiveWSIdx = (s.state.ActiveWSIdx + 1) % len(s.state.Workspaces)
			}
		case "prev-workspace":
			if len(s.state.Workspaces) > 1 {
				s.state.ActiveWSIdx = (s.state.ActiveWSIdx - 1 + len(s.state.Workspaces)) % len(s.state.Workspaces)
			}
		case "widen-pane":
			if p := s.activePaneLocked(); p != nil {
				p.WidthDelta += 5
				s.applyLayoutLocked()
			}
		case "narrow-pane":
			if p := s.activePaneLocked(); p != nil {
				p.WidthDelta -= 5
				s.applyLayoutLocked()
			}
		case "scroll-up":
			if p := s.activePaneLocked(); p != nil {
				p.ScrollOff += 3
			}
		case "scroll-down":
			if p := s.activePaneLocked(); p != nil {
				p.ScrollOff = max(0, p.ScrollOff-3)
			}
		case "page-up":
			if p := s.activePaneLocked(); p != nil {
				p.ScrollOff += 5
			}
		case "page-down":
			if p := s.activePaneLocked(); p != nil {
				p.ScrollOff = max(0, p.ScrollOff-5)
			}
		case "focus-pane":
			if msg.PaneIndex >= 0 && msg.PaneIndex < len(ws.Panes) {
				ws.ActivePaneIdx = msg.PaneIndex
			}
		case "focus-workspace":
			if msg.WorkspaceIndex >= 0 && msg.WorkspaceIndex < len(s.state.Workspaces) {
				s.state.ActiveWSIdx = msg.WorkspaceIndex
			}
		case "quit":
			go s.stopWithQuit()
			return false, true
		case "detach":
			return true, false
		}
		s.requestBroadcast()
		return false, false
	}
	return false, false
}
