package meshtastic

// Meshtastic serial framing:
//   | 0x94 | 0xc3 | len_hi | len_lo | payload (len bytes, protobuf) |
// Non-frame bytes between frames are debug text from firmware.

const (
	magic1 = 0x94
	magic2 = 0xc3
)

type parseState int

const (
	stWaitMagic1 parseState = iota
	stWaitMagic2
	stLenHi
	stLenLo
	stPayload
)

// frameParser is a byte-stream state machine that extracts complete
// Meshtastic frames from serial input. It is not safe for concurrent use.
type frameParser struct {
	st        parseState
	lenHi     byte
	length    int
	remaining int
	payload   []byte

	// onFrame is invoked with each complete framed payload.
	onFrame func(payload []byte)
	// onText, if set, is invoked for each line of debug text between frames.
	onText func(line string)

	textBuf []byte
}

func (p *frameParser) feed(data []byte) {
	for _, b := range data {
		p.step(b)
	}
}

func (p *frameParser) step(b byte) {
	switch p.st {
	case stWaitMagic1:
		if b == magic1 {
			p.st = stWaitMagic2
			return
		}
		p.pushText(b)
	case stWaitMagic2:
		if b == magic2 {
			p.st = stLenHi
			return
		}
		p.pushText(magic1)
		p.pushText(b)
		p.st = stWaitMagic1
	case stLenHi:
		p.lenHi = b
		p.st = stLenLo
	case stLenLo:
		p.length = (int(p.lenHi) << 8) | int(b)
		p.remaining = p.length
		p.payload = make([]byte, 0, p.length)
		if p.length == 0 {
			p.emit()
			p.st = stWaitMagic1
		} else {
			p.st = stPayload
		}
	case stPayload:
		p.payload = append(p.payload, b)
		p.remaining--
		if p.remaining == 0 {
			p.emit()
			p.st = stWaitMagic1
		}
	}
}

func (p *frameParser) pushText(b byte) {
	if b == '\n' {
		p.flushText()
		return
	}
	if b == '\r' {
		return
	}
	p.textBuf = append(p.textBuf, b)
	if len(p.textBuf) > 512 {
		p.flushText()
	}
}

func (p *frameParser) flushText() {
	if len(p.textBuf) == 0 {
		return
	}
	if p.onText != nil {
		p.onText(string(p.textBuf))
	}
	p.textBuf = p.textBuf[:0]
}

func (p *frameParser) emit() {
	if p.onFrame != nil {
		p.onFrame(p.payload)
	}
}
