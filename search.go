package subtrim

import (
	"context"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"subtrim/native"
)

var polygonSyntax = regexp.MustCompile(`^[ml0-9\s+-]+$`)
var polygonInteger = regexp.MustCompile(`^[+-]?[0-9]{1,7}$`)
var polygonClip = regexp.MustCompile(`^(\\i?clip\()([0-9]{1,3},)?([ml][^()\\{}]*)\)$`)
var genericNumber = regexp.MustCompile(`-?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)`)
var genericDrawing = regexp.MustCompile(`^[mnlbspc0-9.\-\s]+$`)
var contourStart = regexp.MustCompile(`(?:^|\s)[mn]\s`)
var searchDrawingMode = regexp.MustCompile(`^\\p([0-9]{1,3})$`)

func polygonProposals(value string) []textProposal {
	if !polygonSyntax.MatchString(value) {
		return nil
	}
	type point [2]int64
	tokens := strings.Fields(value)
	var contours [][]point
	for i := 0; i < len(tokens); {
		command := tokens[i]
		i++
		if command != "m" && command != "l" {
			return nil
		}
		var coords []int64
		for i < len(tokens) && tokens[i] != "m" && tokens[i] != "l" {
			if !polygonInteger.MatchString(tokens[i]) {
				return nil
			}
			v, _ := strconv.ParseInt(tokens[i], 10, 64)
			if v < -1000000 || v > 1000000 {
				return nil
			}
			coords = append(coords, v)
			i++
		}
		if len(coords) < 2 || len(coords)%2 != 0 {
			return nil
		}
		if command == "m" {
			if len(coords) != 2 {
				return nil
			}
			contours = append(contours, []point{{coords[0], coords[1]}})
		} else {
			if len(contours) == 0 {
				return nil
			}
			for j := 0; j < len(coords); j += 2 {
				n := len(contours) - 1
				contours[n] = append(contours[n], point{coords[j], coords[j+1]})
			}
		}
	}
	if len(contours) == 0 {
		return nil
	}
	for _, path := range contours {
		if len(path) < 3 {
			return nil
		}
	}
	encode := func(paths [][]point) string {
		var out []string
		for _, path := range paths {
			out = append(out, "m", strconv.FormatInt(path[0][0], 10), strconv.FormatInt(path[0][1], 10), "l")
			for _, p := range path[1:] {
				out = append(out, strconv.FormatInt(p[0], 10), strconv.FormatInt(p[1], 10))
			}
		}
		return strings.Join(out, " ")
	}
	var simplified [][]point
	for _, path := range contours {
		var kept []point
		for _, p := range path {
			if len(kept) > 0 && p == kept[len(kept)-1] {
				continue
			}
			for len(kept) > 1 {
				a, b := kept[len(kept)-2], kept[len(kept)-1]
				ab, bc := point{b[0] - a[0], b[1] - a[1]}, point{p[0] - b[0], p[1] - b[1]}
				if ab[0]*bc[1] != ab[1]*bc[0] || ab[0]*bc[0]+ab[1]*bc[1] < 0 {
					break
				}
				kept = kept[:len(kept)-1]
			}
			kept = append(kept, p)
		}
		if len(kept) < 3 {
			kept = path
		}
		simplified = append(simplified, kept)
	}
	var result []textProposal
	if s := encode(simplified); len(s) < len(value) {
		result = append(result, textProposal{s, "polygon_segments"})
	}
	seen := map[string]bool{}
	var duplicates []int
	for i, path := range contours {
		key := encode([][]point{path})
		if seen[key] {
			duplicates = append(duplicates, i)
		}
		seen[key] = true
	}
	if len(duplicates) > 0 {
		groups := [][]int{duplicates}
		if len(duplicates) > 1 {
			for _, i := range duplicates[:min(8, len(duplicates))] {
				groups = append(groups, []int{i})
			}
		}
		for _, group := range groups {
			var paths [][]point
			for i, path := range contours {
				if !slices.Contains(group, i) {
					paths = append(paths, path)
				}
			}
			if s := encode(paths); len(s) < len(value) {
				result = append(result, textProposal{s, "coincident_contours"})
			}
		}
	}
	return result
}

