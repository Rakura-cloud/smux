package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"golang.org/x/term"
)

const (
	SidebarWidth     = 18
	ScrollbackSize   = 1000
	DefaultPaneWidth = 82 // 80 cols + 2 for border
)

// ---------------------------------------------------------------------------
// Cell: a single terminal cell with character and style
// ---------------------------------------------------------------------------

type Cell struct {
	Ch    rune
	Style tcell.Style
}

var defaultStyle = tcell.StyleDefault.Foreground(tcell.ColorSilver).Background(tcell.ColorBlack)

func defaultCell() Cell {
	return Cell{Ch: ' ', Style: defaultStyle}
}

// ---------------------------------------------------------------------------
// DEC Special Character Set (line drawing characters)
// Maps ASCII 0x60-0x7E to Unicode box-drawing equivalents when G0 = line drawing
// ---------------------------------------------------------------------------

var decSpecialChars = map[rune]rune{
	'`': '\u25C6', // diamond
	'a': '\u2592', // checkerboard
	'b': '\u2409', // HT symbol
	'c': '\u240C', // FF symbol
	'd': '\u240D', // CR symbol
	'e': '\u240A', // LF symbol
	'f': '\u00B0', // degree
	'g': '\u00B1', // plus/minus
	'h': '\u2424', // NL symbol
	'i': '\u240B', // VT symbol
	'j': '\u2518', // lower-right corner ┘
	'k': '\u2510', // upper-right corner ┐
	'l': '\u250C', // upper-left corner ┌
	'm': '\u2514', // lower-left corner └
	'n': '\u253C', // crossing lines ┼
	'o': '\u23BA', // scan line 1
	'p': '\u23BB', // scan line 3
	'q': '\u2500', // horizontal line ─
	'r': '\u23BC', // scan line 7
	's': '\u23BD', // scan line 9
	't': '\u251C', // left tee ├
	'u': '\u2524', // right tee ┤
	'v': '\u2534', // bottom tee ┴
	'w': '\u252C', // top tee ┬
	'x': '\u2502', // vertical line │
	'y': '\u2264', // less-than-or-equal
	'z': '\u2265', // greater-than-or-equal
	'{': '\u03C0', // pi
	'|': '\u2260', // not-equal
	'}': '\u00A3', // pound sign
	'~': '\u00B7', // middle dot
}

// ---------------------------------------------------------------------------
// VTerm: VT100/xterm virtual terminal emulator with color, alternate screen,
// scroll regions, UTF-8, character sets, and application cursor key support
// ---------------------------------------------------------------------------

type VTerm struct {
	mu   sync.Mutex
	rows int
	cols int

	// Response writer (writes back to PTY master for DSR etc.)
	responseWriter io.Writer

	// Primary screen
	curRow     int
	curCol     int
	screen     [][]Cell
	scrollback [][]Cell
	curStyle   tcell.Style
	savedRow   int
	savedCol   int
	savedStyle tcell.Style

	// Alternate screen buffer
	altScreen     [][]Cell
	altCurRow     int
	altCurCol     int
	altCurStyle   tcell.Style
	altSavedRow   int
	altSavedCol   int
	altSavedStyle tcell.Style
	useAltScreen  bool

	// Scroll region (1-indexed internally stored as 0-indexed)
	scrollTop    int // top row of scroll region (inclusive)
	scrollBottom int // bottom row of scroll region (inclusive)

	// Modes
	applicationCursorKeys bool // DECCKM (mode 1)
	autoWrap              bool // DECAWM (mode 7)
	originMode            bool // DECOM (mode 6)
	cursorVisible         bool // DECTCEM (mode 25)

	// Character set
	charsetG0  byte // 'B' = ASCII, '0' = DEC Special
	charsetG1  byte
	activeCS   int  // 0 = G0, 1 = G1
	escCharset byte // tracks ESC ( / ESC ) for next byte

	// Parser state
	parseState  int
	csiBuf      string
	oscBuf      string
	lastChar    rune // for CSI b (repeat)
	windowTitle string

	// UTF-8 decoder
	utf8Buf    [4]byte
	utf8Expect int // number of continuation bytes expected
	utf8Len    int // bytes accumulated so far
}

const (
	stateGround  = 0
	stateESC     = 1
	stateCSI     = 2
	stateOSC     = 3
	stateCharset = 4 // waiting for charset designator after ESC ( or ESC )
	stateOSCESC  = 5 // saw ESC inside OSC, waiting for '\' to complete ST
)

func NewVTerm(rows, cols int) *VTerm {
	vt := &VTerm{
		rows:          rows,
		cols:          cols,
		curStyle:      defaultStyle,
		altCurStyle:   defaultStyle,
		scrollTop:     0,
		scrollBottom:  rows - 1,
		autoWrap:      true,
		cursorVisible: true,
		charsetG0:     'B',
		charsetG1:     'B',
	}
	vt.screen = makeCellScreen(rows, cols)
	vt.altScreen = makeCellScreen(rows, cols)
	vt.scrollback = make([][]Cell, 0, ScrollbackSize)
	return vt
}

func makeCellScreen(rows, cols int) [][]Cell {
	s := make([][]Cell, rows)
	for i := range s {
		s[i] = makeCellLine(cols)
	}
	return s
}

func makeCellLine(cols int) []Cell {
	line := make([]Cell, cols)
	for i := range line {
		line[i] = defaultCell()
	}
	return line
}

func (vt *VTerm) activeScreen() [][]Cell {
	if vt.useAltScreen {
		return vt.altScreen
	}
	return vt.screen
}

