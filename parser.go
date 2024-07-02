package terminal

import (
	"unicode"
	"unicode/utf8"
)

const (
	parserModeNormal = iota
	parserModeEscape
	parserModeControl
	parserModeOSC
	parserModeOSCEsc // within OSC and just read an escape
	parserModeCharset
	parserModeAPC
	parserModeAPCEsc // within APC and just read an escape
)

type position struct {
	x, y int
}

// Stateful ANSI parser
type parser struct {
	screen               *Screen
	buffer               []byte
	mode                 int
	cursor               int
	escapeStartedAt      int
	instructions         []string
	instructionStartedAt int
	savePosition         position

	// Buildkite-specific state
	lastTimestamp int64
}

/*
 * How this state machine works:
 *
 * We start in MODE_NORMAL. We're not inside an escape sequence. In this mode
 * most input is written directly to the screen. If we receive a newline,
 * backspace or other cursor-moving signal, we let the screen know so that it
 * can change the location of its cursor accordingly.
 *
 * If we're in MODE_NORMAL and we receive an escape character (\x1b) we enter
 * MODE_ESCAPE. The following character could start an escape sequence, a
 * control sequence, an operating system command, or be invalid or not understood.
 *
 * If we're in MODE_ESCAPE we look for ~~three~~ eight possible characters:
 *
 * 1. For `[` we enter MODE_CONTROL and start looking for a control sequence.
 * 2. For `]` we enter MODE_OSC and look for an operating system command.
 * 3. For `(` or ')' we enter MODE_CHARSET and look for a character set name.
 * 4. For `_` we enter MODE_APC and parse the rest of the custom control sequence
 * 5. For `M`, `7`, or `8`, we run an instruction directly (reverse newline,
 *    or save/restore cursor).
 *
 * In all cases we start our instruction buffer. The instruction buffer is used
 * to store the individual characters that make up ANSI instructions before
 * sending them to the screen. If we receive neither of these characters, we
 * treat this as an invalid or unknown escape and return to MODE_NORMAL.
 *
 * If we're in MODE_CONTROL, we expect to receive a sequence of parameters and
 * then a terminal alphabetic character looking like 1;30;42m. That's an
 * instruction to turn on bold, set the foreground colour to black and the
 * background colour to green. We receive these characters one by one turning
 * the parameters into instruction parts (1, 30, 42) followed by an instruction
 * type (m). Once the instruction type is received we send it and its parts to
 * the screen and return to MODE_NORMAL.
 *
 * If we're in MODE_OSC, we expect to receive a sequence of characters up to
 * and including a bell (\a). We skip forward until this bell is reached, then
 * send everything from when we entered MODE_OSC up to the bell to
 * parseElementSequence and return to MODE_NORMAL.
 *
 * If we're in MODE_CHARSET we simply discard the next character which would
 * normally designate the character set.
 */

func (p *parser) parseToScreen(input []byte) {
	if len(p.buffer) == 0 {
		p.buffer = input
	} else {
		p.buffer = append(p.buffer, input...)
	}

	for p.cursor < len(p.buffer) {
		char, charLen := utf8.DecodeRune(p.buffer[p.cursor:])

		switch p.mode {
		case parserModeEscape:
			// We've received an escape character but aren't inside an escape sequence yet
			p.handleEscape(char)

		case parserModeControl:
			// We're inside a control sequence - figure out its code and its instructions.
			p.handleControlSequence(char)

		case parserModeOSC:
			// We're inside an operating system command, capture until we hit BEL or ESC \ (ST)
			p.handleOperatingSystemCommand(char)

		case parserModeOSCEsc:
			// We're inside an operating system command, and just hit an ESC (might be ST)
			p.handleOSCEscape(char)

		case parserModeCharset:
			// We're inside a charset sequence, capture the next character.
			p.handleCharset(char)

		case parserModeAPC:
			// We're inside a custom escape sequence, capture until we hit BEL or ESC \ (ST)
			p.handleApplicationProgramCommand(char)

		case parserModeAPCEsc:
			// We're inside an APC, and just hit an ESC (which might be ST)
			p.handleAPCEscape(char)

		case parserModeNormal:
			// Outside of an escape sequence entirely, normal input
			p.handleNormal(char)
		}

		p.cursor += charLen
	}

	// If we're in normal mode, everything up to the cursor has been procesed.
	// If we're in the middle of an escape, everything up to p.escapeStartedAt
	// has been processed.
	done := p.escapeStartedAt
	if p.mode == parserModeNormal {
		done = p.cursor
	}

	// Drop the completed portion of the buffer.
	p.buffer = p.buffer[done:]
	p.cursor -= done
	p.instructionStartedAt -= done
	p.escapeStartedAt -= done
}

