package subtrim

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type record struct {
	row         int
	fields      []string
	prefix, eol string
}

type layout struct {
	doc            document
	lines          []string
	offsets        []int
	styles, events []record
	comments       []int
	fields         map[string]int
	styleFields    []string
}

// layoutRecords accepts strict style and event record formats.
func layoutRecords(doc document) *layout {
	l := &layout{doc: doc, lines: strings.SplitAfter(doc.text, "\n")}
	section := ""
	sections := map[string]int{}
	offset := 0
	for row, line := range l.lines {
		l.offsets = append(l.offsets, offset)
		offset += len(line)
		s := strings.TrimRight(strings.TrimLeft(line, " \t"), "\r\n")
		if strings.HasPrefix(s, "[") {
			section = strings.TrimRight(s, " \t")
			sections[section]++
		} else if strings.HasPrefix(s, "Format:") {
			if section == "[Events]" {
				l.fields = eventFormat(s[7:])
				if l.fields == nil {
					return nil
				}
				continue
			}
			fields := strings.Split(s[7:], ",")
			seen := map[string]int{}
			for i := range fields {
				fields[i] = strings.ToLower(strings.TrimSpace(fields[i]))
				if _, exists := seen[fields[i]]; exists {
					return nil
				}
				seen[fields[i]] = i
			}
			if section == "[V4+ Styles]" {
				if fields[0] != "name" {
					return nil
				}
				l.styleFields = fields
			}
		} else if section == "[V4+ Styles]" && strings.HasPrefix(s, "Style:") {
			fields := strings.Split(strings.TrimLeft(s[6:], " \t"), ",")
			if l.styleFields == nil || len(fields) != len(l.styleFields) {
				return nil
			}
			l.styles = append(l.styles, record{row: row, fields: fields})
		} else if section == "[Events]" && strings.HasPrefix(s, "Comment:") {
			l.comments = append(l.comments, row)
		} else if section == "[Events]" && strings.HasPrefix(s, "Dialogue:") {
			if l.fields == nil {
				return nil
			}
			body := strings.TrimRight(line, "\r\n")
			offset := strings.Index(body, "Dialogue:") + 9
			fields := strings.SplitN(body[offset:], ",", 10)
			if len(fields) != 10 {
				return nil
			}
			l.events = append(l.events, record{row, fields, body[:offset], line[len(body):]})
		}
	}
	if sections["[V4+ Styles]"] != 1 || sections["[Events]"] != 1 || sections["[V4 Styles]"] != 0 {
		return nil
	}
	return l
}

var recordTime = regexp.MustCompile(`^([0-9]{1,6}):([0-5][0-9]):([0-5][0-9])\.([0-9]{2})$`)
var resetReference = regexp.MustCompile(`\\[ \t]*r([^\\{}]*)`)
var simpleStyleName = regexp.MustCompile(`^[A-Za-z0-9 _-]*$`)
var marginValue = regexp.MustCompile(`^[0-9]{1,6}$`)
var staticAnimation = regexp.MustCompile(`\\(?:t\(|fad|move|[kK])`)
var hiddenAlpha = regexp.MustCompile(`\\(?:alpha|1a)&H[Ff]{2}&?`)
var overrideBlock = regexp.MustCompile(`\{[^{}]*\}`)
var drawingText = regexp.MustCompile(`\\p[0-9]`)

func timestamp(value string) (int64, bool) {
	m := recordTime.FindStringSubmatch(value)
	if m == nil {
		return 0, false
	}
	var v [4]int64
	for i := range v {
		v[i], _ = strconv.ParseInt(m[i+1], 10, 64)
	}
	return ((v[0]*60+v[1])*60+v[2])*1000 + v[3]*10, true
}