// Resize the virtual terminal, preserving content where possible.
func (vt *VTerm) Resize(rows, cols int) {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	if rows <= 0 || cols <= 0 {
		return
	}

	vt.screen = resizeScreen(vt.screen, vt.rows, rows, cols)
	vt.altScreen = resizeScreen(vt.altScreen, vt.rows, rows, cols)

	vt.rows = rows
	vt.cols = cols
	vt.scrollTop = 0
	vt.scrollBottom = rows - 1

	vt.curRow = clamp(vt.curRow, 0, rows-1)
	vt.curCol = clamp(vt.curCol, 0, cols-1)
	vt.altCurRow = clamp(vt.altCurRow, 0, rows-1)
	vt.altCurCol = clamp(vt.altCurCol, 0, cols-1)
}

func resizeScreen(old [][]Cell, oldRows, newRows, newCols int) [][]Cell {
	ns := makeCellScreen(newRows, newCols)
	copyR := newRows
	if copyR > len(old) {
		copyR = len(old)
	}
	for r := 0; r < copyR; r++ {
		copyC := newCols
		if copyC > len(old[r]) {
			copyC = len(old[r])
		}
		copy(ns[r][:copyC], old[r][:copyC])
	}
	return ns
}

// Write processes raw PTY output through the VT state machine.
func (vt *VTerm) Write(data []byte) {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	for i := 0; i < len(data); i++ {
		b := data[i]

		// UTF-8 multi-byte handling in ground state
		if vt.parseState == stateGround && vt.utf8Expect == 0 && b >= 0x80 {
			if b&0xE0 == 0xC0 {
				vt.utf8Expect = 1
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				continue
			} else if b&0xF0 == 0xE0 {
				vt.utf8Expect = 2
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				continue
			} else if b&0xF8 == 0xF0 {
				vt.utf8Expect = 3
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				continue
			}
			// Invalid leading byte, skip
			continue
		}

		if vt.utf8Expect > 0 {
			if b&0xC0 == 0x80 {
				vt.utf8Buf[vt.utf8Len] = b
				vt.utf8Len++
				vt.utf8Expect--
				if vt.utf8Expect == 0 {
					r, _ := utf8.DecodeRune(vt.utf8Buf[:vt.utf8Len])
					if r != utf8.RuneError {
						vt.putChar(r)
					}
					vt.utf8Len = 0
				}
			} else {
				// Invalid continuation, reset and reprocess this byte
				vt.utf8Expect = 0
				vt.utf8Len = 0
				i-- // reprocess
			}
			continue
		}

		ch := rune(b)
		switch vt.parseState {
		case stateGround:
			vt.groundState(ch)
		case stateESC:
			vt.escState(ch)
		case stateCSI:
			vt.csiState(ch)
		case stateOSC:
			vt.oscState(ch)
		case stateOSCESC:
			if ch == '\\' {
				// ST complete (ESC \), done with OSC
				vt.finishOSC()
				vt.parseState = stateGround
			} else {
				// Not ST, treat the ESC as having ended the OSC,
				// then process this character as ESC sequence start
				vt.finishOSC()
				vt.parseState = stateESC
				vt.escState(ch)
			}
		case stateCharset:
			vt.charsetState(ch)
		}
	}
}

func (vt *VTerm) putChar(ch rune) {
	scr := vt.activeScreen()
	row, col := vt.cursorPos()

	if col >= vt.cols {
		if vt.autoWrap {
			vt.setCurCol(0)
			vt.index()
			col = 0
			row = vt.cursorRow()
			scr = vt.activeScreen()
		} else {
			col = vt.cols - 1
			vt.setCurCol(col)
		}
	}

	// Apply character set translation
	mapped := ch
	cs := vt.charsetG0
	if vt.activeCS == 1 {
		cs = vt.charsetG1
	}
	if cs == '0' {
		if replacement, ok := decSpecialChars[ch]; ok {
			mapped = replacement
		}
	}

	scr[row][col] = Cell{Ch: mapped, Style: vt.curStyle}
	vt.lastChar = mapped
	vt.setCurCol(col + 1)
}

func (vt *VTerm) cursorPos() (int, int) {
	if vt.useAltScreen {
		return vt.altCurRow, vt.altCurCol
	}
	return vt.curRow, vt.curCol
}

func (vt *VTerm) cursorRow() int {
	if vt.useAltScreen {
		return vt.altCurRow
	}
	return vt.curRow
}

func (vt *VTerm) setCurRow(r int) {
	if vt.useAltScreen {
		vt.altCurRow = r
	} else {
		vt.curRow = r
	}
}

func (vt *VTerm) setCurCol(c int) {
	if vt.useAltScreen {
		vt.altCurCol = c
	} else {
		vt.curCol = c
	}
}

func (vt *VTerm) groundState(ch rune) {
	switch {
	case ch == 0x1b: // ESC
		vt.parseState = stateESC
	case ch == '\n', ch == '\x0b', ch == '\x0c': // LF, VT, FF
		vt.index()
	case ch == '\r': // CR
		vt.setCurCol(0)
	case ch == '\t': // Tab
		_, c := vt.cursorPos()
		newCol := (c + 8) &^ 7
		if newCol >= vt.cols {
			newCol = vt.cols - 1
		}
		vt.setCurCol(newCol)
	case ch == '\b': // Backspace
		_, c := vt.cursorPos()
		if c > 0 {
			vt.setCurCol(c - 1)
		}
	case ch == '\a': // Bell
	case ch == 0x0e: // Shift Out - activate G1
		vt.activeCS = 1
	case ch == 0x0f: // Shift In - activate G0
		vt.activeCS = 0
	case ch < 0x20: // Other control chars
	default:
		vt.putChar(ch)
	}
}

