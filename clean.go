// Package subtrim implements static cleanup and native-checked O1/O2 optimization.
package subtrim

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const num = `(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)`
const signed = `-?` + num

var simpleTag = regexp.MustCompile(`^\\(xbord|ybord|xshad|yshad|fscx|fscy|blur|bord|shad|frx|fry|frz|fsp|fax|fay|fs|fr|be|b|i|u|s)(` + signed + `)$`)
var colorTag = regexp.MustCompile(`^\\(1c|2c|3c|4c|c|1a|2a|3a|4a|alpha)&H([0-9a-fA-F]{1,8})&?$`)
var numberPattern = regexp.MustCompile(`^` + signed + `$`)
var unsignedPattern = regexp.MustCompile(`^[0-9]+$`)
var integerPattern = regexp.MustCompile(`^-?[0-9]+$`)
var transparentSimple = regexp.MustCompile(`^\\(an|a|q|fe|p|pbo|kt|kf|ko|k|K)(` + signed + `)$`)
var transparentComplex = regexp.MustCompile(`^\\(pos|move|org|fad|fade|clip|iclip)\(([^\\(){}]*)\)$`)
var transformPrefix = regexp.MustCompile(`^(?:[0-9]{1,9}(?:\.[0-9]{1,6})?,){0,3}$`)
var scaleReset = regexp.MustCompile(`^\\fsc(?:` + signed + `)?$`)
var scaleTransform = regexp.MustCompile(`^\\t\((?:[0-9]+(?:\.[0-9]+)?,){0,3}(\\fsc(?:` + signed + `)?)\)$`)
var karaoke = regexp.MustCompile(`\\[ \t]*[kK]`)
var resetTag = regexp.MustCompile(`^\\r([^(){}\\]*)$`)
var trailingFont = regexp.MustCompile(`^\\[bi][01]$`)
var alignmentTag = regexp.MustCompile(`^\\an[1-9]$`)
var oldAlignment = regexp.MustCompile(`^\\a[0-9-]`)
var transformNumbers = regexp.MustCompile(`^\\t\(((?:` + num + `,){1,3})(\\[^(){}]*)\)$`)
var assHeader = regexp.MustCompile(`(?m)^\[V4\+ Styles\][ \t]*\r?$`)
var ssaHeader = regexp.MustCompile(`(?mi)^[ \t]*\[v4 styles\]`)

type state map[string]any
type token struct {
	raw, canonical string
	writes         state
}
type piece struct {
	block           bool
	content, prefix string
	tokens          []token
}

// caches belong to one file and are discarded when Clean returns.
type cleaner struct {
	tokens          map[string]token
	transparentTags map[string]bool
	transforms      map[string]state
}

func (c *cleaner) assignment(raw string) token {
	if t, ok := c.tokens[raw]; ok {
		return t
	}
	t := assignment(raw)
	if len(c.tokens) >= 16384 {
		clear(c.tokens)
	}
	c.tokens[raw] = t
	return t
}

func number(s string) string {
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "-")
	a, b, _ := strings.Cut(s, ".")
	a = strings.TrimLeft(a, "0")
	if a == "" {
		a = "0"
	}
	b = strings.TrimRight(b, "0")
	if b != "" {
		a += "." + b
	}
	if negative && a != "0" {
		a = "-" + a
	}
	return a
}