// staticStructure removes only records and fields with an audited static proof.
func staticStructure(doc document) string {
	l := layoutRecords(doc)
	if l == nil {
		return doc.text
	}
	remove, replace := map[int]bool{}, map[int]string{}
	for _, row := range l.comments {
		remove[row] = true
	}
	if l.fields != nil {
		yi, ti := l.fields["style"], l.fields["text"]
		normalize := func(s string) string { return strings.ToLower(strings.TrimLeft(strings.Trim(s, " \t"), "*")) }
		used := map[string]bool{"default": true}
		if len(l.styles) > 0 {
			used[normalize(l.styles[0].fields[0])] = true
		}
		certain := true
		for _, e := range l.events {
			used[normalize(e.fields[yi])] = true
			for _, m := range resetReference.FindAllStringSubmatch(e.fields[ti], -1) {
				if !simpleStyleName.MatchString(m[1]) {
					certain = false
				}
				used[normalize(m[1])] = true
			}
		}
		if certain {
			for _, s := range l.styles {
				name := normalize(s.fields[0])
				if name != "" && simpleStyleName.MatchString(s.fields[0]) && !used[name] {
					remove[s.row] = true
				}
			}
		}
		c := cleaner{tokens: map[string]token{}, transparentTags: map[string]bool{}, transforms: map[string]state{}}
		paths := map[string]string{}
		pathBytes := 0
		compactPath := func(path string) string {
			if replacement, ok := paths[path]; ok {
				return replacement
			}
			replacement := path
			if parts := drawingParts(path); parts != nil {
				out := make([]string, 0, len(parts))
				last := ""
				for _, part := range parts {
					if len(part) == 1 && strings.ContainsAny(part, "mnlb") {
						if part == last && (part == "l" || part == "b") {
							continue
						}
						last = part
					}
					out = append(out, part)
				}
				if short := strings.Join(out, " "); len(short) < len(path) {
					replacement = short
				}
			}
			if pathBytes+len(path)+len(replacement) < 16<<20 {
				paths[path] = replacement
				pathBytes += len(path) + len(replacement)
			}
			return replacement
		}
		for _, e := range l.events {
			f := e.fields
			// Actor names and automation labels do not affect rendering. Preserve
			// every recognized legacy scrolling effect, including malformed ones.
			effect := strings.ToLower(strings.TrimSpace(f[l.fields["effect"]]))
			if f[l.fields["name"]] != "" || effect != "" && !strings.HasPrefix(effect, "banner") && !strings.HasPrefix(effect, "scroll") {
				f = append([]string(nil), f...)
				f[l.fields["name"]] = ""
				if !strings.HasPrefix(effect, "banner") && !strings.HasPrefix(effect, "scroll") {
					f[l.fields["effect"]] = ""
				}
				replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
			}
			start, a := timestamp(strings.TrimSpace(f[l.fields["start"]]))
			end, b := timestamp(strings.TrimSpace(f[l.fields["end"]]))
			if a && b && end <= start {
				remove[e.row] = true
				continue
			}
			// Optional l/b commands are syntax, not approximate geometry. Keep
			// every coordinate and reject opaque tails and escape ambiguities.
			value := f[ti]
			if (drawingText.MatchString(value) || strings.Contains(value, `clip(`)) && !strings.Contains(value, `\{`) && !strings.Contains(value, `\}`) {
				if pieces, ok := c.scanText(value); ok {
					var out strings.Builder
					drawing := false
					for _, p := range pieces {
						if !p.block {
							if drawing {
								out.WriteString(compactPath(p.content))
							} else {
								out.WriteString(p.content)
							}
							continue
						}
						out.WriteByte('{')
						out.WriteString(p.prefix)
						for _, t := range p.tokens {
							raw := t.raw
							if m := searchDrawingMode.FindStringSubmatch(raw); m != nil {
								n, _ := strconv.Atoi(m[1])
								drawing = n > 0
							} else if strings.HasPrefix(raw, `\r`) || strings.HasPrefix(raw, `\p`) && !strings.HasPrefix(raw, `\pos(`) && !strings.HasPrefix(raw, `\pbo`) {
								drawing = false
							}
							if m := precisionClip.FindStringSubmatch(raw); m != nil {
								lead := m[1]
								if m[2] != "" {
									lead += m[2] + ","
								}
								raw = lead + compactPath(m[3]) + ")"
							}
							out.WriteString(raw)
						}
						out.WriteByte('}')
					}
					if out.Len() < len(value) {
						f = append([]string(nil), f...)
						f[ti] = out.String()
						replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
					}
				}
			}
			vi := l.fields["marginv"]
			if strings.TrimSpace(f[l.fields["effect"]]) != "" || len(f[vi]) <= 1 || !marginValue.MatchString(f[vi]) || !strings.Contains(f[ti], `\pos(`) {
				continue
			}
			pieces, ok := c.scanText(f[ti])
			if !ok {
				continue
			}
			position := ""
			for _, p := range pieces {
				for _, t := range p.tokens {
					if t.writes == nil && !c.transparent(t.raw) && !(strings.HasPrefix(t.raw, `\t(`) && c.transformWrites(t.raw) != nil) {
						ok = false
						break
					}
					if position == "" && (strings.HasPrefix(t.raw, `\pos(`) || strings.HasPrefix(t.raw, `\move(`)) {
						position = t.raw[:4]
					}
				}
				if !ok {
					break
				}
			}
			if ok && position == `\pos` {
				f = append([]string(nil), f...)
				f[vi] = "0"
				replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
			}
		}
	}
	var out strings.Builder
	for i, line := range l.lines {
		if remove[i] {
			continue
		}
		if replacement, ok := replace[i]; ok {
			line = replacement
		}
		out.WriteString(line)
	}
	return out.String()
}