func (vt *VTerm) escState(ch rune) {
	switch ch {
	case '[':
		vt.parseState = stateCSI
		vt.csiBuf = ""
	case ']':
		vt.parseState = stateOSC
		vt.oscBuf = ""
	case '(': // Designate G0 character set
		vt.escCharset = '('
		vt.parseState = stateCharset
	case ')': // Designate G1 character set
		vt.escCharset = ')'
		vt.parseState = stateCharset
	case '7': // DECSC - save cursor
		vt.saveCursor()
		vt.parseState = stateGround
	case '8': // DECRC - restore cursor
		vt.restoreCursor()
		vt.parseState = stateGround
	case 'D': // IND - index (move down, scroll if at bottom of region)
		vt.index()
		vt.parseState = stateGround
	case 'M': // RI - reverse index
		vt.reverseIndex()
		vt.parseState = stateGround
	case 'E': // NEL - next line
		vt.setCurCol(0)
		vt.index()
		vt.parseState = stateGround
	case 'c': // RIS - full reset
		vt.fullReset()
		vt.parseState = stateGround
	case '=': // DECKPAM - application keypad
		vt.parseState = stateGround
	case '>': // DECKPNM - normal keypad
		vt.parseState = stateGround
	case '*', '+', '-', '.': // Other charset designators
		vt.escCharset = byte(ch)
		vt.parseState = stateCharset
	default:
		vt.parseState = stateGround
	}
}

func (vt *VTerm) charsetState(ch rune) {
	switch vt.escCharset {
	case '(':
		vt.charsetG0 = byte(ch) // 'B' = ASCII, '0' = DEC Special
	case ')':
		vt.charsetG1 = byte(ch)
	}
	vt.parseState = stateGround
}

func (vt *VTerm) csiState(ch rune) {
	if (ch >= '0' && ch <= '9') || ch == ';' || ch == '?' || ch == '>' || ch == '!' || ch == ' ' || ch == '"' || ch == '\'' {
		vt.csiBuf += string(ch)
		return
	}
	vt.parseState = stateGround
	vt.handleCSI(ch, vt.csiBuf)
}

func (vt *VTerm) oscState(ch rune) {
	if ch == '\a' {
		vt.finishOSC()
		vt.parseState = stateGround
		return
	}
	if ch == 0x1b {
		// Might be ST (ESC \), wait for next byte
		vt.parseState = stateOSCESC
		return
	}
	vt.oscBuf += string(ch)
	if len(vt.oscBuf) > 4096 {
		vt.parseState = stateGround
	}
}

func (vt *VTerm) finishOSC() {
	buf := vt.oscBuf
	vt.oscBuf = ""
	parts := strings.SplitN(buf, ";", 2)
	if len(parts) != 2 {
		return
	}
	ps := strings.TrimSpace(parts[0])
	pt := strings.TrimSpace(parts[1])
	if ps == "0" || ps == "1" || ps == "2" {
		vt.windowTitle = pt
	}
}

func (vt *VTerm) WindowTitle() string {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	return strings.TrimSpace(vt.windowTitle)
}

func (vt *VTerm) saveCursor() {
	if vt.useAltScreen {
		vt.altSavedRow = vt.altCurRow
		vt.altSavedCol = vt.altCurCol
		vt.altSavedStyle = vt.curStyle
	} else {
		vt.savedRow = vt.curRow
		vt.savedCol = vt.curCol
		vt.savedStyle = vt.curStyle
	}
}

func (vt *VTerm) restoreCursor() {
	if vt.useAltScreen {
		vt.altCurRow = vt.altSavedRow
		vt.altCurCol = vt.altSavedCol
		vt.curStyle = vt.altSavedStyle
	} else {
		vt.curRow = vt.savedRow
		vt.curCol = vt.savedCol
		vt.curStyle = vt.savedStyle
	}
}

func (vt *VTerm) fullReset() {
	vt.screen = makeCellScreen(vt.rows, vt.cols)
	vt.altScreen = makeCellScreen(vt.rows, vt.cols)
	vt.curRow = 0
	vt.curCol = 0
	vt.altCurRow = 0
	vt.altCurCol = 0
	vt.curStyle = defaultStyle
	vt.altCurStyle = defaultStyle
	vt.useAltScreen = false
	vt.scrollTop = 0
	vt.scrollBottom = vt.rows - 1
	vt.applicationCursorKeys = false
	vt.cursorVisible = true
	vt.autoWrap = true
	vt.originMode = false
	vt.charsetG0 = 'B'
	vt.charsetG1 = 'B'
	vt.activeCS = 0
	vt.windowTitle = ""
}

// index moves the cursor down one line. If at the bottom of the scroll region,
// scrolls the region up instead.
func (vt *VTerm) index() {
	row := vt.cursorRow()
	if row == vt.scrollBottom {
		vt.scrollRegionUp(1)
	} else if row < vt.rows-1 {
		vt.setCurRow(row + 1)
	}
}

// reverseIndex moves the cursor up one line. If at the top of the scroll region,
// scrolls the region down instead.
func (vt *VTerm) reverseIndex() {
	row := vt.cursorRow()
	if row == vt.scrollTop {
		vt.scrollRegionDown(1)
	} else if row > 0 {
		vt.setCurRow(row - 1)
	}
}

// scrollRegionUp scrolls lines within the scroll region up by n lines.
func (vt *VTerm) scrollRegionUp(n int) {
	scr := vt.activeScreen()
	top := vt.scrollTop
	bot := vt.scrollBottom

	for i := 0; i < n; i++ {
		// On main screen with full-screen scroll region, save to scrollback
		if !vt.useAltScreen && top == 0 && bot == vt.rows-1 {
			saveLine := make([]Cell, vt.cols)
			copy(saveLine, scr[0])
			vt.scrollback = append(vt.scrollback, saveLine)
			if len(vt.scrollback) > ScrollbackSize {
				vt.scrollback = vt.scrollback[len(vt.scrollback)-ScrollbackSize:]
			}
		}
		// Shift lines up within the region
		for r := top; r < bot; r++ {
			scr[r] = scr[r+1]
		}
		scr[bot] = makeCellLine(vt.cols)
	}
	// Update the active screen reference
	if vt.useAltScreen {
		vt.altScreen = scr
	} else {
		vt.screen = scr
	}
}

