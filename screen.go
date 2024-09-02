package terminal

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	screenStartOfLine = 0
	screenEndOfLine   = math.MaxInt
)

// A terminal 'screen'. Tracks cursor position, cursor style, content, size...
type Screen struct {
	// Current cursor position on the screen
	x, y int

	// Screen contents
	screen []screenLine

	// Current style
	style style

	// Current URL for OSC 8 (iTerm-style) hyperlinking
	urlBrush string

	// Parser to use for streaming processing
	parser parser

	// Optional maximum amount of backscroll to retain in the buffer.
	// Also sets an upper bound on window height.
	// Setting to 0 or negative makes the screen buffer unlimited.
	maxLines int

	// Optional upper bound on window width.
	// Setting to 0 or negative doesn't enforce a limit.
	maxColumns int

	// Current window size. This is required to properly bound cursor movement
	// commands. It defaults to 160 columns * 100 lines.
	// Note that window size does not bound content - maxLines controls the
	// number of screen lines held in the buffer, and each line can be
	// arbitrarily long (when written with plain text).
	cols, lines int

	// Optional callback. If not nil, as each line is scrolled out of the top of
	// the buffer, this func is called with the HTML.
	ScrollOutFunc func(lineHTML string)

	// Processing statistics
	LinesScrolledOut int // count of lines that scrolled off the top
	CursorUpOOB      int // count of times ESC [A or ESC [F tried to move y < 0
	CursorDownOOB    int // count of times ESC [B or ESC [G tried to move y >= height
	CursorFwdOOB     int // count of times ESC [C tried to move x >= width
	CursorBackOOB    int // count of times ESC [D tried to move x < 0
}

// ScreenOption is a functional option for creating new screens.
type ScreenOption = func(*Screen) error

// WithSize sets the initial window size.
func WithSize(w, h int) ScreenOption {
	return func(s *Screen) error { return s.SetSize(w, h) }
}

// WithMaxSize sets the screen size limits.
func WithMaxSize(maxCols, maxLines int) ScreenOption {
	return func(s *Screen) error {
		s.maxColumns, s.maxLines = maxCols, maxLines
		// Ensure the size fits within the new limits.
		if maxCols > 0 {
			s.cols = min(s.cols, maxCols)
		}
		if maxLines > 0 {
			s.lines = min(s.lines, maxLines)
		}
		return nil
	}
}

