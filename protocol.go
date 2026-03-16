package main

import (
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// WireCell is a serializable version of Cell (tcell.Style has unexported fields).
type WireCell struct {
	Ch    rune
	Fg    int64 // tcell.Color as int64
	Bg    int64
	Attrs int64 // tcell.AttrMask
}

func cellToWire(c Cell) WireCell {
	fg, bg, attrs := c.Style.Decompose()
	return WireCell{Ch: c.Ch, Fg: int64(fg), Bg: int64(bg), Attrs: int64(attrs)}
}

func wireToCell(w WireCell) Cell {
	st := tcell.StyleDefault.Foreground(tcell.Color(w.Fg)).Background(tcell.Color(w.Bg)).Attributes(tcell.AttrMask(w.Attrs))
	return Cell{Ch: w.Ch, Style: st}
}

// PaneSnapshot is the per-pane data sent from server to client each frame.
type PaneSnapshot struct {
	Cells         [][]WireCell
	Title         string
	VirtualX      int
	Width         int
	Cols          int
	Rows          int
	IsActive      bool
	CursorRow     int
	CursorCol     int
	CursorVisible bool
	ScrollOff     int
	AppCursorKeys bool // needed by client for key translation
}

// WorkspaceInfo is sidebar metadata.
type WorkspaceInfo struct {
	Name       string
	PaneCount  int
	IsActive   bool
	ColorName  string
	ColorValue int64
}

// ServerMsg is a message from server to client.
type ServerMsg struct {
	Type           string // "snapshot", "quit"
	Panes          []PaneSnapshot
	Workspaces     []WorkspaceInfo
	ActiveWSIdx    int
	PanePresetIdx  int
	PanePresetName string
}

// ClientMsg is a message from client to server.
type ClientMsg struct {
	Type string // "input", "resize", "command"

	// For "input": raw bytes to write to active PTY
	InputData []byte

	// For "resize": new viewport dimensions
	ViewportWidth  int
	ViewportHeight int

	// For "command": command name
	Command string // "new-pane", "close-pane", "new-workspace",
	// "next-pane", "prev-pane", "next-workspace", "prev-workspace",
	// "widen-pane", "narrow-pane", "scroll-up", "scroll-down",
	// "page-up", "page-down", "focus-pane", "quit", "detach"

	// For "focus-pane": which pane index to focus
	PaneIndex int

	// For "focus-workspace": which workspace index to focus
	WorkspaceIndex int

	// For command payloads
	WorkspaceName string
	ColorName     string
	PanePresetIdx int
}

// sendMsg sends a length-prefixed gob-encoded message.
func sendMsg(conn net.Conn, msg interface{}) error {
	buf := make([]byte, 0, 65536)
	var tmp strings.Builder
	enc := gob.NewEncoder(&tmp)
	if err := enc.Encode(msg); err != nil {
		return err
	}
	data := []byte(tmp.String())
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	_, err := conn.Write(buf)
	return err
}

// recvMsg reads a length-prefixed gob-encoded message.
func recvMsg(conn net.Conn, msg interface{}) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return err
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen > 16*1024*1024 {
		return fmt.Errorf("message too large: %d bytes", msgLen)
	}
	data := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return err
	}
	dec := gob.NewDecoder(strings.NewReader(string(data)))
	return dec.Decode(msg)
}