// scrollRegionDown scrolls lines within the scroll region down by n lines.
func (vt *VTerm) scrollRegionDown(n int) {
	scr := vt.activeScreen()
	top := vt.scrollTop
	bot := vt.scrollBottom

	for i := 0; i < n; i++ {
		for r := bot; r > top; r-- {
			scr[r] = scr[r-1]
		}
		scr[top] = makeCellLine(vt.cols)
	}
	if vt.useAltScreen {
		vt.altScreen = scr
	} else {
		vt.screen = scr
	}
}

func (vt *VTerm) clearToEndOfLine() {
	scr := vt.activeScreen()
	row, col := vt.cursorPos()
	for c := col; c < vt.cols; c++ {
		scr[row][c] = defaultCell()
	}
}

func (vt *VTerm) handleCSI(final rune, params string) {
	private := false
	cleanParams := params
	if len(cleanParams) > 0 && (cleanParams[0] == '?' || cleanParams[0] == '>') {
		private = true
		cleanParams = cleanParams[1:]
	}
	// Strip any trailing space or other intermediate bytes
	cleanParams = strings.TrimRight(cleanParams, " \"'!")

	args := parseSemicolonArgs(cleanParams)
	scr := vt.activeScreen()
	row, col := vt.cursorPos()

	switch final {
	case 'A': // CUU - cursor up
		n := argOrDefault(args, 0, 1)
		minRow := 0
		if row >= vt.scrollTop && row <= vt.scrollBottom {
			minRow = vt.scrollTop
		}
		vt.setCurRow(max(minRow, row-n))
	case 'B': // CUD - cursor down
		n := argOrDefault(args, 0, 1)
		maxRow := vt.rows - 1
		if row >= vt.scrollTop && row <= vt.scrollBottom {
			maxRow = vt.scrollBottom
		}
		vt.setCurRow(min(maxRow, row+n))
	case 'C': // CUF - cursor forward
		n := argOrDefault(args, 0, 1)
		vt.setCurCol(min(vt.cols-1, col+n))
	case 'D': // CUB - cursor back
		n := argOrDefault(args, 0, 1)
		vt.setCurCol(max(0, col-n))
	case 'E': // CNL - cursor next line
		n := argOrDefault(args, 0, 1)
		vt.setCurCol(0)
		vt.setCurRow(min(vt.rows-1, row+n))
	case 'F': // CPL - cursor previous line
		n := argOrDefault(args, 0, 1)
		vt.setCurCol(0)
		vt.setCurRow(max(0, row-n))
	case 'G': // CHA - cursor horizontal absolute
		n := argOrDefault(args, 0, 1) - 1
		vt.setCurCol(clamp(n, 0, vt.cols-1))
	case 'H', 'f': // CUP - cursor position
		r := argOrDefault(args, 0, 1) - 1
		c := argOrDefault(args, 1, 1) - 1
		if vt.originMode {
			r += vt.scrollTop
		}
		vt.setCurRow(clamp(r, 0, vt.rows-1))
		vt.setCurCol(clamp(c, 0, vt.cols-1))
	case 'J': // ED - erase in display
		n := argOrDefault(args, 0, 0)
		row, col = vt.cursorPos()
		switch n {
		case 0: // cursor to end
			for c := col; c < vt.cols; c++ {
				scr[row][c] = defaultCell()
			}
			for r := row + 1; r < vt.rows; r++ {
				scr[r] = makeCellLine(vt.cols)
			}
		case 1: // start to cursor
			for c := 0; c <= col && c < vt.cols; c++ {
				scr[row][c] = defaultCell()
			}
			for r := 0; r < row; r++ {
				scr[r] = makeCellLine(vt.cols)
			}
		case 2, 3: // entire screen
			for r := 0; r < vt.rows; r++ {
				scr[r] = makeCellLine(vt.cols)
			}
		}
	case 'K': // EL - erase in line
		n := argOrDefault(args, 0, 0)
		row, col = vt.cursorPos()
		switch n {
		case 0:
			for c := col; c < vt.cols; c++ {
				scr[row][c] = defaultCell()
			}
		case 1:
			for c := 0; c <= col && c < vt.cols; c++ {
				scr[row][c] = defaultCell()
			}
		case 2:
			scr[row] = makeCellLine(vt.cols)
		}
	case 'L': // IL - insert lines
		n := argOrDefault(args, 0, 1)
		row = vt.cursorRow()
		if row >= vt.scrollTop && row <= vt.scrollBottom {
			for i := 0; i < n; i++ {
				for r := vt.scrollBottom; r > row; r-- {
					scr[r] = scr[r-1]
				}
				scr[row] = makeCellLine(vt.cols)
			}
		}
	case 'M': // DL - delete lines
		n := argOrDefault(args, 0, 1)
		row = vt.cursorRow()
		if row >= vt.scrollTop && row <= vt.scrollBottom {
			for i := 0; i < n; i++ {
				for r := row; r < vt.scrollBottom; r++ {
					scr[r] = scr[r+1]
				}
				scr[vt.scrollBottom] = makeCellLine(vt.cols)
			}
		}
	case 'P': // DCH - delete characters
		n := argOrDefault(args, 0, 1)
		row = vt.cursorRow()
		line := scr[row]
		_, col = vt.cursorPos()
		end := col + n
		if end > vt.cols {
			end = vt.cols
		}
		copy(line[col:], line[end:])
		for c := vt.cols - n; c < vt.cols; c++ {
			if c >= 0 {
				line[c] = defaultCell()
			}
		}
	case '@': // ICH - insert characters
		n := argOrDefault(args, 0, 1)
		row = vt.cursorRow()
		line := scr[row]
		_, col = vt.cursorPos()
		for c := vt.cols - 1; c >= col+n; c-- {
			line[c] = line[c-n]
		}
		for c := col; c < col+n && c < vt.cols; c++ {
			line[c] = defaultCell()
		}
	case 'X': // ECH - erase characters
		n := argOrDefault(args, 0, 1)
		row, col = vt.cursorPos()
		for c := col; c < col+n && c < vt.cols; c++ {
			scr[row][c] = defaultCell()
		}
	case 'S': // SU - scroll up
		n := argOrDefault(args, 0, 1)
		vt.scrollRegionUp(n)
	case 'T': // SD - scroll down
		if !private && len(args) <= 1 {
			n := argOrDefault(args, 0, 1)
			vt.scrollRegionDown(n)
		}
	case 'b': // REP - repeat last character
		n := argOrDefault(args, 0, 1)
		if vt.lastChar != 0 {
			for i := 0; i < n; i++ {
				vt.putChar(vt.lastChar)
			}
		}
	case 'd': // VPA - vertical line position absolute
		n := argOrDefault(args, 0, 1) - 1
		vt.setCurRow(clamp(n, 0, vt.rows-1))
	case 'r': // DECSTBM - set scrolling region
		if private {
			break
		}
		top := argOrDefault(args, 0, 1) - 1
		bot := argOrDefault(args, 1, vt.rows) - 1
		if top < 0 {
			top = 0
		}
		if bot >= vt.rows {
			bot = vt.rows - 1
		}
		if top < bot {
			vt.scrollTop = top
			vt.scrollBottom = bot
		}
		// DECSTBM moves cursor to home
		if vt.originMode {
			vt.setCurRow(vt.scrollTop)
		} else {
			vt.setCurRow(0)
		}
		vt.setCurCol(0)
	case 's': // SCP or DECSLRM
		if !private {
			vt.saveCursor()
		}
	case 'u': // RCP
		if !private {
			vt.restoreCursor()
		}
	case 'h': // SM - set mode
		vt.setMode(args, private, true)
	case 'l': // RM - reset mode
		vt.setMode(args, private, false)
	case 'm': // SGR
		vt.handleSGR(args)
	case 'n': // DSR - device status report
		if !private && len(args) > 0 && args[0] == 6 {
			// CPR - cursor position report
			r, c := vt.cursorPos()
			response := fmt.Sprintf("\x1b[%d;%dR", r+1, c+1)
			if vt.responseWriter != nil {
				vt.responseWriter.Write([]byte(response))
			}
		}
	case 'c': // DA - device attributes
		// Ignore
	case 't': // Window manipulation
		// Ignore
	case 'q': // DECSCUSR - cursor style (with space prefix)
		// Ignore
	}
}