// NewScreen creates a new screen with various options.
func NewScreen(opts ...ScreenOption) (*Screen, error) {
	s := &Screen{
		// Arbitrarily chosen size, but 160 is double the traditional terminal
		// width (80) and 100 is 4x the traditional terminal height (25).
		// 160x100 also matches the buildkite-agent PTY size.
		cols:  160,
		lines: 100,
		parser: parser{
			mode: parserModeNormal,
		},
	}
	s.parser.screen = s
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetSize changes the window size.
func (s *Screen) SetSize(cols, lines int) error {
	if cols <= 0 || lines <= 0 {
		return fmt.Errorf("negative dimension in size %dw x %dh", cols, lines)
	}
	if s.maxColumns > 0 && cols > s.maxColumns {
		return fmt.Errorf("cols greater than max [%d > %d]", cols, s.maxColumns)
	}
	if s.maxLines > 0 && lines > s.maxLines {
		return fmt.Errorf("lines greater than max [%d > %d]", lines, s.maxLines)
	}
	s.cols, s.lines = cols, lines
	return nil
}

// ansiInt parses s as a decimal integer. If s is empty or malformed, it
// returns 1.
func ansiInt(s string) int {
	if s == "" {
		return 1
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 1
	}
	return i
}

// Move the cursor up, if we can
func (s *Screen) up(i string) {
	s.y -= ansiInt(i)
	if s.y < 0 {
		s.CursorUpOOB++
		s.y = 0
	}
	// If the cursor was on a line longer than the screen width, then pretend
	// the line wrapped at that width.
	// This is consistent with iTerm2 - try printing a long line of text,
	// then ESC [1B, then some more text... then resize the window and see what
	// happens.
	s.x = s.x % s.cols
}

// Move the cursor down, if we can
func (s *Screen) down(i string) {
	s.y += ansiInt(i)
	if s.y >= s.lines {
		s.CursorDownOOB++
		s.y = s.lines - 1
	}
	s.x = s.x % s.cols // see coment in the up method.
}

// Move the cursor forward (right) on the line, if we can
func (s *Screen) forward(i string) {
	s.x += ansiInt(i)
	if s.x >= s.cols {
		s.CursorFwdOOB++
		s.x = s.cols - 1
	}
}

// Move the cursor backward (left), if we can
func (s *Screen) backward(i string) {
	s.x -= ansiInt(i)
	if s.x < 0 {
		s.CursorBackOOB++
		s.x = 0
	}
}

// top returns the index within s.screen where the window begins.
// The top of the window is not necessarily the top of the buffer: in fact,
// the window is always the bottom-most s.lines (or fewer) elements of s.screen.
// top + s.y = the index of the line where the cursor is.
func (s *Screen) top() int {
	return max(0, len(s.screen)-s.lines)
}

// currentLine returns the line the cursor is on, or nil if no such line has
// been added to the screen buffer yet.
func (s *Screen) currentLine() *screenLine {
	yidx := s.top() + s.y
	if yidx < 0 || yidx >= len(s.screen) {
		return nil
	}
	return &s.screen[yidx]
}

// currentLineForWriting returns the line the cursor is on, or if there is no
// line allocated in the buffer yet, allocates a new line and ensures it has
// enough nodes to write something at the cursor position.
func (s *Screen) currentLineForWriting() *screenLine {
	// Ensure there are enough lines on screen to start writing here.
	for s.currentLine() == nil {
		// If maxLines is not in use, or adding a new line would not make it
		// larger than maxLines, then just allocate a new line.
		if s.maxLines <= 0 || len(s.screen)+1 <= s.maxLines {
			newLine := screenLine{nodes: make([]node, 0, s.cols)}
			s.screen = append(s.screen, newLine)
			if s.y >= s.lines {
				// Because the "window" is always the last s.lines of s.screen
				// (or all of them, if there are fewer lines than s.lines)
				// appending a new line shifts the window down. In that case,
				// compensate by shifting s.y up (eventually to within bounds).
				s.y--
			}
			continue
		}

		// maxLines is in effect, and adding a new line would make the screen
		// larger than maxLines.
		// Pass the line being scrolled out to scrollOutFunc, if not nil.
		if s.ScrollOutFunc != nil {
			s.ScrollOutFunc(s.screen[0].asHTML())
		}
		s.LinesScrolledOut++

		// Trim the first line off the top of the screen.
		// Recycle its nodes slice to make a new line on the bottom.
		newLine := screenLine{nodes: s.screen[0].nodes[:0]}
		s.screen = append(s.screen[1:], newLine)

		// Since the buffer scrolled down, leaving len(s.screen) unchanged,
		// s.y moves upwards.
		s.y--
	}

	line := s.currentLine()

	// Add columns if currently shorter than the cursor's x position
	for i := len(line.nodes); i <= s.x; i++ {
		line.nodes = append(line.nodes, emptyNode)
	}
	return line
}

// Write a character to the screen's current X&Y, along with the current screen style
func (s *Screen) write(data rune) {
	line := s.currentLineForWriting()
	line.nodes[s.x] = node{blob: data, style: s.style}

	// OSC 8 links work like a style.
	if s.style.hyperlink() {
		if line.hyperlinks == nil {
			line.hyperlinks = make(map[int]string)
		}
		line.hyperlinks[s.x] = s.urlBrush
	}
}

// Append a character to the screen
func (s *Screen) append(data rune) {
	s.write(data)
	s.x++
}

// Append multiple characters to the screen
func (s *Screen) appendMany(data []rune) {
	for _, char := range data {
		s.append(char)
	}
}

func (s *Screen) appendElement(i *element) {
	line := s.currentLineForWriting()
	idx := len(line.elements)
	line.elements = append(line.elements, i)
	ns := s.style
	ns.setElement(true)
	line.nodes[s.x] = node{blob: rune(idx), style: ns}
	s.x++
}

// Set line metadata. Merges the provided data into any existing
// metadata for the current line, overwriting data when keys collide.
func (s *Screen) setLineMetadata(namespace string, data map[string]string) {
	line := s.currentLineForWriting()
	if line.metadata == nil {
		line.metadata = map[string]map[string]string{
			namespace: data,
		}
		return
	}

	ns := line.metadata[namespace]
	if ns == nil {
		// namespace did not exist, set all data
		line.metadata[namespace] = data
		return
	}

	// copy new data over old data
	for k, v := range data {
		ns[k] = v
	}
}

// Apply color instruction codes to the screen's current style
func (s *Screen) color(i []string) {
	s.style = s.style.color(i)
}

// Apply an escape sequence to the screen
func (s *Screen) applyEscape(code rune, instructions []string) {
	// Wrap slice accesses in a bounds check. Instructions not supplied default
	// to the empty string.
	inst := func(i int) string {
		if i < 0 || i >= len(instructions) {
			return ""
		}
		return instructions[i]
	}

	if strings.HasPrefix(inst(0), "?") {
		// These are typically "private" control sequences, e.g.
		// - show/hide cursor (not relevant)
		// - enable/disable focus reporting (not relevant)
		// - alternate screen buffer (not implemented)
		// - bracketed paste mode (not relevant)
		// Particularly, "show cursor" is CSI ?25h, which would be picked up
		// below if we didn't handle it.
		return
	}

	switch code {
	case 'A': // Cursor Up: go up n
		s.up(inst(0))

	case 'B': // Cursor Down: go down n
		s.down(inst(0))

	case 'C': // Cursor Forward: go right n
		s.forward(inst(0))

	case 'D': // Cursor Back: go left n
		s.backward(inst(0))

	case 'E': // Cursor Next Line: Go to beginning of line n down
		s.x = 0
		s.down(inst(0))

	case 'F': // Cursor Previous Line: Go to beginning of line n up
		s.x = 0
		s.up(inst(0))

	case 'G': // Cursor Horizontal Absolute: Go to column n (default 1)
		s.x = ansiInt(inst(0)) - 1
		s.x = max(s.x, 0)
		s.x = min(s.x, s.cols-1)

	case 'H': // Cursor Position Absolute: Go to row n and column m (default 1;1).
		s.y = ansiInt(inst(0)) - 1
		s.y = max(s.y, 0)
		s.y = min(s.y, s.lines-1)
		s.x = ansiInt(inst(1)) - 1
		s.x = max(s.x, 0)
		s.x = min(s.x, s.cols-1)

	case 'J': // Erase in Display: Clears part of the screen.
		switch inst(0) {
		case "0", "": // "erase from current position to end (inclusive)"
			s.currentLine().clear(s.x, screenEndOfLine) // same as ESC [0K

			// The window is at the bottom of the screen buffer, so we can clear
			// the rest of the screen using truncation.
			yidx := s.top() + s.y
			if yidx < 0 || yidx >= len(s.screen) {
				return
			}
			s.screen = s.screen[:yidx+1]

		case "1": // "erase from beginning to current position (inclusive)"
			s.currentLine().clear(screenStartOfLine, s.x) // same as ESC [1K

			// real terms erase part of the window, but the cursor stays still.
			// The intervening lines simply become blank.
			top := s.top()
			end := min(top+s.y, len(s.screen))
			for i := top; i < end; i++ {
				s.screen[i].clearAll()
			}

		case "2":
			// 2: "erase entire display"
			// Previous implementations performed this the same as ESC [3J,
			// which also removes all "scroll-back".
			s.screen = s.screen[:s.top()]
			// Note: on real terminals this doesn't reset the cursor position.
			s.x, s.y = 0, 0

		case "3":
			// 3: "erase whole display including scroll-back buffer"
			s.screen = s.screen[:0]
			// Note: on real terminals this doesn't reset the cursor position.
			s.x, s.y = 0, 0
		}

	case 'K': // Erase in Line: erases part of the line.
		switch inst(0) {
		case "0", "":
			s.currentLine().clear(s.x, screenEndOfLine)

		case "1":
			s.currentLine().clear(screenStartOfLine, s.x)

		case "2":
			s.currentLine().clearAll()
		}

	case 'M':
		s.color(instructions)
	}
}

// Write writes ANSI text to the screen.
func (s *Screen) Write(input []byte) (int, error) {
	s.parser.parseToScreen(input)
	return len(input), nil
}

// AsHTML returns the contents of the current screen buffer as HTML.
func (s *Screen) AsHTML() string {
	lines := make([]string, 0, len(s.screen))

	for _, line := range s.screen {
		lines = append(lines, line.asHTML())
	}

	return strings.Join(lines, "\n")
}

// AsPlainText renders the screen without any ANSI style etc.
func (s *Screen) AsPlainText() string {
	lines := make([]string, 0, len(s.screen))

	for _, line := range s.screen {
		lines = append(lines, line.asPlain())
	}

	return strings.Join(lines, "\n")
}

func (s *Screen) newLine() {
	s.x = 0
	s.y++
}

func (s *Screen) revNewLine() {
	if s.y > 0 {
		s.y--
	}
}

func (s *Screen) carriageReturn() {
	s.x = 0
}

func (s *Screen) backspace() {
	if s.x > 0 {
		s.x--
	}
}

type screenLine struct {
	nodes []node

	// metadata is { namespace => { key => value, ... }, ... }
	// e.g. { "bk" => { "t" => "1234" } }
	metadata map[string]map[string]string

	// element nodes refer to elements in this slice by index
	// (if node.style.element(), then elements[node.blob] is the element)
	elements []*element

	// hyperlinks stores the URL targets for OSC 8 (iTerm-style) links
	// by X position. URLs are too big to fit in every node, most lines won't
	// have links and most nodes in a line won't be linked.
	// So a map is used for sparse storage, only lazily created when text with
	// a link style is written.
	hyperlinks map[int]string
}

func (l *screenLine) clearAll() {
	if l == nil {
		return
	}
	l.nodes = l.nodes[:0]
}

// clear clears part (or all) of a line. The range to clear is inclusive
// of xStart and xEnd.
func (l *screenLine) clear(xStart, xEnd int) {
	if l == nil {
		return
	}

	if xStart < 0 {
		xStart = 0
	}
	if xEnd < xStart {
		// Not a valid range.
		return
	}

	if xStart >= len(l.nodes) {
		// Clearing part of a line starting after the end of the current line...
		return
	}

	if xEnd >= len(l.nodes)-1 {
		// Clear from start to end of the line
		l.nodes = l.nodes[:xStart]
		return
	}

	for i := xStart; i <= xEnd; i++ {
		l.nodes[i] = emptyNode
	}
}
