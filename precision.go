package subtrim

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"subtrim/native"
)

var precisionScalar = regexp.MustCompile(`^\\(xbord|ybord|xshad|yshad|bord|shad|blur|fsp|fs|fscx|fscy|frx|fry|frz|fr|fax|fay)(` + signed + `)$`)
var precisionCoordinates = regexp.MustCompile(`^\\(pos|org|move|clip|iclip)\(([^()]*)\)$`)
var precisionTransform = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?,){0,3}$`)
var precisionAnimated = regexp.MustCompile(`\\[ \t]*(?:move|t|fad|k|K)`)
var precisionDrawing = regexp.MustCompile(`^\\p([0-9]{1,2})$`)
var precisionClip = regexp.MustCompile(`^(\\i?clip\()(?:(\d+),)?(.*)\)$`)

// round half to even using decimal digits, without a binary floating-point detour.
func rounded(value string, places int) string {
	if places < 0 {
		return value
	}
	stripped := strings.Trim(value, " \t")
	if len(stripped) > 64 || !numberPattern.MatchString(stripped) {
		return value
	}
	negative := strings.HasPrefix(stripped, "-")
	unsigned := strings.TrimPrefix(stripped, "-")
	whole, fraction, _ := strings.Cut(unsigned, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if len(whole) > 7 || len(whole) == 7 && (whole > "1000000" || whole == "1000000" && strings.Trim(fraction, "0") != "") {
		return value
	}
	if len(fraction) > places {
		digits := []byte(whole + fraction[:places])
		next := fraction[places]
		up := next > '5' || next == '5' && (strings.Trim(fraction[places+1:], "0") != "" || (digits[len(digits)-1]-'0')%2 == 1)
		if up {
			i := len(digits) - 1
			for i >= 0 && digits[i] == '9' {
				digits[i] = '0'
				i--
			}
			if i < 0 {
				digits = append([]byte{'1'}, digits...)
			} else {
				digits[i]++
			}
		}
		whole = string(digits[:len(digits)-places])
		fraction = string(digits[len(digits)-places:])
	}
	fraction = strings.TrimRight(fraction, "0")
	replacement := whole
	if fraction != "" {
		replacement += "." + fraction
	}
	if negative && replacement != "0" {
		replacement = "-" + replacement
	}
	prefix := value[:len(value)-len(strings.TrimLeft(value, " \t"))]
	suffix := value[len(strings.TrimRight(value, " \t")):]
	replacement = prefix + replacement + suffix
	if len(replacement) < len(value) {
		return replacement
	}
	return value
}

func places(step float64, script, frame int) int {
	if step == 0 {
		return -1 // original coordinates: simplify paths without quantization
	}
	return min(12, max(0, int(math.Ceil(-(math.Log10(step)+math.Log10(float64(script))-math.Log10(float64(frame)))-1e-12))))
}

func (c *cleaner) quantizeToken(raw string, xp, yp, depth int, analysis *precisionAnalysis) string {
	if xp < 0 && yp < 0 {
		return raw
	}
	if depth == 0 && strings.HasPrefix(raw, `\t(`) && strings.HasSuffix(raw, ")") {
		inner := raw[3 : len(raw)-1]
		offset := strings.IndexByte(inner, '\\')
		if offset >= 0 && precisionTransform.MatchString(inner[:offset]) {
			tokens, cached := analysis.transforms[raw]
			prefix, ok := "", true
			if !cached {
				prefix, tokens, ok = c.splitTokens(inner[offset:])
			}
			nested := false
			for _, t := range tokens {
				if strings.HasPrefix(t.raw, `\t`) {
					nested = true
				}
			}
			if ok && prefix == "" && !nested {
				if !cached && analysis.bytes+len(raw)*20 < 16<<20 {
					if analysis.transforms == nil {
						analysis.transforms = map[string][]token{}
					}
					analysis.transforms[raw] = tokens
					analysis.bytes += len(raw) * 20
				}
				var out strings.Builder
				out.WriteString(`\t(` + inner[:offset])
				for _, t := range tokens {
					out.WriteString(c.quantizeToken(t.raw, xp, yp, 1, analysis))
				}
				out.WriteByte(')')
				return out.String()
			}
		}
	}
	if m := precisionScalar.FindStringSubmatch(raw); m != nil {
		tag, value := m[1], m[2]
		v, _ := strconv.ParseFloat(value, 64)
		if tag == "fs" && (strings.HasPrefix(value, "-") || v <= 0) {
			return raw
		}
		if (tag == "bord" || tag == "xbord" || tag == "ybord" || tag == "blur") && strings.HasPrefix(value, "-") {
			return raw
		}
		p := max(xp, yp)
		switch tag {
		case "fsp", "xbord", "xshad":
			p = xp
		case "fs", "ybord", "yshad":
			p = yp
		case "fscx", "fscy", "frx", "fry", "frz", "fr", "fax", "fay":
			p = max(2, xp, yp)
		}
		return `\` + tag + rounded(value, p)
	}
	if m := precisionCoordinates.FindStringSubmatch(raw); m != nil {
		tag, values := m[1], strings.Split(m[2], ",")
		count := 4
		if tag == "pos" || tag == "org" {
			count = 2
		}
		if len(values) != count && !(tag == "move" && len(values) == 6) {
			return raw
		}
		for _, v := range values {
			if !numberPattern.MatchString(strings.Trim(v, " \t")) {
				return raw
			}
		}
		for i := 0; i < count; i++ {
			p := xp
			if i%2 == 1 {
				p = yp
			}
			values[i] = rounded(values[i], p)
		}
		return `\` + tag + "(" + strings.Join(values, ",") + ")"
	}
	return raw
}

type drawingGroup struct {
	command byte
	values  []string
}
type drawingPoint [2]*big.Int

func point(values []string, places [2]int) drawingPoint {
	var p drawingPoint
	for axis, value := range values[len(values)-2:] {
		whole, fraction, _ := strings.Cut(value, ".")
		p[axis], _ = new(big.Int).SetString(whole+fraction+strings.Repeat("0", places[axis]-len(fraction)), 10)
	}
	return p
}

func between(a, p, b drawingPoint) bool {
	for i := 0; i < 2; i++ {
		low, high := a[i], b[i]
		if low.Cmp(high) > 0 {
			low, high = high, low
		}
		if p[i].Cmp(low) < 0 || p[i].Cmp(high) > 0 {
			return false
		}
	}
	// Common positive scales on the two axes preserve ordering and multiply
	// both determinant terms by the same factor: no rational reduction needed.
	var left, right, delta big.Int
	left.Sub(p[0], a[0])
	delta.Sub(b[1], a[1])
	left.Mul(&left, &delta)
	right.Sub(p[1], a[1])
	delta.Sub(b[0], a[0])
	right.Mul(&right, &delta)
	return left.Cmp(&right) == 0
}

// Analysis is immutable and bounded to one optimization, across precision steps.
type precisionAnalysis struct {
	transforms map[string][]token
	pieces     map[string][]piece
	drawings   map[string]*drawingAnalysis
	bytes      int
}

type drawingAnalysis struct {
	groups    []drawingGroup
	places    [2]int
	canonical [2]bool
	results   map[[2]int]string
}

func (a *precisionAnalysis) rewriteDrawing(path string, xp, yp int) (result string) {
	if a.drawings == nil {
		a.drawings = map[string]*drawingAnalysis{}
	}
	drawing, cached := a.drawings[path]
	if !cached {
		drawing = &drawingAnalysis{canonical: [2]bool{true, true}}
		parts := drawingParts(path)
		if parts == nil {
			if cost := len(path) + 64; a.bytes+cost < 16<<20 {
				a.drawings[path] = drawing
				a.bytes += cost
			}
			return path
		}
		for _, value := range parts {
			if len(value) == 1 && strings.ContainsAny(value, "mnlb") {
				drawing.groups = append(drawing.groups, drawingGroup{command: value[0]})
			} else {
				i := len(drawing.groups) - 1
				axis := len(drawing.groups[i].values) % 2
				_, fraction, _ := strings.Cut(value, ".")
				drawing.places[axis] = max(drawing.places[axis], len(fraction))
				drawing.canonical[axis] = drawing.canonical[axis] && number(value) == value
				drawing.groups[i].values = append(drawing.groups[i].values, value)
			}
		}
		if a.bytes+len(path)*20 < 16<<20 {
			a.drawings[path] = drawing
			a.bytes += len(path) * 20
			cached = true
		}
	}
	if len(drawing.groups) == 0 {
		return path
	}
	// Extra decimal places cannot change these coordinates. Exact mode shares
	// that result only when the original spelling is already canonical.
	key := [2]int{xp, yp}
	for axis, p := range key {
		if p >= drawing.places[axis] || p < 0 && drawing.canonical[axis] {
			key[axis] = drawing.places[axis]
		}
	}
	if result, ok := drawing.results[key]; ok {
		return result
	}
	defer func() {
		cost := len(result) + 64
		if cached && a.bytes+cost < 16<<20 {
			if drawing.results == nil {
				drawing.results = map[[2]int]string{}
			}
			drawing.results[key] = result
			a.bytes += cost
		}
	}()
	var commands []drawingGroup
	var current, previous drawingPoint
	for _, group := range drawing.groups {
		group.values = append([]string(nil), group.values...)
		for i, v := range group.values {
			p := xp
			if i%2 == 1 {
				p = yp
			}
			group.values[i] = rounded(v, p)
		}
		stride := 2
		if group.command == 'b' {
			stride = 6
		}
		for i := 0; i < len(group.values); i += stride {
			chunk, kind := group.values[i:i+stride], group.command
			last := point(chunk, drawing.places)
			if kind == 'b' && current[0] != nil {
				a, b := point(chunk[:2], drawing.places), point(chunk[2:4], drawing.places)
				if between(current, a, b) && between(a, b, last) {
					kind = 'l'
					chunk = chunk[4:]
				}
			}
			if kind == 'l' && len(commands) > 1 && commands[len(commands)-1].command == 'l' && between(previous, current, last) {
				commands = commands[:len(commands)-1]
			} else {
				previous = current
			}
			commands = append(commands, drawingGroup{kind, chunk})
			current = last
		}
	}
	var out []string
	last := byte(0)
	for _, command := range commands {
		if command.command != last || command.command == 'm' || command.command == 'n' {
			out = append(out, string(command.command))
		}
		out = append(out, command.values...)
		last = command.command
	}
	new := strings.Join(out, " ")
	if len(new) < len(path) {
		return new
	}
	return path
}

// quantizeEdits changes Dialogue Text only. animated/effect events require exhaustive validation.
func quantizeEdits(doc document, spans []dialogueSpan, res [2]int, config native.Config, step float64, animated bool, analysis *precisionAnalysis, progress func() bool) []edit {
	xp, yp := places(step, res[0], config.Width), places(step, res[1], config.Height)
	c := cleaner{tokens: map[string]token{}, transparentTags: map[string]bool{}, transforms: map[string]state{}}
	var edits []edit
	for i, span := range spans {
		if i%32 == 0 && progress != nil && !progress() {
			return nil
		}
		text := doc.text[span.start:span.end]
		// Without an override block, precision cannot alter text or enter drawing mode.
		if !strings.Contains(text, "{") {
			continue
		}
		if step == 0 && !strings.Contains(text, `\p`) && !strings.Contains(text, `\clip(`) && !strings.Contains(text, `\iclip(`) {
			continue
		}
		if !animated && (span.effect != "" || precisionAnimated.MatchString(text)) {
			continue
		}
		pieces, cached := analysis.pieces[text]
		ok := true
		if !cached {
			pieces, ok = c.scanText(text)
			if ok && analysis.bytes+len(text)*20 < 16<<20 {
				if analysis.pieces == nil {
					analysis.pieces = map[string][]piece{}
				}
				analysis.pieces[text] = pieces
				analysis.bytes += len(text) * 20
			}
		}
		if !ok || drawingPresent(pieces) && (strings.Contains(text, `\{`) || strings.Contains(text, `\}`)) {
			continue
		}
		var replacement strings.Builder
		scale := 0
		for _, p := range pieces {
			if !p.block {
				new := p.content
				if scale > 0 {
					dx, dy := places(step, res[0]*(1<<(scale-1)), config.Width), places(step, res[1]*(1<<(scale-1)), config.Height)
					new = analysis.rewriteDrawing(new, dx, dy)
				}
				replacement.WriteString(new)
				continue
			}
			replacement.WriteByte('{')
			replacement.WriteString(p.prefix)
			for _, token := range p.tokens {
				if m := precisionDrawing.FindStringSubmatch(token.raw); m != nil {
					scale, _ = strconv.Atoi(m[1])
					if scale > 30 {
						scale = 0
					}
				} else if strings.HasPrefix(token.raw, `\r`) || drawingPresent([]piece{{block: true, content: token.raw}}) {
					scale = 0
				}
				new := c.quantizeToken(token.raw, xp, yp, 0, analysis)
				if m := precisionClip.FindStringSubmatch(new); m != nil {
					scale := 1
					if m[2] != "" {
						scale = 0
						if len(m[2]) < 3 {
							scale, _ = strconv.Atoi(m[2])
						}
					}
					if scale >= 1 && scale <= 30 {
						new = m[1]
						if m[2] != "" {
							new += m[2] + ","
						}
						new += analysis.rewriteDrawing(m[3], xp, yp) + ")"
					}
				}
				replacement.WriteString(new)
			}
			replacement.WriteByte('}')
		}
		if replacement.Len() < len(text) {
			edits = append(edits, edit{span.start, span.end, replacement.String(), "precision"})
		}
	}
	return edits
}