func (vt *VTerm) setMode(args []int, private bool, set bool) {
	if !private {
		return
	}
	for _, mode := range args {
		switch mode {
		case 1: // DECCKM - application cursor keys
			vt.applicationCursorKeys = set
		case 6: // DECOM - origin mode
			vt.originMode = set
			if set {
				vt.setCurRow(vt.scrollTop)
			} else {
				vt.setCurRow(0)
			}
			vt.setCurCol(0)
		case 7: // DECAWM - auto wrap
			vt.autoWrap = set
		case 25: // DECTCEM - cursor visibility
			vt.cursorVisible = set
		case 47: // Alternate screen buffer (old style)
			if set {
				vt.switchToAltScreen()
			} else {
				vt.switchToMainScreen()
			}
		case 1000, 1002, 1003, 1006: // Mouse tracking modes
			// Ignore
		case 1049: // Alternate screen buffer with save/restore cursor
			if set {
				vt.saveCursor()
				vt.switchToAltScreen()
			} else {
				vt.switchToMainScreen()
				vt.restoreCursor()
			}
		case 1047: // Alternate screen buffer
			if set {
				vt.switchToAltScreen()
			} else {
				vt.switchToMainScreen()
			}
		case 2004: // Bracketed paste mode
			// Ignore
		case 12: // Cursor blinking
			// Ignore
		}
	}
}

func (vt *VTerm) switchToAltScreen() {
	if vt.useAltScreen {
		return
	}
	// Save main screen style before switching
	vt.savedStyle = vt.curStyle
	vt.useAltScreen = true
	// Clear alt screen
	vt.altScreen = makeCellScreen(vt.rows, vt.cols)
	vt.altCurRow = 0
	vt.altCurCol = 0
	vt.altCurStyle = vt.curStyle
	vt.scrollTop = 0
	vt.scrollBottom = vt.rows - 1
}

func (vt *VTerm) switchToMainScreen() {
	if !vt.useAltScreen {
		return
	}
	// Save alt screen style
	vt.altCurStyle = vt.curStyle
	vt.useAltScreen = false
	// Restore main screen style
	vt.curStyle = vt.savedStyle
	vt.scrollTop = 0
	vt.scrollBottom = vt.rows - 1
}

