package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type Pane struct {
	ID         string
	PTY        *os.File
	Cmd        *exec.Cmd
	VT         *VTerm
	ScrollOff  int
	Width      int
	WidthDelta int
	VirtualX   int
	Rows       int
	Cols       int
}

type Workspace struct {
	Name          string
	Color         tcell.Color
	ColorName     string
	Panes         []*Pane
	ActivePaneIdx int
}

type AppState struct {
	Workspaces  []*Workspace
	ActiveWSIdx int
}

type serverClient struct {
	id     int
	conn   net.Conn
	sendCh chan ServerMsg
	mu     sync.Mutex
}

type Server struct {
	session string
	path    string

	listener net.Listener

	mu            sync.Mutex
	state         *AppState
	panePresetIdx int
	viewportWidth int
	viewportHgt   int
	clients       map[int]*serverClient
	nextClientID  int
	stopping      bool

	username string
	hostname string
	homeDir  string

	broadcastCh chan struct{}
	stoppedCh   chan struct{}
}

type ClientUI struct {
	conn net.Conn
	mu   sync.Mutex

	app         *tview.Application
	rootFlex    *tview.Flex
	viewport    *tview.Box
	sidebar     *tview.List
	statusBar   *tview.TextView
	pages       *tview.Pages
	cmdPalette  *tview.List
	renameInput *tview.InputField
	quitModal   *tview.Modal

	paletteActive     bool
	paletteColorMode  bool
	palettePaneMode   bool
	renameActive      bool
	quitConfirmActive bool
	sidebarHidden     bool

	viewportScrollX int
	lastScreenW     int
	lastScreenH     int

	snapshot ServerMsg
	haveSnap bool
}

func getPaneCwd(p *Pane) string {
	if p.Cmd == nil || p.Cmd.Process == nil {
		return ""
	}
	link := fmt.Sprintf("/proc/%d/cwd", p.Cmd.Process.Pid)
	cwd, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return cwd
}

func shortenPath(path, homeDir string) string {
	if homeDir != "" && strings.HasPrefix(path, homeDir) {
		return "~" + path[len(homeDir):]
	}
	return path
}

func getPaneTitle(p *Pane, idx int, _ string, _ string, homeDir string) string {
	cwd := getPaneCwd(p)
	if cwd == "" {
		cwd = "?"
	} else {
		cwd = shortenPath(cwd, homeDir)
		cwd = filepath.Clean(cwd)
	}
	return fmt.Sprintf(" (%d) %s ", idx+1, cwd)
}

type workspaceColorOption struct {
	Name  string
	Color tcell.Color
}

var workspaceColors = []workspaceColorOption{
	{Name: "Red", Color: tcell.NewRGBColor(239, 68, 68)},
	{Name: "Green", Color: tcell.NewRGBColor(34, 197, 94)},
	{Name: "Blue", Color: tcell.NewRGBColor(52, 120, 246)},
	{Name: "Yellow", Color: tcell.NewRGBColor(250, 204, 21)},
	{Name: "Cyan", Color: tcell.NewRGBColor(34, 211, 238)},
	{Name: "Magenta", Color: tcell.NewRGBColor(217, 70, 239)},
	{Name: "Sky", Color: tcell.NewRGBColor(56, 189, 248)},
	{Name: "Teal", Color: tcell.NewRGBColor(45, 212, 191)},
	{Name: "Lime", Color: tcell.NewRGBColor(132, 204, 22)},
	{Name: "Amber", Color: tcell.NewRGBColor(245, 158, 11)},
	{Name: "Orange", Color: tcell.NewRGBColor(249, 115, 22)},
	{Name: "Coral", Color: tcell.NewRGBColor(251, 113, 133)},
	{Name: "Rose", Color: tcell.NewRGBColor(244, 63, 94)},
	{Name: "Purple", Color: tcell.NewRGBColor(168, 85, 247)},
	{Name: "Violet", Color: tcell.NewRGBColor(139, 92, 246)},
	{Name: "Indigo", Color: tcell.NewRGBColor(99, 102, 241)},
	{Name: "Brown", Color: tcell.NewRGBColor(146, 101, 64)},
	{Name: "Slate", Color: tcell.NewRGBColor(100, 116, 139)},
	{Name: "Gray", Color: tcell.NewRGBColor(156, 163, 175)},
	{Name: "White", Color: tcell.NewRGBColor(241, 245, 249)},
}

const fastWorkspaceColorCount = 6

type paneWidthPreset struct {
	Name        string
	UseViewport bool
	Num         int
	Den         int
}

var paneWidthPresets = []paneWidthPreset{
	{Name: "Natural (Default)", UseViewport: false},
	{Name: "Full Width (1/1)", UseViewport: true, Num: 1, Den: 1},
	{Name: "Wide (4/5)", UseViewport: true, Num: 4, Den: 5},
	{Name: "Three Quarters (3/4)", UseViewport: true, Num: 3, Den: 4},
	{Name: "Two Thirds (2/3)", UseViewport: true, Num: 2, Den: 3},
	{Name: "Half (1/2)", UseViewport: true, Num: 1, Den: 2},
	{Name: "Two Fifths (2/5)", UseViewport: true, Num: 2, Den: 5},
	{Name: "One Third (1/3)", UseViewport: true, Num: 1, Den: 3},
	{Name: "Quarter (1/4)", UseViewport: true, Num: 1, Den: 4},
}

func paneWidthForPreset(viewportWidth int, preset paneWidthPreset) int {
	if !preset.UseViewport {
		return DefaultPaneWidth
	}
	if viewportWidth <= 0 || preset.Den <= 0 {
		return DefaultPaneWidth
	}
	w := (viewportWidth * preset.Num) / preset.Den
	if w < 20 {
		w = 20
	}
	return w
}

func colorTag(c tcell.Color) string {
	r, g, b := c.TrueColor().RGB()
	return fmt.Sprintf("[#%02x%02x%02x]", r, g, b)
}

func workspaceColorIndex(current string) int {
	for i, c := range workspaceColors {
		if strings.EqualFold(c.Name, current) {
			return i
		}
	}
	return 0
}

func autoWorkspaceColor(workspaces []*Workspace) workspaceColorOption {
	if len(workspaceColors) == 0 {
		return workspaceColorOption{Name: "Blue", Color: tcell.ColorBlue}
	}
	used := make(map[tcell.Color]bool)
	for _, ws := range workspaces {
		if ws != nil {
			used[ws.Color] = true
		}
	}
	for _, c := range workspaceColors {
		if !used[c.Color] {
			return c
		}
	}
	return workspaceColors[0]
}
