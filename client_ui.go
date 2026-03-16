package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func runClient(session string) error {
	path := socketPathForSession(session)
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	ui := newClientUI(conn)
	return ui.run()
}

func newClientUI(conn net.Conn) *ClientUI {
	ui := &ClientUI{conn: conn, app: tview.NewApplication()}
	settings := loadSettings()
	ui.sidebarHidden = settings.SidebarHidden
	ui.sidebar = tview.NewList().SetSecondaryTextColor(tcell.ColorGray).ShowSecondaryText(true)
	ui.sidebar.SetUseStyleTags(true, false)
	ui.sidebar.SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	ui.sidebar.SetSelectedTextColor(tcell.ColorWhite)
	ui.sidebar.SetBackgroundColor(tcell.ColorBlack)
	ui.sidebar.SetBorderColor(tcell.ColorDarkGray)
	ui.sidebar.SetBorder(true)

	ui.statusBar = tview.NewTextView().
		SetText("").
		SetDynamicColors(true)
	ui.statusBar.SetBackgroundColor(tcell.ColorBlack)
	ui.statusBar.SetTextColor(tcell.ColorSilver)
	ui.statusBar.SetBorder(true)
	ui.statusBar.SetBorderColor(tcell.ColorDarkGray)
	ui.refreshStatusBar()

	ui.viewport = tview.NewBox().SetBackgroundColor(tcell.ColorBlack)

	rightColumn := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(ui.viewport, 0, 1, true).
		AddItem(ui.statusBar, 3, 0, false)

	mainFlex := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(ui.sidebar, SidebarWidth, 0, false).
		AddItem(rightColumn, 0, 1, true)
	ui.rootFlex = mainFlex
	if ui.sidebarHidden {
		ui.rootFlex.ResizeItem(ui.sidebar, 0, 0)
	}

	ui.cmdPalette = tview.NewList().ShowSecondaryText(true).SetSecondaryTextColor(tcell.ColorGray)
	ui.cmdPalette.SetUseStyleTags(true, false)
	ui.cmdPalette.SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	ui.cmdPalette.SetSelectedTextColor(tcell.ColorWhite)
	ui.cmdPalette.SetBackgroundColor(tcell.ColorBlack)
	ui.cmdPalette.SetBorder(true)
	ui.cmdPalette.SetBorderColor(tcell.ColorBlue)
	ui.cmdPalette.SetTitle(" Command Palette ")
	ui.cmdPalette.SetTitleAlign(tview.AlignLeft)

	ui.renameInput = tview.NewInputField().SetLabel(" Workspace name: ").SetFieldWidth(32)
	ui.renameInput.SetBorder(true)
	ui.renameInput.SetTitle(" Rename Workspace ")
	ui.renameInput.SetTitleAlign(tview.AlignLeft)
	ui.renameInput.SetBorderColor(tcell.ColorBlue)

	ui.quitModal = tview.NewModal().
		SetText("Quit all sessions and stop all running panes?").
		AddButtons([]string{"Cancel", "Quit All"})
	ui.quitModal.SetBackgroundColor(tcell.ColorBlack)

	paletteFlex := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(ui.cmdPalette, 22, 0, true).
			AddItem(nil, 0, 1, false), 72, 0, true).
		AddItem(nil, 0, 1, false)

	renameFlex := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(ui.renameInput, 3, 0, true).
			AddItem(nil, 0, 1, false), 56, 0, true).
		AddItem(nil, 0, 1, false)

	ui.pages = tview.NewPages().
		AddPage("main", mainFlex, true, true).
		AddPage("palette", paletteFlex, true, false).
		AddPage("rename", renameFlex, true, false).
		AddPage("quit-confirm", ui.quitModal, true, false)

	ui.installHandlers()
	return ui
}

func (ui *ClientUI) run() error {
	defer ui.conn.Close()
	go ui.recvLoop()
	return ui.app.SetRoot(ui.pages, true).Run()
}

func (ui *ClientUI) recvLoop() {
	for {
		var msg ServerMsg
		if err := recvMsg(ui.conn, &msg); err != nil {
			ui.app.QueueUpdateDraw(func() { ui.app.Stop() })
			return
		}
		if msg.Type == "quit" {
			ui.app.QueueUpdateDraw(func() { ui.app.Stop() })
			return
		}
		if msg.Type == "snapshot" {
			m := msg
			ui.app.QueueUpdateDraw(func() {
				ui.snapshot = m
				ui.haveSnap = true
				ui.updateSidebar()
				ui.refreshStatusBar()
				ui.renderPanes()
			})
		}
	}
}