// handleSGR processes CSI ... m (Select Graphic Rendition) sequences.
func (vt *VTerm) handleSGR(args []int) {
	if len(args) == 0 {
		vt.curStyle = defaultStyle
		return
	}

	i := 0
	for i < len(args) {
		code := args[i]
		switch {
		case code == 0:
			vt.curStyle = defaultStyle
		case code == 1:
			vt.curStyle = vt.curStyle.Bold(true)
		case code == 2:
			vt.curStyle = vt.curStyle.Dim(true)
		case code == 3:
			vt.curStyle = vt.curStyle.Italic(true)
		case code == 4:
			vt.curStyle = vt.curStyle.Underline(true)
		case code == 5 || code == 6:
			vt.curStyle = vt.curStyle.Blink(true)
		case code == 7:
			vt.curStyle = vt.curStyle.Reverse(true)
		case code == 8:
			// Hidden - not supported
		case code == 9:
			vt.curStyle = vt.curStyle.StrikeThrough(true)
		case code == 21:
			vt.curStyle = vt.curStyle.Underline(true)
		case code == 22:
			vt.curStyle = vt.curStyle.Bold(false).Dim(false)
		case code == 23:
			vt.curStyle = vt.curStyle.Italic(false)
		case code == 24:
			vt.curStyle = vt.curStyle.Underline(false)
		case code == 25:
			vt.curStyle = vt.curStyle.Blink(false)
		case code == 27:
			vt.curStyle = vt.curStyle.Reverse(false)
		case code == 28:
			// Not hidden
		case code == 29:
			vt.curStyle = vt.curStyle.StrikeThrough(false)
		case code >= 30 && code <= 37:
			vt.curStyle = vt.curStyle.Foreground(sgrColor(code - 30))
		case code == 38:
			if i+1 < len(args) {
				switch args[i+1] {
				case 5:
					if i+2 < len(args) {
						vt.curStyle = vt.curStyle.Foreground(color256(args[i+2]))
						i += 2
					}
				case 2:
					if i+4 < len(args) {
						vt.curStyle = vt.curStyle.Foreground(tcell.NewRGBColor(
							int32(args[i+2]), int32(args[i+3]), int32(args[i+4]),
						))
						i += 4
					}
				}
			}
		case code == 39:
			vt.curStyle = vt.curStyle.Foreground(tcell.ColorSilver)
		case code >= 40 && code <= 47:
			vt.curStyle = vt.curStyle.Background(sgrColor(code - 40))
		case code == 48:
			if i+1 < len(args) {
				switch args[i+1] {
				case 5:
					if i+2 < len(args) {
						vt.curStyle = vt.curStyle.Background(color256(args[i+2]))
						i += 2
					}
				case 2:
					if i+4 < len(args) {
						vt.curStyle = vt.curStyle.Background(tcell.NewRGBColor(
							int32(args[i+2]), int32(args[i+3]), int32(args[i+4]),
						))
						i += 4
					}
				}
			}
		case code == 49:
			vt.curStyle = vt.curStyle.Background(tcell.ColorBlack)
		case code >= 90 && code <= 97:
			vt.curStyle = vt.curStyle.Foreground(sgrBrightColor(code - 90))
		case code >= 100 && code <= 107:
			vt.curStyle = vt.curStyle.Background(sgrBrightColor(code - 100))
		}
		i++
	}
}

func sgrColor(idx int) tcell.Color {
	colors := [8]tcell.Color{
		tcell.ColorBlack, tcell.ColorMaroon, tcell.ColorGreen, tcell.ColorOlive,
		tcell.ColorNavy, tcell.ColorPurple, tcell.ColorTeal, tcell.ColorSilver,
	}
	if idx >= 0 && idx < 8 {
		return colors[idx]
	}
	return tcell.ColorSilver
}

func sgrBrightColor(idx int) tcell.Color {
	colors := [8]tcell.Color{
		tcell.ColorGray, tcell.ColorRed, tcell.ColorLime, tcell.ColorYellow,
		tcell.ColorBlue, tcell.ColorFuchsia, tcell.ColorAqua, tcell.ColorWhite,
	}
	if idx >= 0 && idx < 8 {
		return colors[idx]
	}
	return tcell.ColorWhite
}

func color256(idx int) tcell.Color {
	if idx < 0 || idx > 255 {
		return tcell.ColorSilver
	}
	return tcell.PaletteColor(idx)
}

// GetCells returns the visible screen (or alt screen) as a 2D slice of Cells.
func (vt *VTerm) GetCells(scrollOff int) [][]Cell {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	scr := vt.activeScreen()
	result := make([][]Cell, vt.rows)

	// Alt screen has no scrollback
	if vt.useAltScreen || scrollOff <= 0 {
		for i := 0; i < vt.rows; i++ {
			row := make([]Cell, vt.cols)
			copy(row, scr[i])
			result[i] = row
		}
		return result
	}

	// Main screen with scrollback
	if scrollOff > len(vt.scrollback) {
		scrollOff = len(vt.scrollback)
	}
	sbStart := len(vt.scrollback) - scrollOff
	if sbStart < 0 {
		sbStart = 0
	}
	lineIdx := 0
	for i := sbStart; i < len(vt.scrollback) && lineIdx < vt.rows; i++ {
		row := make([]Cell, vt.cols)
		copy(row, vt.scrollback[i])
		result[lineIdx] = row
		lineIdx++
	}
	screenStart := 0
	for lineIdx < vt.rows && screenStart < vt.rows {
		row := make([]Cell, vt.cols)
		copy(row, scr[screenStart])
		result[lineIdx] = row
		lineIdx++
		screenStart++
	}

	return result
}

// ApplicationCursorKeys returns whether application cursor key mode is active.
func (vt *VTerm) ApplicationCursorKeys() bool {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	return vt.applicationCursorKeys
}

// CursorPos returns the current cursor row, col, and visibility.
// Row and col are 0-indexed. Only meaningful when scrollOff == 0 (live view).
func (vt *VTerm) CursorPos() (int, int, bool) {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	if vt.useAltScreen {
		return vt.altCurRow, vt.altCurCol, vt.cursorVisible
	}
	return vt.curRow, vt.curCol, vt.cursorVisible
}

// Helpers

func parseSemicolonArgs(s string) []int {
	if s == "" {
		return nil
	}
	clean := strings.Builder{}
	for _, ch := range s {
		if (ch >= '0' && ch <= '9') || ch == ';' {
			clean.WriteRune(ch)
		}
	}
	if clean.Len() == 0 {
		return nil
	}
	parts := strings.Split(clean.String(), ";")
	args := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			n = 0
		}
		args[i] = n
	}
	return args
}