func (l *layout) proposal(remove map[int]bool, replace map[int]string, kind string) proposal {
	var patches []edit
	for i, line := range l.lines {
		replacement, changed := replace[i]
		if remove[i] {
			replacement, changed = "", true
		}
		if changed && replacement != line {
			patches = append(patches, edit{l.offsets[i], l.offsets[i] + len(line), replacement, kind})
		}
	}
	return newProposal(l.doc, patches, kind)
}

// eventProposals tries explicit invisibility, then merges identical static runs.
func eventProposals(l *layout, yield func(proposal) bool) {
	if l == nil || l.fields == nil {
		return
	}
	// Batch candidates with an explicit terminal global alpha first; individual
	// removals remain available when another event depends on collision layout.
	remove := map[int]bool{}
	for _, e := range l.events {
		value := e.fields[l.fields["text"]]
		if strings.Count(value, "{") == 1 && strings.Contains(value, `\alpha&HFF&}`) && !strings.Contains(value, `\t(`) {
			remove[e.row] = true
		}
	}
	if len(remove) > 1 && !yield(l.proposal(remove, nil, "invisible_lines_batch")) {
		return
	}
	si, ei, ti, fi := l.fields["start"], l.fields["end"], l.fields["text"], l.fields["effect"]
	var groups [][]record
	var run []record
	for _, e := range l.events {
		start, a := timestamp(e.fields[si])
		end, b := timestamp(e.fields[ei])
		eligible := a && b && start < end && e.fields[fi] == "" && !staticAnimation.MatchString(e.fields[ti])
		if len(run) > 0 {
			previous := run[len(run)-1].fields
			finish, _ := timestamp(previous[ei])
			same := eligible && finish == start
			for i, v := range previous {
				if i != si && i != ei && v != e.fields[i] {
					same = false
				}
			}
			if !same {
				if len(run) > 1 {
					groups = append(groups, run)
				}
				run = nil
			}
		}
		if eligible {
			run = append(run, e)
		}
	}
	if len(run) > 1 {
		groups = append(groups, run)
	}
	if len(groups) == 0 {
		return
	}
	emit := func(batch [][]record) bool {
		remove, replace := map[int]bool{}, map[int]string{}
		for _, group := range batch {
			e := group[0]
			fields := append([]string(nil), e.fields...)
			fields[ei] = group[len(group)-1].fields[ei]
			replace[e.row] = e.prefix + strings.Join(fields, ",") + e.eol
			for _, e := range group[1:] {
				remove[e.row] = true
			}
		}
		return yield(l.proposal(remove, replace, "merge_lines_batch"))
	}
	if !emit(groups) {
		return
	}
	if len(groups) > 1 {
		for _, group := range groups {
			if !emit([][]record{group}) {
				return
			}
		}
	}
}