func (ui *ClientUI) refreshStatusBar() {
	shortcuts := " Alt+P: Palette | Alt+1..9: Panes | Alt+F1..F9: Workspaces | Alt+Left/Right: Panes | Alt+Up/Down: Workspaces | Alt++/-: Width "
	text := shortcuts
	if ui.sidebarHidden {
		wsName := "Workspace"
		wsColor := tcell.ColorSilver
		if ui.haveSnap && ui.snapshot.ActiveWSIdx >= 0 && ui.snapshot.ActiveWSIdx < len(ui.snapshot.Workspaces) {
			ws := ui.snapshot.Workspaces[ui.snapshot.ActiveWSIdx]
			name := strings.TrimSpace(ws.Name)
			if name != "" {
				wsName = name
			}
			wsColor = tcell.Color(ws.ColorValue)
		}
		text = fmt.Sprintf(" Current: %s%s[-] |%s", colorTag(wsColor), wsName, shortcuts)
	}
	ui.statusBar.SetText(text)
}

func (ui *ClientUI) send(msg ClientMsg) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	_ = sendMsg(ui.conn, msg)
}

func (ui *ClientUI) updateSidebar() {
	if !ui.haveSnap {
		return
	}
	ui.sidebar.Clear()
	for i, w := range ui.snapshot.Workspaces {
		marker := " "
		if i == ui.snapshot.ActiveWSIdx {
			marker = ">"
		}
		name := strings.TrimSpace(w.Name)
		if name == "" {
			name = "Workspace"
		}
		mainText := fmt.Sprintf("%s %s%s[-]", marker, colorTag(tcell.Color(w.ColorValue)), name)
		ui.sidebar.AddItem(mainText, fmt.Sprintf("%d panes | %s", w.PaneCount, w.ColorName), 0, nil)
	}
	ui.sidebar.AddItem("+ New", "", 0, nil)
}

func (ui *ClientUI) activeWorkspaceColor() tcell.Color {
	if !ui.haveSnap || ui.snapshot.ActiveWSIdx < 0 || ui.snapshot.ActiveWSIdx >= len(ui.snapshot.Workspaces) {
		return tcell.ColorBlue
	}
	return tcell.Color(ui.snapshot.Workspaces[ui.snapshot.ActiveWSIdx].ColorValue)
}