// handleCharset is called for each character consumed while in MODE_CHARSET.
// It ignores the character and transitions back to MODE_NORMAL.
func (p *parser) handleCharset(rune) {
	p.mode = parserModeNormal
}

// handleOSCEscape is called for the character after an ESC when reading an OSC.
// It either returns to OSC mode, or terminates the OSC and processes it.
func (p *parser) handleOSCEscape(char rune) {
	switch char {
	case '\\': // ESC + \ = string terminator
		// Don't include the ESC in the OSC contents.
		p.processOperatingSystemCommand(p.cursor - 1)

	default:
		// ESC + anything else = not a string terminator.
		// OSC continues...
		p.mode = parserModeOSC
	}
}

// handleOperatingSystemCommand is called for each character consumed while in
// MODE_OSC. It does nothing until the OSC is terminated with either BEL or
// ESC \ (ST).
func (p *parser) handleOperatingSystemCommand(char rune) {
	switch char {
	case '\x07': // BEL terminates the APC
		p.processOperatingSystemCommand(p.cursor)

	case '\x1b': // ESC
		// Next char _could_ be \ which makes the combination a string terminator
		p.mode = parserModeOSCEsc

	default:
		// OSC continues...
	}
}

// processOperatingSystemCommand processes the contents of the OSC that was just read.
func (p *parser) processOperatingSystemCommand(end int) {
	p.mode = parserModeNormal
	image, err := parseElementSequence(string(p.buffer[p.instructionStartedAt:end]))

	if image == nil && err == nil {
		// No image & no error, nothing to render
		return
	}

	ownLine := image == nil || image.elementType != ELEMENT_LINK

	if ownLine {
		// Images (or the error encountered) should appear on their own line
		if p.screen.x != 0 {
			p.screen.newLine()
		}
		p.screen.clear(p.screen.y, screenStartOfLine, screenEndOfLine)
	}

	if err != nil {
		p.screen.appendMany([]rune("*** Error parsing custom element escape sequence: "))
		p.screen.appendMany([]rune(err.Error()))
	} else {
		p.screen.appendElement(image)
	}

	if ownLine {
		p.screen.newLine()
	}
}

// handleAPCEscape is called for the character after an ESC when reading an APC.
// It either returns to APC mode, or terminates the APC and processes it.
func (p *parser) handleAPCEscape(char rune) {
	switch char {
	case '\\': // ESC + \ = string terminator
		// Don't include the ESC in the APC contents.
		p.processApplicationProgramCommand(p.cursor - 1)

	default:
		// ESC + anything else = not a string terminator.
		// APC continues...
		p.mode = parserModeAPC
	}
}