func assignment(raw string) token {
	t := token{raw: raw, canonical: raw}
	if strings.HasPrefix(raw, `\fn`) {
		family := raw[3:]
		valid := family != "" && family != "0" && strings.Trim(family, " \t") == family && !strings.ContainsAny(family, `\(){}`)
		for _, c := range family {
			if c < 32 {
				valid = false
			}
		}
		if valid {
			t.writes = state{"fn": family}
		}
		return t
	}
	if m := simpleTag.FindStringSubmatch(raw); m != nil {
		tag, arg := m[1], m[2]
		unsigned := strings.TrimLeft(arg, "-")
		_, fraction, _ := strings.Cut(unsigned, ".")
		if len(strings.TrimLeft(strings.ReplaceAll(unsigned, ".", ""), "0")) > 15 || len(fraction) > 6 {
			return t
		}
		v, _ := strconv.ParseFloat(arg, 64)
		if math.Abs(v) > 1e6 {
			return t
		}
		if tag == "fs" && (strings.HasPrefix(arg, "-") || v <= 0) {
			return t
		}
		if tag == "b" || tag == "i" || tag == "u" || tag == "s" || tag == "be" {
			if !unsignedPattern.MatchString(arg) {
				return t
			}
			if tag == "b" && !(v == 0 || v == 1 || (v >= 100 && v <= 1000)) {
				return t
			}
			if (tag == "i" || tag == "u" || tag == "s") && v != 0 && v != 1 {
				return t
			}
		}
		if (tag == "bord" || tag == "xbord" || tag == "ybord" || tag == "shad" || tag == "blur" || tag == "fscx" || tag == "fscy" || tag == "be") && v < 0 {
			return t
		}
		if (tag == "be" || tag == "blur") && v > 100 {
			return t
		}
		if tag == "frz" {
			tag = "fr"
		}
		t.writes = state{tag: v}
		if tag == "bord" {
			t.writes = state{"xbord": v, "ybord": v}
		}
		if tag == "shad" {
			t.writes = state{"xshad": v, "yshad": v}
		}
		if tag == "fr" {
			tag = "frz"
		}
		canonical := `\` + tag + number(arg)
		if len(canonical) <= len(raw) {
			t.canonical = canonical
		}
		return t
	}
	if m := colorTag.FindStringSubmatch(raw); m != nil {
		tag := m[1]
		n, _ := strconv.ParseUint(m[2], 16, 32)
		alpha := strings.HasSuffix(tag, "a") || tag == "alpha"
		if (alpha && n > 255) || (!alpha && n > 0xffffff) {
			return t
		}
		if tag == "1c" {
			tag = "c"
		}
		t.writes = state{tag: float64(n)}
		if tag == "alpha" {
			t.writes = state{"1a": float64(n), "2a": float64(n), "3a": float64(n), "4a": float64(n)}
		}
		canonical := fmt.Sprintf(`\%s&H%X&`, tag, n)
		if len(canonical) <= len(raw) {
			t.canonical = canonical
		}
	}
	if t.writes == nil {
		t.canonical = compactArguments(raw)
	}
	return t
}

func (c *cleaner) splitTokens(content string) (string, []token, bool) {
	first := strings.IndexByte(content, '\\')
	if first < 0 {
		return content, nil, true
	}
	start, depth := first, 0
	var tokens []token
	for i := first + 1; i < len(content); i++ {
		switch content[i] {
		case '(':
			depth++
		case ')':
			depth = max(0, depth-1)
		case '\\':
			if depth == 0 {
				tokens = append(tokens, c.assignment(content[start:i]))
				start = i
			}
		}
	}
	if depth != 0 {
		return "", nil, false
	}
	tokens = append(tokens, c.assignment(content[start:]))
	return content[:first], tokens, true
}

func (c *cleaner) scanText(text string) ([]piece, bool) {
	var pieces []piece
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && strings.ContainsRune(`Nnh{}`, rune(text[i+1])) {
			i++
			continue
		}
		if text[i] != '{' {
			continue
		}
		end := strings.IndexByte(text[i+1:], '}')
		if end < 0 {
			return nil, false
		}
		end += i + 1
		if strings.ContainsRune(text[i+1:end], '{') {
			return nil, false
		}
		if start < i {
			pieces = append(pieces, piece{content: text[start:i]})
		}
		prefix, tokens, ok := c.splitTokens(text[i+1 : end])
		if !ok {
			return nil, false
		}
		pieces = append(pieces, piece{block: true, content: text[i+1 : end], prefix: prefix, tokens: tokens})
		start = end + 1
		i = end
	}
	if start < len(text) {
		pieces = append(pieces, piece{content: text[start:]})
	}
	return pieces, true
}

func drawingParts(path string) []string {
	var parts []string
	count := 0
	command := byte(0)
	validGroup := func() bool {
		return count > 0 && ((command == 'b' && count%6 == 0) || ((command == 'm' || command == 'n') && count == 2) || (command == 'l' && count%2 == 0))
	}
	for i := 0; i < len(path); {
		if strings.IndexByte(" \t\r\n\v\f", path[i]) >= 0 {
			i++
			continue
		}
		if path[i] >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(path[i:])
			if !unicode.IsSpace(r) {
				return nil
			}
			i += size
			continue
		}
		start := i
		if strings.IndexByte("mnlb", path[i]) >= 0 {
			i++
		} else {
			if path[i] == '-' {
				i++
			}
			digits := 0
			for i < len(path) && path[i] >= '0' && path[i] <= '9' {
				i++
				digits++
			}
			if i < len(path) && path[i] == '.' {
				i++
				for i < len(path) && path[i] >= '0' && path[i] <= '9' {
					i++
					digits++
				}
			}
			if digits == 0 {
				return nil
			}
		}
		value := path[start:i]
		parts = append(parts, value)
		if len(parts) > 20000 {
			return nil
		}
		if len(value) == 1 && strings.ContainsAny(value, "mnlb") {
			if command == 0 {
				if value != "m" && value != "n" {
					return nil
				}
			} else if !validGroup() {
				return nil
			}
			command = value[0]
			count = 0
		} else {
			v, _ := strconv.ParseFloat(value, 64)
			if command == 0 || len(value) > 32 || math.Abs(v) > 1e6 {
				return nil
			}
			if math.Abs(v) == 1e6 {
				exact, _ := new(big.Rat).SetString(value)
				if new(big.Rat).Abs(exact).Cmp(big.NewRat(1000000, 1)) > 0 {
					return nil
				}
			}
			count++
		}
	}
	if !validGroup() {
		return nil
	}
	return parts
}

func (c *cleaner) transparent(raw string) (result bool) {
	if value, ok := c.transparentTags[raw]; ok {
		return value
	}
	defer func() {
		if len(c.transparentTags) >= 8192 {
			clear(c.transparentTags)
		}
		c.transparentTags[raw] = result
	}()
	if m := transparentSimple.FindStringSubmatch(raw); m != nil {
		v, _ := strconv.ParseFloat(m[2], 64)
		return len(m[2]) <= 16 && math.Abs(v) <= 1e6 && (m[1] == "pbo" || m[1] == "kt" || m[1] == "kf" || m[1] == "ko" || m[1] == "k" || m[1] == "K" || integerPattern.MatchString(m[2]))
	}
	m := transparentComplex.FindStringSubmatch(raw)
	if m == nil {
		return false
	}
	tag, args := m[1], strings.Split(m[2], ",")
	for i := range args {
		args[i] = strings.Trim(args[i], " \t")
	}
	if (tag == "clip" || tag == "iclip") && (len(args) == 1 || len(args) == 2) {
		if len(args) == 2 {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 || n > 30 || strconv.Itoa(n) != args[0] {
				return false
			}
		}
		return drawingParts(args[len(args)-1]) != nil
	}
	count := map[string]int{"pos": 2, "move": 4, "org": 2, "fad": 2, "fade": 7, "clip": 4, "iclip": 4}[tag]
	if len(args) != count && !(tag == "move" && len(args) == 6) {
		return false
	}
	for _, a := range args {
		v, _ := strconv.ParseFloat(a, 64)
		if len(a) > 16 || !numberPattern.MatchString(a) || math.Abs(v) > 1e6 {
			return false
		}
	}
	return true
}

func (c *cleaner) transformWrites(raw string) (result state) {
	if !strings.HasPrefix(raw, `\t(`) || !strings.HasSuffix(raw, ")") {
		return nil
	}
	if value, ok := c.transforms[raw]; ok {
		return value
	}
	defer func() {
		if len(c.transforms) >= 8192 {
			clear(c.transforms)
		}
		c.transforms[raw] = result
	}()
	inner := raw[3 : len(raw)-1]
	offset := strings.IndexByte(inner, '\\')
	if offset < 0 || !transformPrefix.MatchString(inner[:offset]) {
		return nil
	}
	prefix, tokens, ok := c.splitTokens(inner[offset:])
	if !ok || prefix != "" || len(tokens) == 0 {
		return nil
	}
	fields := state{}
	for _, t := range tokens {
		if t.writes == nil {
			return nil
		}
		if _, ok := t.writes["fn"]; ok {
			return nil
		}
		maps.Copy(fields, t.writes)
	}
	return fields
}

func compactArguments(raw string) string {
	if strings.HasPrefix(raw, `\t(`) {
		m := transformNumbers.FindStringSubmatch(raw)
		if m == nil {
			return raw
		}
		args := strings.Split(strings.TrimSuffix(m[1], ","), ",")
		for i, a := range args {
			_, fraction, _ := strings.Cut(a, ".")
			v, _ := strconv.ParseFloat(a, 64)
			if len(a) > 16 || len(fraction) > 6 || v > 1e6 {
				return raw
			}
			args[i] = number(a)
		}
		compact := `\t(` + strings.Join(args, ",") + "," + m[2] + ")"
		if len(compact) <= len(raw) {
			return compact
		}
		return raw
	}
	m := transparentComplex.FindStringSubmatch(raw)
	if m == nil {
		return raw
	}
	tag, args := m[1], strings.Split(m[2], ",")
	if tag != "pos" && tag != "org" && tag != "move" && tag != "clip" && tag != "iclip" {
		return raw
	}
	count := map[string]int{"pos": 2, "org": 2, "move": 4, "clip": 4, "iclip": 4}[tag]
	if len(args) != count && !(tag == "move" && len(args) == 6) {
		return raw
	}
	for i, a := range args {
		_, fraction, _ := strings.Cut(a, ".")
		v, _ := strconv.ParseFloat(a, 64)
		if !numberPattern.MatchString(a) || len(a) > 24 || len(fraction) > 6 || len(strings.TrimLeft(strings.ReplaceAll(strings.TrimPrefix(a, "-"), ".", ""), "0")) > 15 || math.Abs(v) > 1e6 {
			return raw
		}
		args[i] = number(a)
	}
	compact := `\` + tag + "(" + strings.Join(args, ",") + ")"
	if len(compact) <= len(raw) {
		return compact
	}
	return raw
}

func (c *cleaner) compactSetters(raws []string) string {
	type pendingSetter struct {
		position int
		value    any
	}
	pending := map[string]pendingSetter{}
	var out []string
	for _, raw := range raws {
		t := c.assignment(raw)
		if t.writes == nil {
			clear(pending)
		} else {
			for field := range t.writes {
				delete(pending, field)
			}
			if len(t.writes) == 1 {
				for field, value := range t.writes {
					var fields []string
					tag := ""
					switch field {
					case "xbord", "ybord":
						tag = "bord"
						fields = []string{"xbord", "ybord"}
					case "xshad", "yshad":
						tag = "shad"
						fields = []string{"xshad", "yshad"}
					case "1a", "2a", "3a", "4a":
						tag = "alpha"
						fields = []string{"1a", "2a", "3a", "4a"}
					}
					if tag == "" {
						continue
					}
					pending[field] = pendingSetter{len(out), value}
					v := value.(float64)
					equal := v >= 0
					for _, f := range fields {
						p, ok := pending[f]
						if !ok || p.value != value {
							equal = false
						}
					}
					if equal {
						for _, f := range fields {
							p := pending[f]
							delete(pending, f)
							if p.position < len(out) {
								out[p.position] = ""
							}
						}
						arg := t.canonical[len(field)+1:]
						if tag == "alpha" {
							arg = fmt.Sprintf("&H%X&", int(v))
						}
						raw = `\` + tag + arg
					}
				}
			}
		}
		out = append(out, raw)
	}
	return strings.Join(out, "")
}

func drawingPresent(pieces []piece) bool {
	for _, p := range pieces {
		if p.block {
			for i := 0; i < len(p.content); i++ {
				if p.content[i] != '\\' {
					continue
				}
				arg := strings.TrimLeft(p.content[i+1:], " \t")
				if strings.HasPrefix(arg, "p") && !strings.HasPrefix(arg, "pos") && !strings.HasPrefix(arg, "pbo") {
					return true
				}
			}
		}
	}
	return false
}

// The second result says whether ignored-tag stripping may still change the text.
func (c *cleaner) cleanText(original string, initial state, reset func(string) state) (string, bool) {
	if !strings.ContainsRune(original, '{') {
		return original, false
	}
	text := strings.TrimRight(original, " \t")
	tail := original[len(text):]
	pieces, ok := c.scanText(text)
	if !ok {
		return original, false
	}
	drawing := drawingPresent(pieces)
	if drawing && (strings.Contains(text, `\{`) || strings.Contains(text, `\}`)) {
		return original, false
	}
	leading := 0
	for leading < len(pieces) && pieces[leading].block {
		leading++
	}
	if leading > 1 {
		p := piece{block: true}
		for _, old := range pieces[:leading] {
			p.content += old.content
			p.prefix += old.prefix
			p.tokens = append(p.tokens, old.tokens...)
		}
		pieces = append([]piece{p}, pieces[leading:]...)
	}
	current, active := maps.Clone(initial), initial
	noKaraoke := !karaoke.MatchString(text)
	lastText := -1
	needsStrip := false
	var all []token
	for i, p := range pieces {
		if !p.block {
			lastText = i
		}
		all = append(all, p.tokens...)
		for _, t := range p.tokens {
			if !needsStrip && !recognizedTag(t) {
				needsStrip = true
			}
		}
	}
	scaleSafe := false
	if _, x := initial["fscx"]; x {
		if _, y := initial["fscy"]; y {
			safe, position := true, false
			for _, t := range all {
				if (strings.HasPrefix(t.raw, `\pos(`) || strings.HasPrefix(t.raw, `\move(`)) && c.transparent(t.raw) {
					position = true
				}
				if t.writes == nil && !c.transparent(t.raw) && !scaleReset.MatchString(t.raw) && !scaleTransform.MatchString(t.raw) && c.transformWrites(t.raw) == nil {
					safe = false
				}
			}
			scaleSafe = safe && position
		}
	}
	dropAlignment := false
	if alignment, ok := initial["_alignment"]; ok && strings.Contains(text, `\an`) {
		dropAlignment = true
		expected := fmt.Sprintf(`\an%d`, int(alignment.(float64)))
		for _, t := range all {
			if strings.HasPrefix(t.raw, `\an`) {
				if t.raw != expected {
					dropAlignment = false
				}
			} else if t.writes == nil && t.raw != `\fs` && t.raw != `\fsp` && !(c.transparent(t.raw) && !oldAlignment.MatchString(t.raw)) {
				dropAlignment = false
			}
		}
	}
	globals := map[string]bool{}
	var result []string
	for index, p := range pieces {
		if !p.block {
			result = append(result, p.content)
			continue
		}
		var tokens []token
		for _, t := range p.tokens {
			family := ""
			if t.writes == nil && c.transparent(t.raw) {
				switch {
				case strings.HasPrefix(t.raw, `\fad(`) || strings.HasPrefix(t.raw, `\fade(`):
					family = "fade"
				case strings.HasPrefix(t.raw, `\pos(`) || strings.HasPrefix(t.raw, `\move(`):
					family = "position"
				case strings.HasPrefix(t.raw, `\org(`):
					family = "origin"
				case alignmentTag.MatchString(t.raw):
					family = "alignment"
				}
			}
			if family != "" {
				if family == "alignment" && dropAlignment || globals[family] {
					continue
				}
				globals[family] = true
			} else if t.writes == nil && !c.transparent(t.raw) && c.transformWrites(t.raw) == nil {
				clear(globals)
			}
			if noKaraoke && len(t.writes) == 1 {
				if _, ok := t.writes["2c"]; ok {
					continue
				}
			}
			if !drawing && index > lastText && trailingFont.MatchString(t.raw) {
				continue
			}
			tokens = append(tokens, t)
		}
		covered := state{}
		var kept []token
		for i := len(tokens) - 1; i >= 0; i-- {
			t := tokens[i]
			if t.writes == nil {
				clear(covered)
				kept = append(kept, t)
				continue
			}
			shadowed := true
			for key := range t.writes {
				if _, ok := covered[key]; !ok {
					shadowed = false
				}
			}
			if !shadowed {
				maps.Copy(covered, t.writes)
				kept = append(kept, t)
			}
		}
		var out []string
		for i := len(kept) - 1; i >= 0; i-- {
			t := kept[i]
			if t.raw == `\fs` || t.raw == `\fsp` {
				if value, ok := active[t.raw[1:]]; ok {
					t.writes = state{t.raw[1:]: value}
				}
			}
			if t.writes == nil {
				if scaleSafe && (scaleReset.MatchString(t.raw) || scaleTransform.MatchString(t.raw)) {
					redundant := current["fscx"] == initial["fscx"] && current["fscy"] == initial["fscy"]
					current["fscx"] = initial["fscx"]
					current["fscy"] = initial["fscy"]
					if !redundant {
						out = append(out, t.raw)
					}
					continue
				}
				if fields := c.transformWrites(t.raw); fields != nil {
					for key := range fields {
						delete(current, key)
					}
				} else if !c.transparent(t.raw) {
					clear(current)
					active = nil
				}
				if m := resetTag.FindStringSubmatch(t.raw); m != nil && reset != nil {
					maps.Copy(current, reset(strings.TrimRight(m[1], " \t")))
					active = maps.Clone(current)
				}
				out = append(out, t.canonical)
			} else {
				equal := true
				for key, value := range t.writes {
					if actual, ok := current[key]; !ok || actual != value {
						equal = false
					}
				}
				if !equal {
					maps.Copy(current, t.writes)
					out = append(out, t.canonical)
				}
			}
		}
		new := c.compactSetters(out)
		if new != "" {
			result = append(result, "{"+new+"}")
		} else if drawing || (len(result) > 0 && strings.HasSuffix(result[len(result)-1], `\`)) {
			result = append(result, "{}")
		}
	}
	cleaned := strings.Join(result, "")
	if strings.HasSuffix(cleaned, " ") || strings.HasSuffix(cleaned, "\t") {
		cleaned += "{}"
	}
	return cleaned + tail, needsStrip
}

var libassPrefixes = []string{"xbord", "ybord", "xshad", "yshad", "fax", "fay", "iclip", "blur", "fscx", "fscy", "fsc", "fsp", "fs", "bord", "move", "frx", "fry", "frz", "fr", "fn", "alpha", "an", "a", "pos", "fade", "fad", "org", "t", "clip", "c", "1c", "2c", "3c", "4c", "1a", "2a", "3a", "4a", "r", "be", "b", "i", "kt", "kf", "K", "ko", "k", "shad", "s", "u", "pbo", "p", "q", "fe"}

func recognizedTag(t token) bool {
	if t.writes != nil || strings.Contains(t.raw[1:], `\`) {
		return true
	}
	name := strings.TrimLeft(t.raw[1:], " \t")
	for _, prefix := range libassPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (c *cleaner) stripIgnored(text string) string {
	pieces, ok := c.scanText(text)
	if !ok {
		return text
	}
	if drawingPresent(pieces) && (strings.Contains(text, `\{`) || strings.Contains(text, `\}`)) {
		return text
	}
	for _, p := range pieces {
		if !p.block {
			continue
		}
		depth := 0
		for _, c := range p.content {
			if c == '(' {
				depth++
				if depth > 1 {
					return text
				}
			}
			if c == ')' {
				depth--
				if depth < 0 {
					return text
				}
			}
		}
		if depth != 0 {
			return text
		}
	}
	var out strings.Builder
	for _, p := range pieces {
		if !p.block {
			out.WriteString(p.content)
			continue
		}
		out.WriteByte('{')
		out.WriteString(p.prefix)
		for _, t := range p.tokens {
			if recognizedTag(t) {
				out.WriteString(t.raw)
			}
		}
		out.WriteByte('}')
	}
	return out.String()
}

// Clean preserves source encoding, record fields, line endings and comments.
// Unsupported or ambiguous input is retained byte-for-byte.
func Clean(data []byte) []byte {
	doc, ok := decode(data)
	if !ok || !assHeader.MatchString(doc.text) || ssaHeader.MatchString(doc.text) {
		return data
	}
	return doc.encode(cleanStatic(doc.text))
}

// cleanStatic receives decoded ASS text; callers own transport and header checks.
func cleanStatic(text string) string {
	c := cleaner{tokens: make(map[string]token), transparentTags: make(map[string]bool), transforms: make(map[string]state)}
	lines := strings.SplitAfter(text, "\n")
	tracker, final := newStyleTracker(), newStyleTracker()
	for _, line := range lines {
		final.feed(line)
	}
	section := ""
	index, styleIndex := -1, -1
	var out strings.Builder
	out.Grow(len(text))
	for _, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		tracker.feed(body)
		stripped := strings.TrimLeft(body, " \t")
		if strings.HasPrefix(stripped, "[") {
			section = stripped
			if strings.HasPrefix(strings.ToLower(stripped), "[events]") {
				section = "[Events]"
			}
			index, styleIndex = -1, -1
		}
		if section == "[Events]" {
			if strings.HasPrefix(stripped, "Format:") {
				index, styleIndex = -1, -1
				if fields := eventFormat(stripped[7:]); fields != nil {
					index, styleIndex = fields["text"], fields["style"]
				}
			} else if strings.HasPrefix(stripped, "Dialogue:") && index >= 0 {
				offset := strings.Index(body, "Dialogue:") + len("Dialogue:")
				fields := strings.SplitN(body[offset:], ",", index+1)
				if len(fields) == index+1 {
					initial := tracker.initial(fields[styleIndex])
					reset := func(name string) state {
						if name == "" {
							return initial
						}
						if !final.enabled || final.trackType != "ass" {
							return state{}
						}
						if s, ok := final.styles[name]; ok {
							return s
						}
						return initial
					}
					cleaned, needsStrip := c.cleanText(fields[index], initial, reset)
					for needsStrip {
						stripped := c.stripIgnored(cleaned)
						if stripped == cleaned {
							break
						}
						cleaned, needsStrip = c.cleanText(stripped, initial, reset)
					}
					if cleaned != fields[index] {
						body = body[:len(body)-len(fields[index])] + cleaned
					}
				}
			}
		}
		out.WriteString(body)
		out.WriteString(eol)
	}
	return out.String()
}

var styleFields = strings.Fields("name fontname fontsize primarycolour secondarycolour outlinecolour backcolour bold italic underline strikeout scalex scaley spacing angle borderstyle outline shadow alignment marginl marginr marginv encoding")
var styleDecimal = regexp.MustCompile(`^[+-]?` + num + `$`)
var styleInteger = regexp.MustCompile(`^[+-]?[0-9]+$`)
var styleColor = regexp.MustCompile(`^&[Hh]([0-9a-fA-F]{1,8})&?$`)

func smallInteger(value string) (float64, bool) {
	if !styleInteger.MatchString(value) {
		return 0, false
	}
	significant := strings.TrimLeft(strings.TrimLeft(value, "+-"), "0")
	if len(significant) > 7 {
		return 0, false
	}
	v, err := strconv.ParseFloat(value, 64)
	return v, err == nil && math.Abs(v) <= 1e6
}

func styleState(fields map[string]string) state {
	s := state{"frx": 0.0, "fry": 0.0, "fax": 0.0, "fay": 0.0, "be": 0.0, "blur": 0.0}
	if v, ok := smallInteger(fields["alignment"]); ok && v >= 1 && v <= 9 {
		s["_alignment"] = v
	}
	if v, ok := smallInteger(fields["borderstyle"]); ok && (v == 1 || v == 3) {
		s["_border_style"] = v
	}
	if t := assignment(`\fn` + fields["fontname"]); t.writes != nil {
		maps.Copy(s, t.writes)
	}
	for source, key := range map[string]string{"bold": "b", "italic": "i", "underline": "u", "strikeout": "s"} {
		if v, ok := smallInteger(fields[source]); ok {
			s[key] = 0.0
			if v != 0 {
				s[key] = 1.0
			}
		}
	}
	for _, field := range []struct {
		source string
		keys   []string
		clamp  bool
	}{
		{"fontsize", []string{"fs"}, false}, {"scalex", []string{"fscx"}, true}, {"scaley", []string{"fscy"}, true},
		{"spacing", []string{"fsp"}, true}, {"angle", []string{"fr"}, false}, {"outline", []string{"xbord", "ybord"}, true}, {"shadow", []string{"xshad", "yshad"}, true},
	} {
		value := fields[field.source]
		if !styleDecimal.MatchString(value) {
			continue
		}
		unsigned := strings.TrimLeft(value, "+-")
		_, fraction, _ := strings.Cut(unsigned, ".")
		if len(strings.TrimLeft(strings.ReplaceAll(unsigned, ".", ""), "0")) > 15 || len(fraction) > 6 {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || math.Abs(v) > 1e6 || field.source == "fontsize" && v <= 0 {
			continue
		}
		if field.clamp {
			v = max(v, 0)
		}
		for _, key := range field.keys {
			s[key] = v
		}
	}
	for i, source := range []string{"primarycolour", "secondarycolour", "outlinecolour", "backcolour"} {
		value := fields[source]
		var color uint32
		if m := styleColor.FindStringSubmatch(value); m != nil {
			n, _ := strconv.ParseUint(m[1], 16, 32)
			color = uint32(n)
		} else {
			if !styleInteger.MatchString(value) || len(strings.TrimLeft(strings.TrimLeft(value, "+-"), "0")) > 10 {
				continue
			}
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || n < math.MinInt32 || n > math.MaxUint32 {
				continue
			}
			color = uint32(int64(n))
		}
		key := strconv.Itoa(i+1) + "c"
		if i == 0 {
			key = "c"
		}
		s[key] = float64(color & 0xffffff)
		s[strconv.Itoa(i+1)+"a"] = float64(color >> 24)
	}
	return s
}

type styleTracker struct {
	section, trackType string
	format             []string
	styles             map[string]state
	enabled            bool
}

func newStyleTracker() *styleTracker { return &styleTracker{styles: map[string]state{}, enabled: true} }

func (s *styleTracker) feed(line string) {
	line = strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
	lower := line
	if strings.HasPrefix(line, "[") {
		lower = strings.ToLower(line)
	}
	for _, header := range []struct{ name, section string }{{"[script info]", "info"}, {"[v4+ styles]", "ass_styles"}, {"[v4 styles]", "ssa_styles"}, {"[events]", "events"}, {"[fonts]", "fonts"}} {
		if strings.HasPrefix(lower, header.name) {
			s.section = header.section
			if s.section == "ass_styles" {
				s.trackType = "ass"
			}
			if s.section == "ssa_styles" {
				s.trackType = "ssa"
				s.enabled = false
			}
			return
		}
	}
	if s.section == "info" && strings.HasPrefix(line, "ScriptType:") {
		if strings.Trim(line[len("ScriptType:"):], " \t") == "v4.00+" {
			s.trackType = "ass"
		} else {
			s.trackType = "unknown"
		}
		return
	}
	if s.section != "ass_styles" && s.section != "ssa_styles" {
		return
	}
	if strings.HasPrefix(line, "Format:") {
		cols := strings.Split(line[len("Format:"):], ",")
		seen := map[string]bool{}
		for i, c := range cols {
			cols[i] = strings.ToLower(strings.Trim(c, " \t"))
			seen[cols[i]] = true
		}
		s.format = nil
		if len(cols) == len(styleFields) && len(seen) == len(styleFields) {
			valid := true
			for _, f := range styleFields {
				if !seen[f] {
					valid = false
				}
			}
			if valid {
				s.format = cols
			}
		}
		return
	}
	if !strings.HasPrefix(line, "Style:") {
		return
	}
	if s.trackType != "ass" || s.format == nil {
		s.enabled = false
		return
	}
	values := strings.Split(line[len("Style:"):], ",")
	if len(values) != len(s.format) {
		s.enabled = false
		return
	}
	fields := map[string]string{}
	for i, value := range values {
		value = strings.Trim(value, " \t")
		if value == "" {
			s.enabled = false
			return
		}
		fields[s.format[i]] = value
	}
	name := strings.TrimLeft(fields["name"], "*")
	if name == "" {
		s.enabled = false
		return
	}
	s.styles[name] = styleState(fields)
}

// Stored style states are immutable; event-local edits clone them before writing.
func (s *styleTracker) initial(name string) state {
	if !s.enabled || s.trackType != "ass" {
		return state{}
	}
	name = strings.TrimLeft(strings.Trim(name, " \t"), "*")
	if strings.EqualFold(name, "default") {
		name = "Default"
	}
	if value, ok := s.styles[name]; ok {
		return value
	}
	if value, ok := s.styles["Default"]; ok {
		return value
	}
	return state{}
}