func (ui *ClientUI) renderPanes() {
	if !ui.haveSnap {
		return
	}
	snaps := ui.snapshot.Panes
	wsColor := ui.activeWorkspaceColor()

	ui.viewport.SetDrawFunc(func(screen tcell.Screen, x, y, w, h int) (int, int, int, int) {
		clearStyle := tcell.StyleDefault.Background(tcell.ColorBlack)
		for ry := 0; ry < h; ry++ {
			for rx := 0; rx < w; rx++ {
				screen.SetCell(x+rx, y+ry, clearStyle, ' ')
			}
		}

		if len(snaps) > 0 {
			active := -1
			for i, p := range snaps {
				if p.IsActive {
					active = i
					break
				}
			}
			if active >= 0 {
				left := snaps[active].VirtualX
				right := left + snaps[active].Width
				if right > ui.viewportScrollX+w {
					ui.viewportScrollX = right - w
				}
				if left < ui.viewportScrollX {
					ui.viewportScrollX = left
				}
			}
			total := snaps[len(snaps)-1].VirtualX + snaps[len(snaps)-1].Width
			maxScroll := total - w
			if maxScroll < 0 {
				maxScroll = 0
			}
			if ui.viewportScrollX > maxScroll {
				ui.viewportScrollX = maxScroll
			}
			if ui.viewportScrollX < 0 {
				ui.viewportScrollX = 0
			}
		}

		for _, snap := range snaps {
			paneScreenX := x + snap.VirtualX - ui.viewportScrollX
			paneW := snap.Width
			paneH := h
			if paneScreenX+paneW <= x || paneScreenX >= x+w {
				continue
			}
			borderColor := tcell.ColorDarkGray
			if snap.IsActive {
				borderColor = wsColor
			}
			borderStyle := tcell.StyleDefault.Foreground(borderColor).Background(tcell.ColorBlack)
			drawClippedBorder(screen, paneScreenX, y, paneW, paneH, x, y, w, h, borderStyle, snap.Title)

			innerX := paneScreenX + 1
			innerY := y + 1
			innerW := paneW - 2
			innerH := paneH - 2
			if innerW <= 0 || innerH <= 0 {
				continue
			}
			for row := 0; row < innerH && row < len(snap.Cells); row++ {
				line := snap.Cells[row]
				for col := 0; col < innerW && col < len(line); col++ {
					sx := innerX + col
					sy := innerY + row
					if sx >= x && sx < x+w && sy >= y && sy < y+h {
						cell := line[col]
						r := cell.Ch
						if r == 0 {
							r = ' '
						}
						screen.SetCell(sx, sy, wireCellStyle(cell), r)
					}
				}
			}

			if snap.IsActive && snap.CursorVisible && snap.ScrollOff == 0 {
				cx := innerX + snap.CursorCol
				cy := innerY + snap.CursorRow
				if cx >= x && cx < x+w && cy >= y && cy < y+h && snap.CursorCol < innerW && snap.CursorRow < innerH {
					ch := ' '
					style := tcell.StyleDefault.Background(tcell.ColorSilver).Foreground(tcell.ColorBlack)
					if snap.CursorRow < len(snap.Cells) && snap.CursorCol < len(snap.Cells[snap.CursorRow]) {
						cell := snap.Cells[snap.CursorRow][snap.CursorCol]
						ch = cell.Ch
						if ch == 0 {
							ch = ' '
						}
						fg := tcell.Color(cell.Fg)
						bg := tcell.Color(cell.Bg)
						if fg == tcell.ColorDefault {
							fg = tcell.ColorSilver
						}
						if bg == tcell.ColorDefault {
							bg = tcell.ColorBlack
						}
						style = tcell.StyleDefault.Foreground(bg).Background(fg).Attributes(tcell.AttrMask(cell.Attrs))
					}
					screen.SetCell(cx, cy, style, ch)
				}
			}
		}
		return x, y, w, h
	})
}

func (ui *ClientUI) showPalette() {
	ui.cmdPalette.Clear()
	ui.paletteColorMode = false
	ui.palettePaneMode = false
	ui.cmdPalette.AddItem("[t] New Pane", "Create using server width behavior", 't', nil)
	ui.cmdPalette.AddItem("[w] Close Pane", "Close active pane", 'w', nil)
	ui.cmdPalette.AddItem("[n] New Workspace", "Create workspace", 'n', nil)
	ui.cmdPalette.AddItem("[x] Close Workspace", "Close active workspace", 'x', nil)
	ui.cmdPalette.AddItem("[r] Rename Workspace", "Set active workspace name", 'r', nil)
	ui.cmdPalette.AddItem("[c] Set Workspace Color", "Pick from list (1-6 fast)", 'c', nil)
	ui.cmdPalette.AddItem("[b] Pane Width Behavior", ui.snapshot.PanePresetName, 'b', nil)
	ui.cmdPalette.AddItem("[s] Toggle Sidebar", "Show or hide workspace sidebar", 's', nil)
	ui.cmdPalette.AddItem("[d] Detach", "Detach client, keep server running", 'd', nil)
	ui.cmdPalette.AddItem("[h] Previous Pane", "Focus previous pane", 'h', nil)
	ui.cmdPalette.AddItem("[l] Next Pane", "Focus next pane", 'l', nil)
	ui.cmdPalette.AddItem("[j] Next Workspace", "Switch workspace", 'j', nil)
	ui.cmdPalette.AddItem("[k] Previous Workspace", "Switch workspace", 'k', nil)
	ui.cmdPalette.AddItem("[q] Quit All", "Prompt to stop server and all sessions", 'q', nil)
	ui.cmdPalette.SetCurrentItem(0)
	ui.paletteActive = true
	ui.pages.ShowPage("palette")
	ui.app.SetFocus(ui.cmdPalette)
}

