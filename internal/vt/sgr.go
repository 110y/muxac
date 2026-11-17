package vt

import "strconv"

// selectGraphicRendition applies an SGR sequence to the pen.
func (t *Terminal) selectGraphicRendition() {
	p := &t.parser
	if p.n == 0 {
		t.pen = style{}
		return
	}
	for i := 0; i < p.n; i++ {
		if p.sub[i] {
			continue // a subparameter of an attribute that does not take one
		}
		// subs are the subparameters of the attribute, given after colons.
		end := i + 1
		for end < p.n && p.sub[end] {
			end++
		}
		subs := p.vals[i+1 : end]
		next := end

		switch v := p.vals[i]; {
		case v == 0:
			t.pen = style{}
		case v == 1:
			t.pen.attrs |= attrBold
		case v == 2:
			t.pen.attrs |= attrDim
		case v == 3:
			t.pen.attrs |= attrItalic
		case v == 4:
			t.pen.underline = underlineSingle
			// 4:0 to 4:5 select the style of the line, as kitty defined.
			if len(subs) > 0 && subs[0] >= 0 && subs[0] < len(underlineStyles) {
				t.pen.underline = underlineStyles[subs[0]]
			}
		case v == 5 || v == 6:
			t.pen.attrs |= attrBlink
		case v == 7:
			t.pen.attrs |= attrInverse
		case v == 8:
			t.pen.attrs |= attrInvisible
		case v == 9:
			t.pen.attrs |= attrStrike
		case v == 21:
			t.pen.underline = underlineDouble
		case v == 22:
			t.pen.attrs &^= attrBold | attrDim
		case v == 23:
			t.pen.attrs &^= attrItalic
		case v == 24:
			t.pen.underline = underlineNone
		case v == 25:
			t.pen.attrs &^= attrBlink
		case v == 27:
			t.pen.attrs &^= attrInverse
		case v == 28:
			t.pen.attrs &^= attrInvisible
		case v == 29:
			t.pen.attrs &^= attrStrike
		case v >= 30 && v <= 37:
			t.pen.fg = paletteColor(v - 30)
		case v == 38 || v == 48 || v == 58:
			var c color
			var ok bool
			if len(subs) > 0 {
				c, ok = colonColor(subs)
			} else {
				var used int
				c, ok, used = semicolonColor(p.vals[end:p.n], p.sub[end:p.n])
				next = end + used
			}
			if ok {
				switch v {
				case 38:
					t.pen.fg = c
				case 48:
					t.pen.bg = c
				default:
					t.pen.ul = c
				}
			}
		case v == 39:
			t.pen.fg = 0
		case v >= 40 && v <= 47:
			t.pen.bg = paletteColor(v - 40)
		case v == 49:
			t.pen.bg = 0
		case v == 53:
			t.pen.attrs |= attrOverline
		case v == 55:
			t.pen.attrs &^= attrOverline
		case v == 59:
			t.pen.ul = 0
		case v >= 90 && v <= 97:
			t.pen.fg = paletteColor(v - 90 + 8)
		case v >= 100 && v <= 107:
			t.pen.bg = paletteColor(v - 100 + 8)
		}
		i = next - 1
	}
}

// colonColor parses the subparameters of an extended color given in the
// colon form, such as 38:5:n, 38:2:r:g:b or 38:2::r:g:b (with a color space
// identifier).
func colonColor(subs []int) (color, bool) {
	switch subs[0] {
	case 5:
		if len(subs) >= 2 {
			return paletteColor(subs[1]), true
		}
	case 2:
		switch {
		case len(subs) >= 5:
			return rgbColor(subs[2], subs[3], subs[4]), true
		case len(subs) == 4:
			return rgbColor(subs[1], subs[2], subs[3]), true
		}
	}
	return 0, false
}

// semicolonColor parses an extended color given in the semicolon form, such
// as 38;5;n or 38;2;r;g;b, from the parameters that follow 38, 48 or 58. It
// returns how many parameters the color took.
func semicolonColor(vals []int, sub []bool) (color, bool, int) {
	if len(vals) == 0 || sub[0] {
		return 0, false, 0
	}
	switch vals[0] {
	case 5:
		if len(vals) >= 2 {
			return paletteColor(vals[1]), true, 2
		}
		return 0, false, len(vals)
	case 2:
		if len(vals) >= 4 {
			return rgbColor(vals[1], vals[2], vals[3]), true, 4
		}
		return 0, false, len(vals)
	}
	return 0, false, 1
}

// appendSGR appends the SGR sequence that selects s, from any previous style.
func appendSGR(b []byte, s style) []byte {
	b = append(b, "\x1b["...)
	b = appendSGRParams(b, s)
	return append(b, 'm')
}

// appendSGRParams appends the SGR parameters that select s, starting with a
// reset.
func appendSGRParams(b []byte, s style) []byte {
	b = append(b, '0')
	for _, a := range [...]struct {
		attr  attrs
		param string
	}{
		{attrBold, ";1"},
		{attrDim, ";2"},
		{attrItalic, ";3"},
		{attrBlink, ";5"},
		{attrInverse, ";7"},
		{attrInvisible, ";8"},
		{attrStrike, ";9"},
		{attrOverline, ";53"},
	} {
		if s.attrs&a.attr != 0 {
			b = append(b, a.param...)
		}
	}
	switch s.underline {
	case underlineSingle:
		b = append(b, ";4"...)
	case underlineDouble:
		b = append(b, ";4:2"...)
	case underlineCurly:
		b = append(b, ";4:3"...)
	case underlineDotted:
		b = append(b, ";4:4"...)
	case underlineDashed:
		b = append(b, ";4:5"...)
	case underlineNone:
	}
	b = appendColorParams(b, s.fg, 30, 90, "38")
	b = appendColorParams(b, s.bg, 40, 100, "48")
	switch s.ul.kind() {
	case colorPalette:
		b = append(b, ";58:5:"...)
		b = strconv.AppendUint(b, uint64(s.ul&0xff), 10)
	case colorRGB:
		b = append(b, ";58:2::"...)
		b = appendRGB(b, s.ul, ':')
	case colorDefault:
	}
	return b
}

// appendColorParams appends the parameters that select c as the foreground or
// background color: base and brightBase are the parameters of palette entries
// 0 and 8, extended the one of an extended color.
func appendColorParams(b []byte, c color, base, brightBase int, extended string) []byte {
	switch c.kind() {
	case colorPalette:
		i := int(c & 0xff)
		b = append(b, ';')
		switch {
		case i < 8:
			b = strconv.AppendInt(b, int64(base+i), 10)
		case i < 16:
			b = strconv.AppendInt(b, int64(brightBase+i-8), 10)
		default:
			b = append(b, extended...)
			b = append(b, ";5;"...)
			b = strconv.AppendInt(b, int64(i), 10)
		}
	case colorRGB:
		b = append(b, ';')
		b = append(b, extended...)
		b = append(b, ";2;"...)
		b = appendRGB(b, c, ';')
	case colorDefault:
	}
	return b
}

func appendRGB(b []byte, c color, sep byte) []byte {
	b = strconv.AppendUint(b, uint64(c>>16&0xff), 10)
	b = append(b, sep)
	b = strconv.AppendUint(b, uint64(c>>8&0xff), 10)
	b = append(b, sep)
	return strconv.AppendUint(b, uint64(c&0xff), 10)
}

// underlineStyles are the styles 4:0 to 4:5 select.
var underlineStyles = [...]underline{
	underlineNone,
	underlineSingle,
	underlineDouble,
	underlineCurly,
	underlineDotted,
	underlineDashed,
}