// rewriteStyleUsers retains text and record prefixes except explicit references.
func (l *layout) rewriteStyleUsers(mapping map[string]string, trimReset, trimStars bool) map[int]string {
	replace := map[int]string{}
	yi, ti := l.fields["style"], l.fields["text"]
	c := cleaner{tokens: map[string]token{}, transparentTags: map[string]bool{}, transforms: map[string]state{}}
	for _, e := range l.events {
		name := strings.TrimSpace(e.fields[yi])
		if trimStars {
			name = strings.TrimLeft(name, "*")
		}
		style, renamed := mapping[name]
		value := e.fields[ti]
		if strings.Contains(value, `\r`) {
			if pieces, ok := c.scanText(value); ok {
				var out strings.Builder
				for _, p := range pieces {
					if !p.block {
						out.WriteString(p.content)
						continue
					}
					out.WriteByte('{')
					out.WriteString(p.prefix)
					for _, t := range p.tokens {
						raw := t.raw
						if strings.HasPrefix(raw, `\r`) {
							name := raw[2:]
							if trimReset {
								name = strings.TrimRight(name, " \t")
							}
							if short, ok := mapping[name]; ok {
								raw = `\r` + short
							}
						}
						out.WriteString(raw)
					}
					out.WriteByte('}')
				}
				value = out.String()
			}
		}
		if renamed || value != e.fields[ti] {
			f := append([]string(nil), e.fields...)
			f[ti] = value
			if renamed {
				f[yi] = style
			}
			replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
		}
	}
	return replace
}

func structuralProposals(l *layout, yield func(proposal) bool) {
	if l == nil || l.fields == nil {
		return
	}
	fonts, unused := map[int]bool{}, map[int]bool{}
	section := ""
	for row, line := range l.lines {
		header := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(header, "[") {
			section = header
		}
		switch section {
		case "", "[script info]", "[v4+ styles]", "[events]":
		case "[fonts]":
			fonts[row] = true
		default:
			unused[row] = true
		}
	}
	if len(unused) > 0 && !yield(l.proposal(unused, nil, "unused_sections_remove")) {
		return
	}
	if len(fonts) > 0 && !yield(l.proposal(fonts, nil, "embedded_fonts_remove")) {
		return
	}
	replace := map[int]string{}
	for _, e := range l.events {
		if e.fields[l.fields["effect"]] != "" {
			f := append([]string(nil), e.fields...)
			f[l.fields["effect"]] = ""
			replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
		}
	}
	if len(replace) > 0 && !yield(l.proposal(nil, replace, "clear_event_effect")) {
		return
	}
	yi, ti := l.fields["style"], l.fields["text"]
	used := map[string]bool{"Default": true}
	if len(l.styles) > 0 {
		used[l.styles[0].fields[0]] = true
	}
	for _, e := range l.events {
		used[strings.TrimSpace(e.fields[yi])] = true
		for _, m := range resetReference.FindAllStringSubmatch(e.fields[ti], -1) {
			name := m[1]
			if i := strings.IndexAny(name, "()"); i >= 0 {
				name = name[:i]
			}
			used[strings.TrimRight(name, " \t")] = true
		}
	}
	unused = map[int]bool{}
	for _, s := range l.styles {
		if !used[s.fields[0]] {
			unused[s.row] = true
		}
	}
	if len(unused) > 1 && !yield(l.proposal(unused, nil, "unused_styles_batch")) {
		return
	}
	for i := len(l.styles) - 1; i >= 0; i-- {
		s := l.styles[i]
		if unused[s.row] && !yield(l.proposal(map[int]bool{s.row: true}, nil, "unused_style_remove")) {
			return
		}
	}
	type alias struct {
		row          int
		name, target string
	}
	definitions := map[string]string{}
	var aliases []alias
	for _, s := range l.styles {
		key := strings.Join(s.fields[1:], ",")
		target, ok := definitions[key]
		if !ok {
			definitions[key] = s.fields[0]
		} else if target != s.fields[0] {
			aliases = append(aliases, alias{s.row, s.fields[0], target})
		}
	}
	emitAliases := func(group []alias) bool {
		mapping := map[string]string{}
		remove := map[int]bool{}
		for _, a := range group {
			mapping[a.name] = a.target
			remove[a.row] = true
		}
		return yield(l.proposal(remove, l.rewriteStyleUsers(mapping, false, false), "style_alias"))
	}
	if len(aliases) > 1 && !emitAliases(aliases) {
		return
	}
	for _, a := range aliases {
		if !emitAliases([]alias{a}) {
			return
		}
	}
	var invisible []int
	for _, e := range l.events {
		value := e.fields[ti]
		if strings.TrimSpace(overrideBlock.ReplaceAllString(value, "")) == "" || hiddenAlpha.MatchString(value) {
			invisible = append(invisible, e.row)
		}
	}
	for _, row := range invisible {
		if !yield(l.proposal(map[int]bool{row: true}, nil, "invisible_line")) {
			return
		}
	}
}