func argOrDefault(args []int, idx, def int) int {
	if idx < len(args) && args[idx] > 0 {
		return args[idx]
	}
	return def
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// Rounded border drawing
// ---------------------------------------------------------------------------

const (
	cornerTL = '╭'
	cornerTR = '╮'
	cornerBL = '╰'
	cornerBR = '╯'
	horizBar = '─'
	vertBar  = '│'
)

// drawRoundedBorder draws a rounded border on screen at the given outer rect.
// If title is non-empty, it is rendered centered in the top border.
func drawRoundedBorder(screen tcell.Screen, x, y, w, h int, style tcell.Style, title string) {
	if w < 2 || h < 2 {
		return
	}

	// Corners
	screen.SetCell(x, y, style, cornerTL)
	screen.SetCell(x+w-1, y, style, cornerTR)
	screen.SetCell(x, y+h-1, style, cornerBL)
	screen.SetCell(x+w-1, y+h-1, style, cornerBR)

	// Top and bottom edges
	for i := 1; i < w-1; i++ {
		screen.SetCell(x+i, y, style, horizBar)
		screen.SetCell(x+i, y+h-1, style, horizBar)
	}

	// Left and right edges
	for i := 1; i < h-1; i++ {
		screen.SetCell(x, y+i, style, vertBar)
		screen.SetCell(x+w-1, y+i, style, vertBar)
	}

	// Title (centered in top border)
	if title != "" {
		maxTitleW := w - 4 // leave 2 cells padding on each side
		if maxTitleW < 1 {
			return
		}
		titleRunes := []rune(title)
		if len(titleRunes) > maxTitleW {
			titleRunes = titleRunes[:maxTitleW]
		}
		startX := x + (w-len(titleRunes))/2
		titleStyle := style.Bold(true)
		for i, r := range titleRunes {
			screen.SetCell(startX+i, y, titleStyle, r)
		}
	}
}

// drawClippedBorder draws a rounded border with title, clipped to a viewport rect.
// bx,by,bw,bh = border rect; cx,cy,cw,ch = clip (viewport) rect.
func drawClippedBorder(screen tcell.Screen, bx, by, bw, bh, cx, cy, cw, ch int, style tcell.Style, title string) {
	if bw < 2 || bh < 2 {
		return
	}

	clip := func(sx, sy int) bool {
		return sx >= cx && sx < cx+cw && sy >= cy && sy < cy+ch
	}

	// Corners
	if clip(bx, by) {
		screen.SetCell(bx, by, style, cornerTL)
	}
	if clip(bx+bw-1, by) {
		screen.SetCell(bx+bw-1, by, style, cornerTR)
	}
	if clip(bx, by+bh-1) {
		screen.SetCell(bx, by+bh-1, style, cornerBL)
	}
	if clip(bx+bw-1, by+bh-1) {
		screen.SetCell(bx+bw-1, by+bh-1, style, cornerBR)
	}

	// Top and bottom edges
	for i := 1; i < bw-1; i++ {
		if clip(bx+i, by) {
			screen.SetCell(bx+i, by, style, horizBar)
		}
		if clip(bx+i, by+bh-1) {
			screen.SetCell(bx+i, by+bh-1, style, horizBar)
		}
	}

	// Left and right edges
	for i := 1; i < bh-1; i++ {
		if clip(bx, by+i) {
			screen.SetCell(bx, by+i, style, vertBar)
		}
		if clip(bx+bw-1, by+i) {
			screen.SetCell(bx+bw-1, by+i, style, vertBar)
		}
	}

	// Title (left-aligned in top border, after corner+1 space)
	if title != "" {
		maxTitleW := bw - 4
		if maxTitleW < 1 {
			return
		}
		titleRunes := []rune(title)
		if len(titleRunes) > maxTitleW {
			titleRunes = titleRunes[:maxTitleW]
		}
		startX := bx + 2 // left-aligned with 2-cell offset
		titleStyle := style.Bold(true)
		for i, r := range titleRunes {
			if clip(startX+i, by) {
				screen.SetCell(startX+i, by, titleStyle, r)
			}
		}
	}
}

// roundCorners overdraws the 4 corners of a tview bordered box with rounded chars.
// Call this after tview has drawn the box's default border.
func roundCorners(screen tcell.Screen, box *tview.Box, borderStyle tcell.Style) {
	x, y, w, h := box.GetRect()
	if w < 2 || h < 2 {
		return
	}
	screen.SetCell(x, y, borderStyle, cornerTL)
	screen.SetCell(x+w-1, y, borderStyle, cornerTR)
	screen.SetCell(x, y+h-1, borderStyle, cornerBL)
	screen.SetCell(x+w-1, y+h-1, borderStyle, cornerBR)
}

func socketDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".terminal-multiplexer")
}

func socketPathForSession(name string) string {
	if strings.TrimSpace(name) == "" {
		name = "default"
	}
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	return filepath.Join(socketDir(), clean+".sock")
}

func listSessions() ([]string, error) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sessions := make([]string, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sock") {
			sessions = append(sessions, strings.TrimSuffix(name, ".sock"))
		}
	}
	sort.Strings(sessions)
	return sessions, nil
}