func (c *cleaner) eventEdits(value, tier string, initial state, final *styleTracker) []edit {
	var edits []edit
	add := func(start, end int, text, kind string) {
		if end-start > len(text) {
			edits = append(edits, edit{start, end, text, kind})
		}
	}
	if tier == "state" || tier == "cleanup" {
		var proposals []textProposal
		if tier == "state" {
			proposals = c.stateProposals(value, initial, final)
		} else {
			proposals = c.cleanupProposals(value)
		}
		for _, p := range proposals {
			add(0, len(value), p.text, p.kind)
		}
		return edits
	}
	pieces, ok := c.scanText(value)
	if !ok {
		return nil
	}
	cursor := 0
	drawing := false
	contours := func(s string) []interval {
		matches := contourStart.FindAllStringIndex(s, -1)
		if len(matches) < 2 {
			return nil
		}
		var result []interval
		for i, m := range matches {
			start := m[0]
			if s[start] != 'm' && s[start] != 'n' {
				start++
			}
			end := len(s)
			if i+1 < len(matches) {
				end = matches[i+1][0]
				if s[end] != 'm' && s[end] != 'n' {
					end++
				}
			}
			result = append(result, interval{int64(start), int64(end)})
		}
		return result
	}
	short := func(s string) string {
		return genericNumber.ReplaceAllStringFunc(s, func(v string) string {
			if len(v) <= 32 {
				return number(v)
			}
			return v
		})
	}
	for _, p := range pieces {
		width := len(p.content)
		if p.block {
			width += 2
			offset := cursor + 1 + len(p.prefix)
			if tier == "generic" {
				for i, t := range p.tokens {
					for _, g := range []struct {
						fields []string
						tag    string
					}{{[]string{"xbord", "ybord"}, "bord"}, {[]string{"xshad", "yshad"}, "shad"}, {[]string{"1a", "2a", "3a", "4a"}, "alpha"}} {
						if i+len(g.fields) > len(p.tokens) {
							continue
						}
						values := state{}
						width := 0
						valid := true
						for _, child := range p.tokens[i : i+len(g.fields)] {
							if len(child.writes) != 1 {
								valid = false
								break
							}
							for k, v := range child.writes {
								values[k] = v
							}
							width += len(child.raw)
						}
						var value any
						if valid && len(values) == len(g.fields) {
							for j, k := range g.fields {
								v, ok := values[k]
								if !ok || j > 0 && v != value {
									valid = false
									break
								}
								value = v
							}
						} else {
							valid = false
						}
						if !valid {
							continue
						}
						v, ok := value.(float64)
						if !ok || v < 0 {
							continue
						}
						argument := number(strconv.FormatFloat(v, 'f', -1, 64))
						if g.tag == "alpha" {
							argument = fmt.Sprintf("&H%X&", int(v))
						}
						add(offset, offset+width, `\`+g.tag+argument, "combined_axes_or_alpha")
					}
					r := t.raw
					replacement := ""
					switch r {
					case `\b700`:
						replacement = `\b1`
					case `\b400`:
						replacement = `\b0`
					}
					if strings.HasPrefix(r, `\move(`) && strings.HasSuffix(r, ")") {
						a := strings.Split(r[6:len(r)-1], ",")
						if len(a) >= 4 && a[0] == a[2] && a[1] == a[3] {
							replacement = `\pos(` + a[0] + "," + a[1] + ")"
						}
					}
					if strings.HasPrefix(r, `\t(1,\`) {
						replacement = `\t(` + r[5:]
					}
					if strings.HasPrefix(r, `\kf`) && len(r) > 3 && r[3] >= '0' && r[3] <= '9' {
						replacement = `\K` + r[3:]
					}
					for _, head := range []string{`\clip(1,`, `\iclip(1,`} {
						if strings.HasPrefix(r, head) && len(r) > len(head)+1 && strings.ContainsRune("mn", rune(r[len(head)])) && r[len(head)+1] == ' ' {
							replacement = head[:len(head)-2] + r[len(head):]
						}
					}
					if replacement != "" {
						add(offset, offset+len(r), replacement, "tag_contraction")
					}
					if strings.HasPrefix(r, `\t(`) && strings.HasSuffix(r, ")") {
						inner := r[3 : len(r)-1]
						pos := strings.IndexByte(inner, '\\')
						if pos >= 0 {
							_, children, ok := c.splitTokens(inner[pos:])
							if ok {
								start := 0
								for _, child := range children {
									childStart := offset + 3 + pos + start
									add(childStart, childStart+len(child.raw), "", "transform_child")
									start += len(child.raw)
								}
							}
						}
					}
					add(offset, offset+len(r), "", "tag_delete")
					offset += len(r)
				}
				offset = cursor + 1 + len(p.prefix)
			}
			for _, t := range p.tokens {
				if strings.HasPrefix(t.raw, `\p`) {
					if m := searchDrawingMode.FindStringSubmatch(t.raw); m != nil {
						n, _ := strconv.Atoi(m[1])
						drawing = n > 0
					} else if !strings.HasPrefix(t.raw, `\pos`) && !strings.HasPrefix(t.raw, `\pbo`) {
						drawing = false
					}
				} else if strings.HasPrefix(t.raw, `\r`) {
					drawing = false
				}
				if tier == "geometry" {
					if m := polygonClip.FindStringSubmatch(t.raw); m != nil {
						for _, q := range polygonProposals(m[3]) {
							add(offset, offset+len(t.raw), m[1]+m[2]+q.text+")", q.kind)
						}
					}
				} else {
					if m := transparentComplex.FindStringSubmatch(t.raw); m != nil && (m[1] == "pos" || m[1] == "move" || m[1] == "org" || m[1] == "clip" || m[1] == "iclip") {
						add(offset, offset+len(t.raw), short(t.raw), "coordinate_spelling")
						if m[1] == "clip" || m[1] == "iclip" {
							payload, lead := m[2], ""
							if scale, rest, ok := strings.Cut(payload, ","); ok && unsignedPattern.MatchString(scale) {
								lead, payload = scale+",", rest
							}
							for _, span := range contours(payload) {
								start := offset + len(m[1]) + 2 + len(lead)
								add(start+int(span.start), start+int(span.end), "", "clip_contour")
							}
						}
					}
				}
				offset += len(t.raw)
			}
			if tier == "generic" {
				add(cursor, cursor+width, "", "block")
				if p.prefix != "" {
					add(cursor+1, cursor+1+len(p.prefix), "", "prefix")
				}
			}
		} else if drawing {
			if tier == "geometry" {
				for _, q := range polygonProposals(p.content) {
					add(cursor, cursor+width, q.text, q.kind)
				}
			} else if genericDrawing.MatchString(p.content) {
				add(cursor, cursor+width, short(p.content), "drawing_spelling")
				for _, span := range contours(p.content) {
					add(cursor+int(span.start), cursor+int(span.end), "", "drawing_contour")
				}
			}
		}
		cursor += width
	}
	if tier == "generic" {
		for start := 0; ; {
			i := strings.Index(value[start:], "}{")
			if i < 0 {
				break
			}
			i += start
			add(i, i+2, "", "join_blocks")
			start = i + 2
		}
	}
	return edits
}

// Inline analysis produces patches; only the selected trial is materialized.
// Analysis is file-local; a structural acceptance starts a fresh analysis.
type analyzedEvent struct {
	text  string
	edits []edit
}

type searchInput struct {
	doc      document
	spans    []dialogueSpan
	layout   *layout
	initials []state
	styles   *styleTracker
	cleaner  cleaner
	events   []native.Event
}

func prepareSearch(doc document) *searchInput {
	spans := dialogueSpans(doc.text)
	final, tracker := newStyleTracker(), newStyleTracker()
	lines := strings.SplitAfter(doc.text, "\n")
	for _, line := range lines {
		final.feed(strings.TrimRight(line, "\r\n"))
	}
	initials := make([]state, len(spans))
	offset, index, styleIndex := 0, 0, -1
	for _, line := range lines {
		tracker.feed(strings.TrimRight(line, "\r\n"))
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "Format:") && tracker.section == "events" {
			for i, v := range strings.Split(s[7:], ",") {
				if strings.EqualFold(strings.TrimSpace(v), "style") {
					styleIndex = i
				}
			}
		}
		if index < len(spans) && spans[index].start >= offset && spans[index].start < offset+len(line) {
			if styleIndex >= 0 {
				_, body, _ := strings.Cut(line, ":")
				fields := strings.SplitN(body, ",", 10)
				if styleIndex < len(fields) {
					initials[index] = tracker.initial(fields[styleIndex])
				}
			}
			if initials[index] == nil {
				initials[index] = state{}
			}
			index++
		}
		offset += len(line)
	}
	c := cleaner{tokens: map[string]token{}, transparentTags: map[string]bool{}, transforms: map[string]state{}}
	return &searchInput{doc, spans, layoutRecords(doc), initials, final, c, nil}
}

func inlineProposals(ctx context.Context, input *searchInput, tier string, cache map[string][]analyzedEvent, batch *int, wholeTrack bool, yield func(proposal) bool) {
	doc, spans, initials, final, c := input.doc, input.spans, input.initials, input.styles, &input.cleaner
	seen := map[string]bool{}
	emit := func(group []edit) bool {
		if len(group) == 0 {
			return true
		}
		group = append([]edit(nil), group...)
		slices.SortStableFunc(group, func(a, b edit) int { return a.start - b.start })
		var key strings.Builder
		for _, e := range group {
			key.WriteString(strconv.Itoa(e.start) + ":" + strconv.Itoa(e.end) + ":" + strconv.Itoa(len(e.text)) + ":" + e.text)
		}
		if seen[key.String()] {
			return true
		}
		seen[key.String()] = true
		return yield(newProposal(doc, group, "inline_"+group[0].kind))
	}
	if len(cache[tier]) != len(spans) {
		cache[tier] = make([]analyzedEvent, len(spans))
	}
	var pending []edit
	var windowStart, windowEnd int64
	work := 0.0
	setup := 0.002 + float64(len(doc.text))/100000000
	discoveryStarted := time.Now()
	for i, span := range spans {
		if i%32 == 0 && time.Since(discoveryStarted) >= 2*time.Millisecond {
			// Yield CPU without turning a scheduling quantum into a tiny batch.
			if !yield(proposal{}) {
				return
			}
			discoveryStarted = time.Now()
		}
		if ctx.Err() != nil {
			return
		}
		value := doc.text[span.start:span.end]
		cached := &cache[tier][i]
		if cached.text != value {
			cached.text = strings.Clone(value)
			cached.edits = c.eventEdits(value, tier, initials[i], final)
			for j := range cached.edits {
				cached.edits[j].text = strings.Clone(cached.edits[j].text)
			}
		}
		edits := append([]edit(nil), cached.edits...)
		if len(edits) == 0 {
			continue
		}
		for j := range edits {
			edits[j].start += span.start
			edits[j].end += span.start
		}
		best := edits[0]
		for _, e := range edits[1:] {
			if e.end-e.start-len(e.text) > best.end-best.start-len(best.text) {
				best = e
			}
		}
		if tier == "state" || tier == "geometry" {
			if tier == "state" {
				for j, e := range edits {
					original := doc.text[e.start:e.end]
					left, right, limit := 0, 0, min(len(original), len(e.text))
					for left < limit && original[left] == e.text[left] {
						left++
					}
					for right < limit-left && original[len(original)-1-right] == e.text[len(e.text)-1-right] {
						right++
					}
					edits[j] = edit{e.start + left, e.end - right, e.text[left : len(e.text)-right], e.kind}
				}
			}
			slices.SortStableFunc(edits, func(a, b edit) int { return (b.end - b.start - len(b.text)) - (a.end - a.start - len(a.text)) })
			var group []edit
			for _, e := range edits {
				disjoint := true
				for _, g := range group {
					if e.end > g.start && g.end > e.start {
						disjoint = false
						break
					}
				}
				if disjoint {
					group = append(group, e)
				}
			}
			if len(group) > 1 {
				slices.SortFunc(group, func(a, b edit) int { return a.start - b.start })
				var out strings.Builder
				cursor := group[0].start
				for _, e := range group {
					out.WriteString(doc.text[cursor:e.start])
					out.WriteString(e.text)
					cursor = e.end
				}
				best = edit{group[0].start, group[len(group)-1].end, out.String(), "composed_" + tier}
			}
		}

		pending = append(pending, best)
		added := 1.0
		if i < len(input.events) {
			e := input.events[i]
			end := e.Start + e.Duration
			added = eventWork(e)
			if len(pending) == 1 {
				windowStart, windowEnd = e.Start, end
			} else {
				extension := max(int64(0), windowStart-e.Start) + max(int64(0), end-windowEnd)
				added *= math.Min(1, float64(extension)/float64(max(int64(1), e.Duration)))
				windowStart, windowEnd = min(windowStart, e.Start), max(windowEnd, end)
			}
		}
		work += added
		// An exhaustive guard renders the same whole track for every group;
		// adding another edit then adds no validation scope.
		if !wholeTrack && work*0.00002 >= setup*float64(*batch) || len(pending) >= 1024 {
			if !emit(pending) {
				return
			}
			pending = nil
			work = 0
		}
	}
	if !emit(pending) {
		return
	}
	// Alternatives are discovered lazily after the best edit for each event.
	for i, span := range spans {
		if ctx.Err() != nil {
			return
		}
		for _, e := range cache[tier][i].edits {
			e.start += span.start
			e.end += span.start
			if !emit([]edit{e}) {
				return
			}
		}
	}
}

func eventWork(e native.Event) float64 {
	return float64(1+len(e.Text)/128) * math.Max(1, math.Min(float64(e.Duration)/100, 10))
}

type textProposal struct{ text, kind string }
type edit struct {
	start, end int
	text, kind string
}

type proposal struct {
	patches []edit
	kind    string
	saving  int
	step    float64
}

func newProposal(doc document, patches []edit, kind string) proposal {
	saving := 0
	for _, e := range patches {
		saving += doc.encodedLen(doc.text[e.start:e.end]) - doc.encodedLen(e.text)
	}
	return proposal{patches: patches, kind: kind, saving: saving}
}

func materialize(text string, edits []edit) string {
	slices.SortStableFunc(edits, func(a, b edit) int { return a.start - b.start })
	var out strings.Builder
	cursor := 0
	for _, e := range edits {
		out.WriteString(text[cursor:e.start])
		out.WriteString(e.text)
		cursor = e.end
	}
	out.WriteString(text[cursor:])
	return out.String()
}

var speculativeScalar = regexp.MustCompile(`^\\[ \t]*(xbord|ybord|xshad|yshad|fscx|fscy|blur|bord|shad|frx|fry|frz|fsp|fax|fay|fs|fr|be|b|i|u|s|an|a|q|fe|pbo|p|kt|kf|ko|k|K)[ \t]*(` + signed + `)[ \t]*$`)
var emptyParentheses = regexp.MustCompile(`^\\(?:t|clip|iclip)\([ \t]*\)$`)
var inertTag = regexp.MustCompile(`^\\(?:t\([0-9., \t]*\)|[A-Za-z]+\(\))$`)
var relativeSize = regexp.MustCompile(`^\\fs([+-][0-9]{1,2})$`)
var absoluteSize = regexp.MustCompile(`^\\fs([1-9][0-9]{0,5})$`)
var globalDuplicate = regexp.MustCompile(`^\\(pos|move|org|fad|fade|an|a|q|clip|iclip)[(0-9-]`)
var simpleTransform = regexp.MustCompile(`^\\t\(([^\\()]*)(\\.*)\)$`)
var linebreakBlocks = regexp.MustCompile(`\{(\\[^{}]*)\}([ \t]*\\N[ \t]*)\{(\\[^{}]*)\}`)
var trailingBlocks = regexp.MustCompile(`(?:\{[^{}]*\})+$`)
var repeatedSpaces = regexp.MustCompile(` {2,}`)

func (c *cleaner) blockProposals(content string) []textProposal {
	prefix, tokens, ok := c.splitTokens(content)
	if !ok {
		return nil
	}
	raws := make([]string, len(tokens))
	for i, t := range tokens {
		raws[i] = t.raw
	}
	var result []textProposal
	emit := func(value, kind string) {
		if len(value) < len(content) {
			result = append(result, textProposal{value, kind})
		}
	}
	remove := func(indices map[int]bool, kind string) {
		var b strings.Builder
		b.WriteString(prefix)
		for i, r := range raws {
			if !indices[i] {
				b.WriteString(r)
			}
		}
		emit(b.String(), kind)
	}

	empty, inert := map[int]bool{}, map[int]bool{}
	for i, r := range raws {
		if emptyParentheses.MatchString(r) {
			empty[i] = true
		}
		if r == `\fad(0,0)` || r == `\frx0` || r == `\fry0` || inertTag.MatchString(r) {
			inert[i] = true
		}
	}
	if len(empty) > 0 {
		remove(empty, "empty_parentheses")
		if len(empty) > 1 {
			for i := range raws {
				if empty[i] {
					remove(map[int]bool{i: true}, "empty_parentheses_single")
				}
			}
		}
	}
	var normalized strings.Builder
	normalized.WriteString(prefix)
	for _, raw := range raws {
		replacement := raw
		if m := speculativeScalar.FindStringSubmatch(raw); m != nil && len(m[2]) <= 32 {
			if m[1] == "fr" {
				m[1] = "frz"
			}
			replacement = `\` + m[1] + number(m[2])
		} else if m := transparentComplex.FindStringSubmatch(raw); m != nil {
			args := strings.Split(m[2], ",")
			valid := true
			for i, arg := range args {
				if len(arg) > 32 || !numberPattern.MatchString(strings.Trim(arg, " \t")) {
					valid = false
					break
				}
				args[i] = number(strings.Trim(arg, " \t"))
			}
			if valid {
				replacement = `\` + m[1] + "(" + strings.Join(args, ",") + ")"
			}
		}
		normalized.WriteString(replacement)
	}
	emit(normalized.String(), "numeric_spelling")
	for i, r := range raws {
		if m := absoluteSize.FindStringSubmatch(r); m != nil {
			size, _ := new(big.Rat).SetString(m[1])
			for j := i + 1; j < len(raws); j++ {
				m := relativeSize.FindStringSubmatch(raws[j])
				if m == nil {
					break
				}
				delta, _ := strconv.ParseInt(m[1], 10, 64)
				size.Mul(size, big.NewRat(10+delta, 10))
				if size.Sign() <= 0 || size.Cmp(big.NewRat(1000000, 1)) > 0 {
					break
				}
				if size.IsInt() {
					emit(prefix+strings.Join(raws[:i], "")+`\fs`+size.Num().String()+strings.Join(raws[j+1:], ""), "relative_font_chain")
				}
			}
		}
		if i > 0 && resetTag.MatchString(r) {
			emit(prefix+strings.Join(raws[i:], ""), "reset_prefix")
		}
	}
	seen := map[string]int{}
	for i, r := range raws {
		if m := globalDuplicate.FindStringSubmatch(r); m != nil {
			key := m[1]
			if key == "pos" || key == "move" {
				key = "position"
			}
			if previous, ok := seen[key]; ok {
				remove(map[int]bool{i: true}, "duplicate_global")
				remove(map[int]bool{previous: true}, "duplicate_global")
			}
			seen[key] = i
		}
	}
	for i := 0; i+1 < len(raws); i++ {
		a, b := simpleTransform.FindStringSubmatch(raws[i]), simpleTransform.FindStringSubmatch(raws[i+1])
		if a != nil && b != nil && a[1] == b[1] && !strings.Contains(a[2]+b[2], "(") {
			emit(prefix+strings.Join(raws[:i], "")+`\t(`+a[1]+a[2]+b[2]+")"+strings.Join(raws[i+2:], ""), "merge_transforms")
		}
	}
	var rest, transforms strings.Builder
	count := 0
	for _, r := range raws {
		if strings.HasPrefix(r, `\t(\`) && strings.HasSuffix(r, ")") && !strings.ContainsAny(r[3:len(r)-1], "()") {
			transforms.WriteString(r[3 : len(r)-1])
			count++
		} else {
			rest.WriteString(r)
		}
	}
	if count > 1 {
		emit(prefix+rest.String()+`\t(`+transforms.String()+")", "collect_transforms")
	}
	if len(inert) > 0 {
		remove(inert, "inert_tags")
		if len(inert) > 1 {
			for i := range raws {
				if inert[i] && !empty[i] {
					remove(map[int]bool{i: true}, "inert_tag_single")
				}
			}
		}
	}
	return result
}

func (c *cleaner) cleanupProposals(value string) []textProposal {
	pieces, ok := c.scanText(value)
	if !ok {
		return nil
	}
	var result []textProposal
	emit := func(text, kind string) {
		if len(text) < len(value) {
			result = append(result, textProposal{text, kind})
		}
	}
	fadeSeen, changed, certain := false, false, true
	var rebuilt strings.Builder
	for _, p := range pieces {
		if !p.block {
			rebuilt.WriteString(p.content)
			continue
		}
		rebuilt.WriteByte('{')
		rebuilt.WriteString(p.prefix)
		for _, t := range p.tokens {
			if strings.HasPrefix(t.raw, `\fad(`) || strings.HasPrefix(t.raw, `\fade(`) {
				m := transparentComplex.FindStringSubmatch(t.raw)
				valid := m != nil
				if valid {
					args := strings.Split(m[2], ",")
					valid = len(args) == 2 || len(args) == 7
					for _, a := range args {
						valid = valid && numberPattern.MatchString(strings.TrimSpace(a))
					}
				}
				if !valid {
					certain = false
					break
				}
				if fadeSeen {
					changed = true
					continue
				}
				fadeSeen = true
			} else if t.writes == nil && !c.transparent(t.raw) {
				certain = false
				break
			}
			rebuilt.WriteString(t.raw)
		}
		rebuilt.WriteByte('}')
		if !certain {
			break
		}
	}
	if certain && changed {
		emit(rebuilt.String(), "first_wins_fades")
	}
	emit(strings.ReplaceAll(value, "}{", ""), "adjacent_blocks_batch")
	cursor := 0
	for _, p := range pieces {
		width := len(p.content)
		if p.block {
			width += 2
			for _, q := range c.blockProposals(p.content) {
				emit(value[:cursor]+"{"+q.text+"}"+value[cursor+width:], q.kind)
			}
		}
		cursor += width
	}
	for _, m := range linebreakBlocks.FindAllStringSubmatchIndex(value, -1) {
		emit(value[:m[0]]+value[m[4]:m[5]]+"{"+value[m[2]:m[3]]+value[m[6]:m[7]]+"}"+value[m[1]:], "linebreak_blocks")
	}
	if m := trailingBlocks.FindStringIndex(value); m != nil {
		emit(value[:m[0]], "trailing_blocks")
	}
	if strings.HasPrefix(value, `{\r}`) {
		emit(value[4:], "initial_reset")
	}
	emit(strings.Trim(value, " \t"), "edge_spaces")
	rebuilt.Reset()
	for _, p := range pieces {
		if p.block {
			rebuilt.WriteString("{" + p.content + "}")
		} else {
			rebuilt.WriteString(repeatedSpaces.ReplaceAllString(p.content, " "))
		}
	}
	emit(rebuilt.String(), "repeated_spaces")
	return result
}

var positionedTag = regexp.MustCompile(`^\\(?:pos|org)\([^()\\{}]*\)$|^\\an[1-9]$`)
var timedTransform = regexp.MustCompile(`^\\t\(([0-9]{1,9}),([0-9]{1,9}),(?:[0-9]+(?:\.[0-9]+)?,)?(\\.*)\)$`)

func (c *cleaner) stateProposals(value string, initial state, final *styleTracker) []textProposal {
	pieces, ok := c.scanText(value)
	if !ok {
		return nil
	}
	var result []textProposal
	emit := func(s, kind string) {
		if len(s) < len(value) {
			result = append(result, textProposal{s, kind})
		}
	}
	var shortened map[[2]int]string
	rebuild := func(removed map[[2]int]bool) string {
		var out strings.Builder
		for i, p := range pieces {
			if !p.block {
				out.WriteString(p.content)
				continue
			}
			out.WriteByte('{')
			out.WriteString(p.prefix)
			for j, t := range p.tokens {
				if !removed[[2]int{i, j}] {
					if raw, ok := shortened[[2]int{i, j}]; ok {
						out.WriteString(raw)
					} else {
						out.WriteString(t.raw)
					}
				}
			}
			out.WriteByte('}')
		}
		return out.String()
	}
	// Track every color write through its complete lifetime, including later text.
	protected := map[string]bool{}
	valid := true
	for _, p := range pieces {
		for _, t := range p.tokens {
			if strings.HasPrefix(t.raw, `\t(`) {
				fields := c.transformWrites(t.raw)
				if fields == nil {
					valid = false
					break
				}
				for k := range fields {
					protected[k] = true
				}
			} else if t.writes == nil && !c.transparent(t.raw) {
				valid = false
				break
			}
		}
	}
	if valid {
		current := maps.Clone(initial)
		writers := map[string][2]int{}
		live, possible := map[[2]int]bool{}, map[[2]int]bool{}
		colors := map[string]string{"c": "1a", "2c": "2a", "3c": "3a", "4c": "4a"}
		axes := map[string][2]string{"3c": {"xbord", "ybord"}, "3a": {"xbord", "ybord"}, "4c": {"xshad", "yshad"}, "4a": {"xshad", "yshad"}}
		hidden := func(k string) bool {
			if protected[k] {
				return false
			}
			if alpha, ok := colors[k]; ok && !protected[alpha] && current[alpha] == float64(255) {
				return true
			}
			pair, ok := axes[k]
			return ok && (current["_border_style"] == float64(1) || current["_border_style"] == float64(3)) && !protected[pair[0]] && !protected[pair[1]] && current[pair[0]] == float64(0) && current[pair[1]] == float64(0)
		}
		for i, p := range pieces {
			if !p.block {
				if p.content != "" {
					for k, w := range writers {
						if !hidden(k) {
							live[w] = true
						}
					}
				}
				continue
			}
			for j, t := range p.tokens {
				for k := range t.writes {
					delete(writers, k)
				}
				if len(t.writes) == 1 {
					for k := range t.writes {
						_, a := colors[k]
						_, b := axes[k]
						if a || b {
							w := [2]int{i, j}
							writers[k] = w
							possible[w] = true
						}
					}
				}
				maps.Copy(current, t.writes)
			}
		}
		for w := range live {
			delete(possible, w)
		}
		if len(possible) > 0 {
			emit(rebuild(possible), "hidden_channel_lifetime")
		}
	}
	// Settled transform intervals are tracked per property; overlapping writes lose knowledge.
	type settledValue struct {
		end   float64
		value any
	}
	settled := map[string]settledValue{}
	for k, v := range initial {
		settled[k] = settledValue{math.Inf(-1), v}
	}
	removed := map[[2]int]bool{}
	shortened = map[[2]int]string{}
	for i, p := range pieces {
		if !p.block {
			if p.content != "" {
				clear(settled)
			}
			continue
		}
		for j, t := range p.tokens {
			if t.writes != nil {
				for k, v := range t.writes {
					settled[k] = settledValue{math.Inf(-1), v}
				}
				continue
			}
			if strings.HasPrefix(t.raw, `\t(`) {
				m := timedTransform.FindStringSubmatch(t.raw)
				writes := state{}
				count := 0
				var children []token
				valid := false
				var start, end float64
				if m != nil {
					start, _ = strconv.ParseFloat(m[1], 64)
					end, _ = strconv.ParseFloat(m[2], 64)
					prefix, parsed, ok := c.splitTokens(m[3])
					children = parsed
					valid = ok && prefix == "" && len(children) > 0 && end > start
					for _, child := range children {
						_, font := child.writes["fn"]
						if child.writes == nil || font {
							valid = false
						}
						maps.Copy(writes, child.writes)
						count += len(child.writes)
					}
				}
				if valid {
					unique := count == len(writes)
					redundant := unique
					for k, v := range writes {
						known, ok := settled[k]
						if !ok || known.end > start || known.value != v {
							redundant = false
						}
					}
					if redundant {
						removed[[2]int{i, j}] = true
						continue
					}
					if unique {
						var kept strings.Builder
						for _, child := range children {
							redundant := true
							for k, v := range child.writes {
								known, ok := settled[k]
								if !ok || known.end > start || known.value != v {
									redundant = false
								}
							}
							if !redundant {
								kept.WriteString(child.raw)
							}
						}
						if kept.Len() > 0 && kept.Len() < len(m[3]) {
							shortened[[2]int{i, j}] = t.raw[:len(t.raw)-len(m[3])-1] + kept.String() + ")"
						}
					}
					for k, v := range writes {
						previous, ok := settled[k]
						if unique && (!ok || previous.end <= start) {
							settled[k] = settledValue{end, v}
						} else {
							settled[k] = settledValue{math.Inf(1), nil}
						}
					}
				} else {
					fields := c.transformWrites(t.raw)
					if fields == nil {
						clear(settled)
					} else {
						for k := range fields {
							delete(settled, k)
						}
					}
				}
			} else if !c.transparent(t.raw) {
				clear(settled)
			}
		}
	}
	if len(removed) > 0 || len(shortened) > 0 {
		emit(rebuild(removed), "settled_transform_timeline")
	}
	shortened = nil
	// Single-block persistent invisibility and equal-scale font-size proposals.
	blocks := 0
	for _, p := range pieces {
		if p.block {
			blocks++
		}
	}
	if blocks == 1 && len(pieces) > 0 && pieces[0].block && pieces[0].prefix == "" {
		current := maps.Clone(initial)
		valid := true
		for _, t := range pieces[0].tokens {
			if t.writes != nil {
				maps.Copy(current, t.writes)
			} else if !positionedTag.MatchString(t.raw) {
				valid = false
				break
			}
		}
		if valid {
			hidden := map[string]bool{}
			for color, alpha := range map[string]string{"c": "1a", "2c": "2a", "3c": "3a", "4c": "4a"} {
				if current[alpha] == float64(255) {
					hidden[color] = true
				}
			}
			removed = map[[2]int]bool{}
			for j, t := range pieces[0].tokens {
				all := len(t.writes) > 0
				for k := range t.writes {
					if !hidden[k] {
						all = false
					}
				}
				if all {
					removed[[2]int{0, j}] = true
				}
			}
			if len(removed) > 0 {
				emit(rebuild(removed), "persistently_invisible_colors")
			}
			scale, sok := current["fscx"].(float64)
			size, fok := current["fs"].(float64)
			if initial["fscx"] == float64(100) && initial["fscy"] == float64(100) && initial["fsp"] == float64(0) && sok && fok && scale > 0 && scale != 100 && current["fscy"] == scale && current["fsp"] == float64(0) && size > 0 {
				a, _ := new(big.Rat).SetString(strconv.FormatFloat(size, 'f', -1, 64))
				b, _ := new(big.Rat).SetString(strconv.FormatFloat(scale, 'f', -1, 64))
				a.Mul(a, b)
				a.Quo(a, big.NewRat(100, 1))
				spelling := a.FloatString(6)
				rounded, _ := new(big.Rat).SetString(spelling)
				if a.Cmp(big.NewRat(10000, 1)) <= 0 && a.Cmp(rounded) == 0 {
					var out strings.Builder
					out.WriteByte('{')
					for _, t := range pieces[0].tokens {
						_, fs := t.writes["fs"]
						_, x := t.writes["fscx"]
						_, y := t.writes["fscy"]
						if !fs && !x && !y {
							out.WriteString(t.raw)
						}
					}
					out.WriteString(`\fs` + number(spelling) + "}")
					for _, p := range pieces[1:] {
						out.WriteString(p.content)
					}
					emit(out.String(), "uniform_scale_font_size")
				}
			}
		}
	}
	current, active := maps.Clone(initial), maps.Clone(initial)
	removed = map[[2]int]bool{}
	for i, p := range pieces {
		for j, t := range p.tokens {
			writes := t.writes
			transform := false
			if writes == nil && scaleReset.MatchString(t.raw) {
				writes = state{}
				for _, k := range []string{"fscx", "fscy"} {
					if v, ok := active[k]; ok {
						writes[k] = v
					}
				}
				if len(writes) != 2 {
					delete(current, "fscx")
					delete(current, "fscy")
					maps.Copy(current, writes)
					continue
				}
			}
			if writes == nil && strings.HasPrefix(t.raw, `\t(`) && strings.HasSuffix(t.raw, ")") {
				transform = true
				inner := t.raw[3 : len(t.raw)-1]
				pos := strings.IndexByte(inner, '\\')
				valid := pos >= 0
				if valid {
					prefix, children, ok := c.splitTokens(inner[pos:])
					valid = ok && prefix == "" && len(children) > 0 && precisionTransform.MatchString(inner[:pos])
					writes = state{}
					for _, child := range children {
						w := child.writes
						if w == nil && scaleReset.MatchString(child.raw) {
							if x, ok := active["fscx"]; ok {
								if y, ok := active["fscy"]; ok {
									w = state{"fscx": x, "fscy": y}
								}
							}
						}
						if w == nil {
							valid = false
						}
						maps.Copy(writes, w)
					}
				}
				if !valid {
					clear(current)
					clear(active)
					continue
				}
			}
			if writes != nil {
				redundant := true
				for k, v := range writes {
					if known, ok := current[k]; !ok || known != v {
						redundant = false
					}
				}
				if redundant {
					removed[[2]int{i, j}] = true
				}
				if transform {
					if !redundant {
						for k := range writes {
							delete(current, k)
						}
					}
				} else {
					maps.Copy(current, writes)
				}
				continue
			}
			if m := resetTag.FindStringSubmatch(t.raw); m != nil {
				name := strings.TrimRight(m[1], " \t")
				current = maps.Clone(initial)
				if name != "" {
					if !final.enabled || final.trackType != "ass" {
						current = state{}
					} else if s, ok := final.styles[name]; ok {
						current = maps.Clone(s)
					}
				}
				active = maps.Clone(current)
			} else if !c.transparent(t.raw) {
				clear(current)
				clear(active)
			}
		}
	}
	if len(removed) > 0 {
		emit(rebuild(removed), "state_redundancy")
	}
	return result
}