func (ui *ClientUI) showPanePresetPalette() {
	ui.cmdPalette.Clear()
	ui.palettePaneMode = true
	for i, p := range paneWidthPresets {
		shortcut := rune(0)
		if i < 9 {
			shortcut = rune('1' + i)
		}
		desc := "Use as server default for new panes"
		if i == ui.snapshot.PanePresetIdx {
			desc = "Current default"
		}
		ui.cmdPalette.AddItem(p.Name, desc, shortcut, nil)
	}
	ui.cmdPalette.SetCurrentItem(ui.snapshot.PanePresetIdx)
	ui.paletteActive = true
	ui.pages.ShowPage("palette")
	ui.app.SetFocus(ui.cmdPalette)
}

func (ui *ClientUI) showColorPalette() {
	ui.cmdPalette.Clear()
	ui.paletteColorMode = true
	for i, c := range workspaceColors {
		shortcut := rune(0)
		label := fmt.Sprintf("%s%s[-]", colorTag(c.Color), c.Name)
		if i < fastWorkspaceColorCount {
			shortcut = rune('1' + i)
			label = fmt.Sprintf("[%d] %s", i+1, label)
		}
		ui.cmdPalette.AddItem(label, "Apply to active workspace", shortcut, nil)
	}
	idx := 0
	if ui.haveSnap && ui.snapshot.ActiveWSIdx >= 0 && ui.snapshot.ActiveWSIdx < len(ui.snapshot.Workspaces) {
		idx = workspaceColorIndex(ui.snapshot.Workspaces[ui.snapshot.ActiveWSIdx].ColorName)
	}
	ui.cmdPalette.SetCurrentItem(idx)
	ui.paletteActive = true
	ui.pages.ShowPage("palette")
	ui.app.SetFocus(ui.cmdPalette)
}

func (ui *ClientUI) hidePalette() {
	ui.paletteActive = false
	ui.paletteColorMode = false
	ui.palettePaneMode = false
	ui.pages.HidePage("palette")
	ui.app.SetFocus(ui.pages)
}

func (ui *ClientUI) showRename() {
	ui.hidePalette()
	if !ui.haveSnap || ui.snapshot.ActiveWSIdx < 0 || ui.snapshot.ActiveWSIdx >= len(ui.snapshot.Workspaces) {
		return
	}
	ui.renameInput.SetText(ui.snapshot.Workspaces[ui.snapshot.ActiveWSIdx].Name)
	ui.renameActive = true
	ui.pages.ShowPage("rename")
	ui.app.SetFocus(ui.renameInput)
}

func (ui *ClientUI) hideRename() {
	ui.renameActive = false
	ui.pages.HidePage("rename")
	ui.app.SetFocus(ui.pages)
}

func (ui *ClientUI) showQuitConfirm() {
	ui.hidePalette()
	ui.hideRename()
	ui.quitConfirmActive = true
	ui.pages.ShowPage("quit-confirm")
	ui.app.SetFocus(ui.quitModal)
}

func (ui *ClientUI) hideQuitConfirm() {
	ui.quitConfirmActive = false
	ui.pages.HidePage("quit-confirm")
	ui.app.SetFocus(ui.pages)
}

func (ui *ClientUI) toggleSidebar() {
	if ui.rootFlex == nil {
		return
	}
	if ui.sidebarHidden {
		ui.rootFlex.ResizeItem(ui.sidebar, SidebarWidth, 0)
		ui.sidebarHidden = false
	} else {
		ui.rootFlex.ResizeItem(ui.sidebar, 0, 0)
		ui.sidebarHidden = true
	}
	go updateSettings(func(cfg *AppSettings) {
		cfg.SidebarHidden = ui.sidebarHidden
	})
	ui.refreshStatusBar()
}

func (ui *ClientUI) executePaletteCmd(shortcut rune) {
	switch shortcut {
	case 'b':
		ui.showPanePresetPalette()
		return
	case 'c':
		ui.showColorPalette()
		return
	case 's':
		ui.hidePalette()
		ui.toggleSidebar()
		return
	}
	ui.hidePalette()
	switch shortcut {
	case 'q':
		ui.showQuitConfirm()
	case 't':
		ui.send(ClientMsg{Type: "command", Command: "new-pane"})
	case 'w':
		ui.send(ClientMsg{Type: "command", Command: "close-pane"})
	case 'n':
		ui.send(ClientMsg{Type: "command", Command: "new-workspace"})
	case 'x':
		ui.send(ClientMsg{Type: "command", Command: "close-workspace"})
	case 'r':
		ui.showRename()
	case 'd':
		ui.send(ClientMsg{Type: "command", Command: "detach"})
		ui.app.Stop()
	case 'h':
		ui.send(ClientMsg{Type: "command", Command: "prev-pane"})
	case 'l':
		ui.send(ClientMsg{Type: "command", Command: "next-pane"})
	case 'j':
		ui.send(ClientMsg{Type: "command", Command: "next-workspace"})
	case 'k':
		ui.send(ClientMsg{Type: "command", Command: "prev-workspace"})
	}
}