func serverRunning(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func startServerDaemon(name string) error {
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		return err
	}
	nullFile, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer nullFile.Close()
	cmd := exec.Command(os.Args[0], "internal", "--server", name)
	cmd.Stdin = nullFile
	cmd.Stdout = nullFile
	cmd.Stderr = nullFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func waitForServer(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if serverRunning(path) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for server socket: %s", path)
}

func main() {
	mode := "attach"
	session := "default"
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "attach":
			if len(args) > 1 {
				session = args[1]
			}
		case "list":
			mode = "list"
		case "internal":
			if len(args) > 1 && args[1] == "--server" {
				mode = "server"
				if len(args) > 2 {
					session = args[2]
				}
			} else {
				fmt.Fprintln(os.Stderr, "usage: internal --server [session]")
				os.Exit(2)
			}
		default:
			if strings.TrimSpace(args[0]) != "" {
				session = args[0]
			}
		}
	}

	switch mode {
	case "list":
		sessions, err := listSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
			os.Exit(1)
		}
		for _, s := range sessions {
			path := socketPathForSession(s)
			if serverRunning(path) {
				fmt.Println(s)
			}
		}
		return
	case "server":
		if err := runServer(session); err != nil {
			fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
			os.Exit(1)
		}
		return
	default:
		if err := runAttach(session); err != nil {
			fmt.Fprintf(os.Stderr, "attach failed: %v\n", err)
			os.Exit(1)
		}
	}
}

func runAttach(session string) error {
	path := socketPathForSession(session)
	if !serverRunning(path) {
		if err := startServerDaemon(session); err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		if err := waitForServer(path); err != nil {
			return err
		}
	}
	return runClient(session)
}

func wireCellStyle(c WireCell) tcell.Style {
	return tcell.StyleDefault.Foreground(tcell.Color(c.Fg)).Background(tcell.Color(c.Bg)).Attributes(tcell.AttrMask(c.Attrs))
}

func keyToInput(event *tcell.EventKey, appCursor bool) []byte {
	key := event.Key()
	ch := event.Rune()
	keyStr := ""
	switch key {
	case tcell.KeyEnter:
		keyStr = "\r"
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		keyStr = "\x7f"
	case tcell.KeyDelete:
		keyStr = "\x1b[3~"
	case tcell.KeyTab:
		keyStr = "\t"
	case tcell.KeyEsc:
		keyStr = "\x1b"
	case tcell.KeyUp:
		if appCursor {
			keyStr = "\x1bOA"
		} else {
			keyStr = "\x1b[A"
		}
	case tcell.KeyDown:
		if appCursor {
			keyStr = "\x1bOB"
		} else {
			keyStr = "\x1b[B"
		}
	case tcell.KeyRight:
		if appCursor {
			keyStr = "\x1bOC"
		} else {
			keyStr = "\x1b[C"
		}
	case tcell.KeyLeft:
		if appCursor {
			keyStr = "\x1bOD"
		} else {
			keyStr = "\x1b[D"
		}
	case tcell.KeyHome:
		if appCursor {
			keyStr = "\x1bOH"
		} else {
			keyStr = "\x1b[H"
		}
	case tcell.KeyEnd:
		if appCursor {
			keyStr = "\x1bOF"
		} else {
			keyStr = "\x1b[F"
		}
	case tcell.KeyInsert:
		keyStr = "\x1b[2~"
	case tcell.KeyF1:
		keyStr = "\x1bOP"
	case tcell.KeyF2:
		keyStr = "\x1bOQ"
	case tcell.KeyF3:
		keyStr = "\x1bOR"
	case tcell.KeyF4:
		keyStr = "\x1bOS"
	case tcell.KeyF5:
		keyStr = "\x1b[15~"
	case tcell.KeyF6:
		keyStr = "\x1b[17~"
	case tcell.KeyF7:
		keyStr = "\x1b[18~"
	case tcell.KeyF8:
		keyStr = "\x1b[19~"
	case tcell.KeyF9:
		keyStr = "\x1b[20~"
	case tcell.KeyF10:
		keyStr = "\x1b[21~"
	case tcell.KeyF11:
		keyStr = "\x1b[23~"
	case tcell.KeyF12:
		keyStr = "\x1b[24~"
	case tcell.KeyCtrlA:
		keyStr = "\x01"
	case tcell.KeyCtrlB:
		keyStr = "\x02"
	case tcell.KeyCtrlC:
		keyStr = "\x03"
	case tcell.KeyCtrlD:
		keyStr = "\x04"
	case tcell.KeyCtrlE:
		keyStr = "\x05"
	case tcell.KeyCtrlF:
		keyStr = "\x06"
	case tcell.KeyCtrlG:
		keyStr = "\x07"
	case tcell.KeyCtrlK:
		keyStr = "\x0b"
	case tcell.KeyCtrlL:
		keyStr = "\x0c"
	case tcell.KeyCtrlN:
		keyStr = "\x0e"
	case tcell.KeyCtrlO:
		keyStr = "\x0f"
	case tcell.KeyCtrlR:
		keyStr = "\x12"
	case tcell.KeyCtrlS:
		keyStr = "\x13"
	case tcell.KeyCtrlT:
		keyStr = "\x14"
	case tcell.KeyCtrlU:
		keyStr = "\x15"
	case tcell.KeyCtrlV:
		keyStr = "\x16"
	case tcell.KeyCtrlW:
		keyStr = "\x17"
	case tcell.KeyCtrlX:
		keyStr = "\x18"
	case tcell.KeyCtrlY:
		keyStr = "\x19"
	case tcell.KeyCtrlZ:
		keyStr = "\x1a"
	default:
		if ch != 0 {
			keyStr = string(ch)
		}
	}
	if keyStr == "" {
		return nil
	}
	return []byte(keyStr)
}

func getTerminalSize() (int, int) {
	cols, rows, err := term.GetSize(0)
	if err == nil && cols > 0 && rows > 0 {
		return cols, rows
	}

	cols, rows, err = term.GetSize(1)
	if err == nil && cols > 0 && rows > 0 {
		return cols, rows
	}

	cols, rows, err = term.GetSize(2)
	if err == nil && cols > 0 && rows > 0 {
		return cols, rows
	}

	return 120, 40
}