// handleApplicationProgramCommand is called for each character consumed while
// in MODE_APC, but does nothing until the APC is terminated with BEL (0x07)
// or the two-byte form of ST (ESC \).
//
// Technically an APC sequence is terminated by String Terminator (ST; 0x9C or ESC \):
// https://en.wikipedia.org/wiki/C0_and_C1_control_codes#C1_controls
//
// But:
// > For historical reasons, Xterm can end the command with BEL as well as the standard ST
// https://en.wikipedia.org/wiki/ANSI_escape_code#OSC_(Operating_System_Command)_sequences
//
// .. and this is how iTerm2 implements inline images:
// > ESC ] 1337 ; key = value ^G
// https://iterm2.com/documentation-images.html
//
// Buildkite's ansi timestamper does the same, and we don't _expect_ to be
// seeing any other APCs that could be ST-terminated. But we've seen ESC \
// in some bug reports.
func (p *parser) handleApplicationProgramCommand(char rune) {
	switch char {
	case '\x07': // BEL terminates the APC
		p.processApplicationProgramCommand(p.cursor)

	case '\x1b': // ESC
		// Next char _could_ be \ which makes the combination ST
		p.mode = parserModeAPCEsc

	default:
		// APC continues...
	}
}

// processApplicationProgramCommand process the contents of the APC that was just read.
func (p *parser) processApplicationProgramCommand(end int) {
	p.mode = parserModeNormal
	sequence := string(p.buffer[p.instructionStartedAt:end])

	// this might be a Buildkite Application Program Command sequence...
	data, err := p.parseBuildkiteAPC(sequence)
	if err != nil {
		p.screen.appendMany([]rune("*** Error parsing Buildkite APC ANSI escape sequence: "))
		p.screen.appendMany([]rune(err.Error()))
		return
	}

	if data == nil {
		return
	}
	p.screen.setLineMetadata(bkNamespace, data)
}

// handleControlSequence is called for each character consumed while in
// MODE_CONTROL.
func (p *parser) handleControlSequence(char rune) {
	char = unicode.ToUpper(char)
	switch char {
	case '?', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		// Part of an instruction
	case ';':
		p.addInstruction()
		p.instructionStartedAt = p.cursor + utf8.RuneLen(';')
	case 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'J', 'K', 'M', 'Q':
		p.addInstruction()
		p.screen.applyEscape(char, p.instructions)
		p.mode = parserModeNormal
	case 'H', 'L':
		// Set/reset mode (SM/RM), ignore and continue
		p.mode = parserModeNormal
	default:
		// unrecognized character, abort the escapeCode
		p.cursor = p.escapeStartedAt
		p.mode = parserModeNormal
	}
}

// handleNormal is called for each character consumed while in MODE_NORMAL.
func (p *parser) handleNormal(char rune) {
	switch char {
	case '\n':
		p.screen.newLine()
	case '\r':
		p.screen.carriageReturn()
	case '\b':
		p.screen.backspace()
	case '\x1b':
		p.escapeStartedAt = p.cursor
		p.mode = parserModeEscape
	default:
		p.screen.append(char)
	}
}

// handleEscape is called for each character consumed while in MODE_ESCAPE.
func (p *parser) handleEscape(char rune) {
	switch char {
	case '[':
		p.instructionStartedAt = p.cursor + utf8.RuneLen('[')
		p.instructions = make([]string, 0, 1)
		p.mode = parserModeControl
	case ']':
		p.instructionStartedAt = p.cursor + utf8.RuneLen('[')
		p.mode = parserModeOSC
	case ')', '(':
		p.instructionStartedAt = p.cursor + utf8.RuneLen('(')
		p.mode = parserModeCharset
	case '_':
		p.instructionStartedAt = p.cursor + utf8.RuneLen('[')
		p.mode = parserModeAPC
	case 'M':
		p.screen.revNewLine()
		p.mode = parserModeNormal
	case '7':
		p.savePosition = position{x: p.screen.x, y: p.screen.y}
		p.mode = parserModeNormal
	case '8':
		p.screen.x = p.savePosition.x
		p.screen.y = p.savePosition.y
		p.mode = parserModeNormal
	default:
		// Not an escape code, false alarm
		p.cursor = p.escapeStartedAt
		p.mode = parserModeNormal
	}
}

// addInstruction appends an instruction to p.instructions, if the current
// instruction is nonempty.
func (p *parser) addInstruction() {
	instruction := string(p.buffer[p.instructionStartedAt:p.cursor])
	if instruction != "" {
		p.instructions = append(p.instructions, instruction)
	}
}