func (ui *ClientUI) activePaneSnapshot() *PaneSnapshot {
	if !ui.haveSnap {
		return nil
	}
	for i := range ui.snapshot.Panes {
		if ui.snapshot.Panes[i].IsActive {
			return &ui.snapshot.Panes[i]
		}
	}
	if len(ui.snapshot.Panes) > 0 {
		return &ui.snapshot.Panes[0]
	}
	return nil
}

func (ui *ClientUI) installHandlers() {
	ui.quitModal.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		if buttonLabel == "Quit All" {
			ui.send(ClientMsg{Type: "command", Command: "quit"})
		}
		ui.hideQuitConfirm()
	})

	ui.renameInput.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEsc:
			ui.hideRename()
		case tcell.KeyEnter:
			name := strings.TrimSpace(ui.renameInput.GetText())
			if name != "" {
				ui.send(ClientMsg{Type: "command", Command: "rename-workspace", WorkspaceName: name})
			}
			ui.hideRename()
		}
	})

	ui.cmdPalette.SetSelectedFunc(func(index int, mainText string, secondaryText string, shortcut rune) {
		if ui.paletteColorMode {
			if index >= 0 && index < len(workspaceColors) {
				ui.send(ClientMsg{Type: "command", Command: "set-workspace-color", ColorName: workspaceColors[index].Name})
			}
			ui.hidePalette()
			return
		}
		if ui.palettePaneMode {
			if index >= 0 && index < len(paneWidthPresets) {
				ui.send(ClientMsg{Type: "command", Command: "set-pane-preset", PanePresetIdx: index})
			}
			ui.showPalette()
			return
		}
		ui.executePaletteCmd(shortcut)
	})

	ui.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		key := event.Key()
		mods := event.Modifiers()

		if ui.quitConfirmActive {
			if key == tcell.KeyEsc {
				ui.hideQuitConfirm()
				return nil
			}
			return event
		}

		if ui.renameActive {
			if key == tcell.KeyEsc {
				ui.hideRename()
				return nil
			}
			return event
		}

		if key == tcell.KeyRune && (event.Rune() == 'p' || event.Rune() == 'P') && mods&tcell.ModAlt != 0 {
			if ui.paletteActive {
				ui.hidePalette()
			} else {
				ui.showPalette()
			}
			return nil
		}

		if ui.paletteActive {
			if key == tcell.KeyEsc {
				if ui.paletteColorMode || ui.palettePaneMode {
					ui.showPalette()
				} else {
					ui.hidePalette()
				}
				return nil
			}
			return event
		}

		if mods&tcell.ModAlt != 0 {
			switch key {
			case tcell.KeyF1:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 0})
				return nil
			case tcell.KeyF2:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 1})
				return nil
			case tcell.KeyF3:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 2})
				return nil
			case tcell.KeyF4:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 3})
				return nil
			case tcell.KeyF5:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 4})
				return nil
			case tcell.KeyF6:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 5})
				return nil
			case tcell.KeyF7:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 6})
				return nil
			case tcell.KeyF8:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 7})
				return nil
			case tcell.KeyF9:
				ui.send(ClientMsg{Type: "command", Command: "focus-workspace", WorkspaceIndex: 8})
				return nil
			}
		}

		if key == tcell.KeyRune && mods&tcell.ModAlt != 0 {
			r := event.Rune()
			if r >= '1' && r <= '9' {
				ui.send(ClientMsg{Type: "command", Command: "focus-pane", PaneIndex: int(r - '1')})
				return nil
			}
		}

		if key == tcell.KeyRight && mods&tcell.ModAlt != 0 {
			ui.send(ClientMsg{Type: "command", Command: "next-pane"})
			return nil
		}
		if key == tcell.KeyLeft && mods&tcell.ModAlt != 0 {
			ui.send(ClientMsg{Type: "command", Command: "prev-pane"})
			return nil
		}
		if key == tcell.KeyUp && mods&tcell.ModAlt != 0 {
			ui.send(ClientMsg{Type: "command", Command: "next-workspace"})
			return nil
		}
		if key == tcell.KeyDown && mods&tcell.ModAlt != 0 {
			ui.send(ClientMsg{Type: "command", Command: "prev-workspace"})
			return nil
		}
		if key == tcell.KeyRune && mods&tcell.ModAlt != 0 {
			r := event.Rune()
			if r == '+' || r == '=' {
				ui.send(ClientMsg{Type: "command", Command: "widen-pane"})
				return nil
			}
			if r == '-' {
				ui.send(ClientMsg{Type: "command", Command: "narrow-pane"})
				return nil
			}
		}

		if key == tcell.KeyPgUp {
			ui.send(ClientMsg{Type: "command", Command: "page-up"})
			return nil
		}
		if key == tcell.KeyPgDn {
			ui.send(ClientMsg{Type: "command", Command: "page-down"})
			return nil
		}

		pane := ui.activePaneSnapshot()
		appCursor := false
		if pane != nil {
			appCursor = pane.AppCursorKeys
		}
		if data := keyToInput(event, appCursor); len(data) > 0 {
			ui.send(ClientMsg{Type: "input", InputData: data})
			return nil
		}
		return event
	})

	ui.app.SetAfterDrawFunc(func(screen tcell.Screen) {
		w, h := screen.Size()
		if w != ui.lastScreenW || h != ui.lastScreenH {
			ui.lastScreenW = w
			ui.lastScreenH = h
			viewportW := w - SidebarWidth - 2
			if viewportW < 20 {
				viewportW = 80
			}
			viewportH := h - 6
			if viewportH < 10 {
				viewportH = 24
			}
			ui.send(ClientMsg{Type: "resize", ViewportWidth: viewportW, ViewportHeight: viewportH})
		}
		sidebarBorderStyle := tcell.StyleDefault.Foreground(tcell.ColorDarkGray).Background(tcell.ColorBlack)
		roundCorners(screen, ui.sidebar.Box, sidebarBorderStyle)
		statusBorderStyle := tcell.StyleDefault.Foreground(tcell.ColorDarkGray).Background(tcell.ColorBlack)
		roundCorners(screen, ui.statusBar.Box, statusBorderStyle)
	})

	ui.app.EnableMouse(true)
	ui.app.SetMouseCapture(func(event *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
		mx, my := event.Position()
		if ui.paletteActive {
			return event, action
		}

		if action == tview.MouseScrollUp {
			ui.send(ClientMsg{Type: "command", Command: "scroll-up"})
			return nil, tview.MouseConsumed
		}
		if action == tview.MouseScrollDown {
			ui.send(ClientMsg{Type: "command", Command: "scroll-down"})
			return nil, tview.MouseConsumed
		}

		if action == tview.MouseLeftClick {
			sx, sy, sw, sh := ui.sidebar.GetRect()
			if mx >= sx && mx < sx+sw && my >= sy && my < sy+sh {
				innerY := my - sy - 1
				if innerY >= 0 {
					itemIdx := innerY / 2
					num := len(ui.snapshot.Workspaces)
					if itemIdx == num {
						ui.send(ClientMsg{Type: "command", Command: "new-workspace"})
					} else if itemIdx >= 0 && itemIdx < num && ui.haveSnap {
						for ui.snapshot.ActiveWSIdx < itemIdx {
							ui.send(ClientMsg{Type: "command", Command: "next-workspace"})
							ui.snapshot.ActiveWSIdx++
						}
						for ui.snapshot.ActiveWSIdx > itemIdx {
							ui.send(ClientMsg{Type: "command", Command: "prev-workspace"})
							ui.snapshot.ActiveWSIdx--
						}
					}
				}
				return nil, tview.MouseConsumed
			}

			vx, vy, _, vh := ui.viewport.GetRect()
			for i, pane := range ui.snapshot.Panes {
				px := vx + pane.VirtualX - ui.viewportScrollX
				if mx >= px && mx < px+pane.Width && my >= vy && my < vy+vh {
					if !pane.IsActive {
						ui.send(ClientMsg{Type: "command", Command: "focus-pane", PaneIndex: i})
					}
					return nil, tview.MouseConsumed
				}
			}
		}

		return event, action
	})
}