func shortStyleNames(l *layout) proposal {
	if l == nil || l.fields == nil {
		return proposal{}
	}
	occupied, unique := map[string]bool{}, map[string]bool{}
	for _, s := range l.styles {
		name := s.fields[0]
		if unique[name] {
			return proposal{}
		}
		unique[name] = true
		occupied[strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "*"))] = true
	}
	counts := map[string]int{}
	for _, e := range l.events {
		name := strings.TrimLeft(strings.TrimSpace(e.fields[l.fields["style"]]), "*")
		counts[name]++
		occupied[strings.ToLower(name)] = true
		for _, m := range resetReference.FindAllStringSubmatch(e.fields[l.fields["text"]], -1) {
			occupied[strings.ToLower(strings.TrimLeft(strings.TrimSpace(m[1]), "*"))] = true
		}
	}
	mapping := map[string]string{}
	serial := 0
	for i, s := range l.styles {
		name := s.fields[0]
		if i == 0 || strings.EqualFold(name, "default") || len(name) < 3 || counts[name] < 2 {
			continue
		}
		short := ""
		for {
			short = string(rune('a' + serial%26))
			if serial >= 26 {
				short += strconv.Itoa(serial / 26)
			}
			serial++
			if !occupied[short] {
				break
			}
		}
		if (len(name)-len(short))*counts[name] < 64 {
			continue
		}
		mapping[name] = short
		occupied[short] = true
	}
	if len(mapping) == 0 {
		return proposal{}
	}
	replace := l.rewriteStyleUsers(mapping, true, true)
	for _, s := range l.styles {
		if short, ok := mapping[s.fields[0]]; ok {
			line := l.lines[s.row]
			colon := strings.IndexByte(line, ':') + 1
			start := colon + len(line[colon:]) - len(strings.TrimLeft(line[colon:], " \t"))
			replace[s.row] = line[:start] + short + line[start+strings.IndexByte(line[start:], ','):]
		}
	}
	return l.proposal(nil, replace, "style_names")
}

// factorStyles shares repeated initial overrides through ordinary ASS styles.
// It is an O2 proposal, not a semantic fast path: the common renderer guard
// checks resets, font metrics, transforms and collision behavior before commit.
func factorStyles(l *layout, yield func(proposal) bool) bool {
	if l == nil || l.fields == nil || !slices.Equal(l.styleFields, styleFields) || len(l.styles) == 0 {
		return true
	}
	columns := map[string]int{}
	for i, name := range styleFields {
		columns[name] = i
	}
	targets := map[string]string{"fs": "fontsize", "fscx": "scalex", "fscy": "scaley", "fsp": "spacing", "frz": "angle", "bord": "outline", "shad": "shadow", "b": "bold", "i": "italic", "u": "underline", "s": "strikeout"}
	colors := map[string]string{"c": "primarycolour", "2c": "secondarycolour", "3c": "outlinecolour", "4c": "backcolour",
		"1a": "primarycolour", "2a": "secondarycolour", "3a": "outlinecolour", "4a": "backcolour"}
	originals := map[string][]string{}
	occupied := map[string]bool{}
	for _, st := range l.styles {
		if _, exists := originals[st.fields[0]]; exists {
			return true
		}
		originals[st.fields[0]] = st.fields
		occupied[strings.ToLower(st.fields[0])] = true
	}

	for _, e := range l.events {
		occupied[strings.ToLower(strings.TrimLeft(strings.TrimSpace(e.fields[l.fields["style"]]), "*"))] = true
		for _, m := range resetReference.FindAllStringSubmatch(e.fields[l.fields["text"]], -1) {
			occupied[strings.ToLower(strings.TrimSpace(m[1]))] = true
		}
	}
	type group struct {
		fields  []string
		replace map[int]string
		saving  int
	}
	type candidate struct {
		groups []*group
		saving int
	}
	var candidates []candidate
	last := l.styles[len(l.styles)-1].row
	eol := "\n"
	if strings.HasSuffix(l.lines[last], "\r\n") {
		eol = "\r\n"
	}
	reserved := occupied
	alphaSeen := false
	c := cleaner{tokens: map[string]token{}}
	for _, factorAlpha := range []bool{false, true} {
		occupied := maps.Clone(reserved)
		groups := []*group{}
		byValue := map[string]*group{}
		yi, ti := l.fields["style"], l.fields["text"]
		serial := 0
		for _, e := range l.events {
			text := e.fields[ti]
			base := originals[strings.TrimSpace(e.fields[yi])]
			end := strings.IndexByte(text, '}')
			// Unqualified resets refer to the event's base style, so leave such events.
			if base == nil || !strings.HasPrefix(text, "{") || end < 0 || strings.Contains(text, `\r`) {
				continue
			}
			prefix, tokens, ok := c.splitTokens(text[1:end])
			if !ok || prefix != "" {
				continue
			}
			fields := append([]string(nil), base...)
			var kept strings.Builder
			changed, stopped := false, false
			for _, t := range tokens {
				raw := t.raw
				if strings.HasPrefix(raw, `\t(`) {
					stopped = true
				}
				field, value := "", ""
				color := colorTag.MatchString(raw)
				alpha := color && (strings.HasPrefix(raw, `\alpha`) || raw[2] == 'a')
				alphaSeen = alphaSeen || alpha
				if !stopped && t.writes != nil {
					if strings.HasPrefix(raw, `\fn`) && !strings.Contains(raw, ",") {
						field, value = "fontname", raw[3:]
					} else if color && (factorAlpha || !alpha) {
						updates := map[int]string{}
						for channel, value := range t.writes {
							column := columns[colors[channel]]
							old := styleColor.FindStringSubmatch(fields[column])
							if old == nil {
								updates = nil
								break
							}
							base, _ := strconv.ParseUint(old[1], 16, 32)
							color, mask := uint64(value.(float64)), uint64(0xff000000)
							if strings.HasSuffix(channel, "a") {
								color, mask = color<<24, 0xffffff
							}
							updates[column] = fmt.Sprintf("&H%08X", base&mask|color)
						}
						if len(updates) > 0 {
							for column, value := range updates {
								fields[column] = value
							}
							changed = true
							continue
						}
					} else if m := simpleTag.FindStringSubmatch(raw); m != nil {
						field, value = targets[m[1]], number(m[2])
						if strings.Contains("bius", m[1]) && len(m[1]) == 1 {
							if value == "1" {
								value = "-1"
							} else if value != "0" {
								field = ""
							}
						}
					}
				}
				if field == "" {
					kept.WriteString(raw)
					continue
				}
				fields[columns[field]] = value
				changed = true
			}
			if !changed {
				continue
			}
			key := strings.Join(fields[1:], ",")
			g := byValue[key]
			if g == nil {
				name := ""
				for name == "" || occupied[strings.ToLower(name)] {
					name = "s" + strconv.FormatInt(int64(serial), 36)
					serial++
				}
				occupied[strings.ToLower(name)] = true
				fields[0] = name
				g = &group{fields: fields, replace: map[int]string{}}
				byValue[key] = g
				groups = append(groups, g)
			}
			f := append([]string(nil), e.fields...)
			f[yi] = g.fields[0]
			f[ti] = text[end+1:]
			if kept.Len() > 0 {
				f[ti] = "{" + kept.String() + "}" + f[ti]
			}
			g.replace[e.row] = e.prefix + strings.Join(f, ",") + e.eol
		}
		var useful []*group
		total := 0
		for _, g := range groups {
			saving := -l.doc.encodedLen("Style: " + strings.Join(g.fields, ",") + eol)
			for row, line := range g.replace {
				saving += l.doc.encodedLen(l.lines[row]) - l.doc.encodedLen(line)
			}
			if len(g.replace) > 1 && saving > 0 {
				g.saving = saving
				total += saving
				useful = append(useful, g)
			}
		}
		if len(useful) > 1 {
			candidates = append(candidates, candidate{useful, total})
		}
		for _, g := range useful {
			candidates = append(candidates, candidate{[]*group{g}, g.saving})
		}
		if !alphaSeen {
			break
		}
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return b.saving - a.saving })
	for _, p := range candidates {
		replace := map[int]string{}
		var definitions strings.Builder
		definitions.WriteString(l.lines[last])
		for _, g := range p.groups {
			definitions.WriteString("Style: " + strings.Join(g.fields, ",") + eol)
			maps.Copy(replace, g.replace)
		}
		replace[last] = definitions.String()
		if !yield(l.proposal(nil, replace, "style_factor")) {
			return false
		}
	}
	return true
}
