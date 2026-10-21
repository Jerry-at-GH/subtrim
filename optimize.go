package subtrim

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"subtrim/native"
)

type Options struct {
	Level          string
	Config         native.Config
	Budget         time.Duration
	Exhaustive     bool
	timestampLimit int
	session        *verificationSession
	coverage       []native.Event
}

type Verification struct {
	native.Comparison
	Coverage string `json:"coverage"`
	Reason   string `json:"reason,omitempty"`
}

type Report struct {
	Level             string       `json:"level"`
	Status            string       `json:"status"`
	ElapsedSeconds    float64      `json:"elapsed_seconds"`
	FinalVerification Verification `json:"final_verification"`
}

// One session owns all native resources and caches for one optimization call.
type verificationSession struct {
	resolution              [2]int
	precision               precisionAnalysis
	sampler                 native.Sampler
	config                  native.Config
	before, after, metadata *native.Renderer
	source, metadataSource  []byte
	frames, preview         *native.FrameCache
	witnesses               []int64
}

func (v *verificationSession) close() {
	for _, r := range []*native.Renderer{v.before, v.after, v.metadata} {
		if r != nil {
			r.Close()
		}
	}
}

func (v *verificationSession) reference(data []byte) ([]native.Event, error) {
	if v.before != nil && bytes.Equal(data, v.source) {
		return v.before.Events, nil
	}
	var err error
	if v.metadata == nil {
		v.metadata, err = native.Open(data, v.config)
	} else if !bytes.Equal(data, v.metadataSource) {
		err = v.metadata.Reload(data)
	}
	if err != nil {
		return nil, err
	}
	v.metadataSource = data
	return v.metadata.Events, nil
}

type interval struct{ start, end int64 }

func mergeIntervals(ranges []interval) []interval {
	slices.SortFunc(ranges, func(a, b interval) int { return cmp.Compare(a.start, b.start) })
	merged := ranges[:0]
	for _, r := range ranges {
		if r.end <= r.start {
			continue
		}
		if len(merged) > 0 && r.start <= merged[len(merged)-1].end {
			merged[len(merged)-1].end = max(merged[len(merged)-1].end, r.end)
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

func verify(ctx context.Context, original, baseline, candidate []byte, options Options, maxError, maxMean float64) (Verification, error) {
	result := Verification{Coverage: "sampled"}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	v := options.session
	defer func() {
		if result.FirstExceededMS != nil && !slices.Contains(v.witnesses, *result.FirstExceededMS) {
			if len(v.witnesses) == 16 {
				v.witnesses = v.witnesses[1:]
			}
			v.witnesses = append(v.witnesses, *result.FirstExceededMS)
		}
	}()
	var err error
	fresh := v.before == nil || !bytes.Equal(original, v.source)
	if fresh {
		if v.before == nil {
			v.before, err = native.Open(original, options.Config)
		} else {
			err = v.before.Reload(original)
		}
		if err != nil {
			return result, err
		}
		v.source = original
		v.frames = native.NewFrameCache(v.before, original)
		v.preview = native.NewFrameCache(v.before, original)
	}
	if v.after == nil {
		v.after, err = native.Open(candidate, options.Config)
	} else {
		err = v.after.Reload(candidate)
	}
	if err != nil {
		return result, err
	}
	before, after := v.before, v.after
	events := slices.Concat(before.Events, after.Events, options.coverage)
	v.frames.Begin(!fresh)
	v.preview.Begin(!fresh)
	var times []int64
	if options.Exhaustive || baseline == nil {
		if options.Exhaustive {
			result.Coverage = "exhaustive_integer_ms"
		}
		times, err = v.sampler.Timeline(ctx, events, options.Exhaustive, options.timestampLimit)
		if errors.Is(err, native.ErrCoverage) {
			result.Reason = "coverage_limit"
			return result, nil
		}
		if err != nil {
			return result, err
		}
	} else {
		reference, err := v.reference(baseline)
		if err != nil {
			return result, err
		}
		// candidate generation preserves event records; native correspondence is
		// checked at this parser boundary before mapping changed-event windows.
		if len(reference) != len(after.Events) {
			result.Reason = "native_event_correspondence_changed"
			return result, nil
		}
		var ranges, collisionRanges []interval
		c := cleaner{tokens: map[string]token{}, transparentTags: map[string]bool{}, transforms: map[string]state{}}
		positioned := func(text string) bool {
			if !strings.Contains(text, `\pos(`) && !strings.Contains(text, `\move(`) {
				return false
			}
			pieces, ok := c.scanText(text)
			if !ok {
				return false
			}
			found := false
			for _, p := range pieces {
				for _, t := range p.tokens {
					if t.writes == nil && !c.transparent(t.raw) && c.transformWrites(t.raw) == nil {
						return false
					}
					if (strings.HasPrefix(t.raw, `\pos(`) || strings.HasPrefix(t.raw, `\move(`)) && c.transparent(t.raw) {
						found = true
					}
				}
			}
			return found
		}
		for i, a := range reference {
			b := after.Events[i]
			if a.Start != b.Start || a.Duration != b.Duration || a.Effect != b.Effect {
				result.Reason = "native_event_correspondence_changed"
				return result, nil
			}
			if a != b && a.Duration > 0 {
				r := interval{a.Start - 1, a.Start + a.Duration + 2}
				ranges = append(ranges, r)
				if !positioned(a.Text) || !positioned(b.Text) {
					collisionRanges = append(collisionRanges, r)
				}
			}
		}
		ranges = mergeIntervals(ranges)
		collisionRanges = mergeIntervals(collisionRanges)
		// Include the entire temporal overlap component: collision displacement
		// can survive past the end of the event that caused it.
		if len(collisionRanges) > 0 {
			var contextRanges []interval
			for _, e := range events {
				if e.Duration > 0 {
					contextRanges = append(contextRanges, interval{e.Start - 1, e.Start + e.Duration + 2})
				}
			}
			contextRanges = mergeIntervals(contextRanges)
			expanded := ranges
			i := 0
			for _, r := range contextRanges {
				for i < len(collisionRanges) && collisionRanges[i].end <= r.start {
					i++
				}
				if i < len(collisionRanges) && collisionRanges[i].start < r.end {
					expanded = append(expanded, r)
				}
			}
			ranges = mergeIntervals(expanded)
		}
		full, err := v.sampler.Timeline(ctx, events, options.Exhaustive, 0)
		if err != nil {
			return result, err
		}
		index := 0
		for _, t := range full {
			for index < len(ranges) && t >= ranges[index].end {
				index++
			}
			if index < len(ranges) && t >= ranges[index].start {
				times = append(times, t)
			}
		}
	}
	if options.Exhaustive && maxError == 0 && maxMean == 0 {
		result.Comparison, err = native.CompareExact(ctx, before, after, times, v.frames)
		if !result.MeetsTolerance {
			result.Reason = "image_records_differ"
		}
		return result, err
	}
	if len(times) == 0 {
		result.Reason = "no_visible_evidence"
		return result, nil
	}
	if len(times) > 256 {
		probes := []int64{times[(len(times)-1)/5], times[2*(len(times)-1)/5], times[3*(len(times)-1)/5], times[4*(len(times)-1)/5]}
		for _, t := range v.witnesses {
			if _, ok := slices.BinarySearch(times, t); ok {
				probes = append(probes, t)
			}
		}
		slices.Sort(probes)
		probes = slices.Compact(probes)
		preview, err := native.Compare(ctx, before, after, probes, maxError, maxMean, v.preview)
		if err != nil {
			return result, err
		}
		if !preview.MeetsTolerance {
			result.Comparison = preview
			result.Reason = "preflight_rejected"
			return result, nil
		}
		// preview is rejection-only; fresh tracks restore collision/render history.
		v.frames.Begin(true)
		if err = after.Reload(candidate); err != nil {
			return result, err
		}
	}
	result.Comparison, err = native.Compare(ctx, before, after, times, maxError, maxMean, v.frames)
	if result.ActivePixelObservations == 0 {
		result.MeetsTolerance = false
		result.Reason = "no_visible_evidence"
	}
	return result, err
}

// Optimize owns one deadline, report and native session across both phases.
func Optimize(ctx context.Context, data []byte, options Options) (output []byte, report Report, err error) {
	started := time.Now()
	options.session = &verificationSession{config: options.Config}
	defer options.session.close()
	report = Report{Level: options.Level, Status: "complete"}
	defer func() {
		report.ElapsedSeconds = time.Since(started).Seconds()
	}()
	if options.Level != "O1" && options.Level != "O2" {
		return nil, report, errors.New("optimization level must be O1 or O2")
	}
	if options.Budget < 0 || options.Config.Width <= 0 || options.Config.Height <= 0 {
		return nil, report, errors.New("invalid budget or rendering dimensions")
	}
	doc, ok := decode(data)
	if !ok {
		return nil, report, errors.New("unsupported encoding or record separators")
	}
	dimensions, err := scriptDimensions(doc.text)
	if err != nil {
		return nil, report, err
	}
	if dimensions["PlayResX"] == 0 || dimensions["PlayResY"] == 0 {
		return nil, report, errors.New("explicit PlayResX and PlayResY are required")
	}
	options.session.resolution = [2]int{dimensions["PlayResX"], dimensions["PlayResY"]}
	if options.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Budget)
		defer cancel()
	}
	output, doc, err = optimizeInline(ctx, data, doc, options, &report)
	if err != nil || options.Level == "O1" || report.Status == "unsupported" {
		return output, report, err
	}
	if ctx.Err() != nil {
		report.Status = "time_budget_exhausted"
		return output, report, nil
	}
	output, err = optimizeSearch(ctx, data, output, doc, options, &report)
	return output, report, err
}

func optimizeInline(ctx context.Context, data []byte, doc document, options Options, report *Report) (output []byte, current document, err error) {
	current = doc
	output = data
	report.FinalVerification = Verification{Comparison: native.Comparison{MeetsTolerance: true}, Coverage: "static_semantic_equivalence"}
	if !assHeader.MatchString(doc.text) || ssaHeader.MatchString(doc.text) {
		report.Status = "unsupported"
		return output, current, nil
	}
	base := doc
	base.text = cleanStatic(doc.text)
	current = base
	output = base.encode(base.text)
	spans := dialogueSpans(base.text)
	var previous []byte
	// Fine candidates already include path reduction. If they fail, isolate
	// coordinate-preserving reduction under zero error instead of losing it.
	for _, step := range []float64{0.01, 0.001, 0} {
		if err = ctx.Err(); err != nil {
			report.Status = "time_budget_exhausted"
			return output, current, nil
		}
		patches := quantizeEdits(base, spans, options.session.resolution, options.Config, step, options.Exhaustive, &options.session.precision, func() bool { return ctx.Err() == nil })
		if len(patches) == 0 {
			continue
		}
		text := materialize(base.text, patches)
		candidate := base.encode(text)
		if len(candidate) >= len(output) || bytes.Equal(candidate, previous) {
			continue
		}
		// A zero-error rejection says nothing about the approximate policy.
		// Deduplicate only the fine candidates, which share the same limits.
		if step > 0 {
			previous = candidate
		}
		maxError, maxMean := 2.0, 0.25
		if step == 0 {
			maxError, maxMean = 0, 0
		}
		checked, checkErr := verify(ctx, data, output, candidate, options, maxError, maxMean)
		if checkErr != nil {
			if errors.Is(checkErr, context.DeadlineExceeded) || errors.Is(checkErr, context.Canceled) {
				report.Status = "time_budget_exhausted"
				return output, current, nil
			}
			return output, current, checkErr
		}
		if checked.MeetsTolerance {
			output = candidate
			current.text = text
			report.FinalVerification = checked
			report.Status = "accepted_sample"
			if options.Exhaustive {
				report.Status = "accepted_exhaustive"
			}
			break
		}
		if checked.Reason == "coverage_limit" || checked.Reason == "no_visible_evidence" {
			report.Status = checked.Reason
			break
		}
	}
	return output, current, nil
}

// Structural and precision candidates share one bounded frontier and commit path.
func optimizeSearch(ctx context.Context, original, current []byte, doc document, options Options, report *Report) ([]byte, error) {
	var err error
	doc.text = staticStructure(doc)
	current = doc.encode(doc.text)
	report.Status = "complete"
	attempts := 0
	diminishing := false
	guard := &verificationSession{config: options.Config}
	defer guard.close()
	exactOptions := options
	exactOptions.session = guard
	originalEvents, eventErr := options.session.reference(original)
	if eventErr != nil {
		return current, eventErr
	}
	exactOptions.coverage = originalEvents
	analysis := map[string][]analyzedEvent{}
	names := [...]string{"state", "cleanup", "geometry", "generic", "events", "structure", "precision", "styles"}
	var families [8]struct {
		attempts, accepted, batch int
		seconds, work             float64
	}
	for i := range families {
		families[i].batch = 1
	}
	// A deterministic work bound supplements the cooperative deadline; failures in
	// one family never consume a separate global rejection allowance.
	searchStarted := time.Now()
	committedBytes := 0
	for attempts < 32 && ctx.Err() == nil {
		input := prepareSearch(doc)
		source := current
		sourceEvents, eventErr := options.session.reference(source)
		if eventErr != nil {
			return current, eventErr
		}
		input.events = sourceEvents
		_, coverageErr := options.session.sampler.Timeline(ctx, sourceEvents, true, 10000)
		exhaustive := coverageErr == nil
		if coverageErr != nil && !errors.Is(coverageErr, native.ErrCoverage) {
			break
		}
		type stream struct {
			next      func() (proposal, bool)
			stop      func()
			head      *proposal
			done      bool
			alternate bool
			pending   []proposal
			input     *searchInput
		}
		streams := make([]stream, len(names))
		tried := make([]bool, len(names))
		resetStream := func(i int, input *searchInput) {
			name := names[i]
			sequence := func(yield func(proposal) bool) {
				switch name {
				case "events":
					eventProposals(input.layout, yield)
				case "structure":
					structuralProposals(input.layout, yield)
				case "styles":
					p := shortStyleNames(input.layout)
					if !factorStyles(input.layout, func(factor proposal) bool {
						// Both proposals use the same family cost estimate. Expose
						// renaming before smaller factoring trials can hide it.
						if p.saving > factor.saving {
							if !yield(p) {
								return false
							}
							p = proposal{}
						}
						return yield(factor)
					}) {
						return
					}
					if p.saving > 0 {
						yield(p)
					}
				case "precision":
					res := options.session.resolution
					stopped := false
					discoveryStarted := time.Now()
					for _, step := range []float64{1, 0.25, 0.1, 0.025, 0.01, 0.001} {
						patches := quantizeEdits(input.doc, input.spans, res, options.Config, step, options.Exhaustive, &options.session.precision, func() bool {
							if ctx.Err() != nil {
								stopped = true
								return false
							}
							if time.Since(discoveryStarted) < 2*time.Millisecond {
								return true
							}
							if !yield(proposal{}) {
								stopped = true
								return false
							}
							discoveryStarted = time.Now()
							return true
						})
						if ctx.Err() != nil || stopped {
							return
						}
						if len(patches) > 0 {
							p := newProposal(input.doc, patches, "inline_precision")
							p.step = step
							if !yield(p) {
								return
							}
						}
					}
				default:
					inlineProposals(ctx, input, name, analysis, &families[i].batch, exhaustive, yield)
				}
			}
			streams[i] = stream{input: input}
			streams[i].next, streams[i].stop = iter.Pull(sequence)
		}
		for i := range names {
			resetStream(i, input)
		}
		latest := input
		liveDoc, liveSpans := input.doc, input.spans
		seen := map[string]bool{}
		changed, restart := false, false
		for attempts < 32 && ctx.Err() == nil && !restart {
			// Give every empty frontier slot a discovery quantum before comparing
			// proposals. The first family to yield must not win by default.
			for i := range streams {
				st := &streams[i]
				if st.head != nil || st.done && len(st.pending) == 0 {
					continue
				}
				var p proposal
				ok := true
				if len(st.pending) > 0 && (st.done || st.alternate) {
					st.alternate = false
					p = st.pending[len(st.pending)-1]
					st.pending = st.pending[:len(st.pending)-1]
				} else {
					p, ok = st.next()
					st.done = !ok
					if ok && p.saving > 0 {
						st.alternate = true
					}
				}
				if ok && p.saving > 0 {
					st.head = &p
				}
			}
			selected := -1
			best := -1.0
			selectedWork := 0.0
			exploring := false
			for i, st := range streams {
				if st.head != nil && !tried[i] {
					exploring = true
					break
				}
			}
			for i := range streams {
				if streams[i].head == nil || exploring && tried[i] {
					continue
				}
				p := streams[i].head
				input := streams[i].input
				work := 1.0
				if (!exhaustive || p.step > 0) && strings.HasPrefix(p.kind, "inline_") && len(sourceEvents) == len(input.spans) {
					for _, e := range p.patches {
						j := spanIndex(input.spans, e.start)
						if j < len(sourceEvents) {
							v := sourceEvents[j]
							work += eventWork(v)
						}
					}
				} else {
					for _, e := range sourceEvents {
						work += eventWork(e)
					}
				}
				stats := &families[i]
				cost := 0.002 + float64(len(current))/100000000 + work*0.00002
				units := cost
				if stats.attempts > 0 {
					cost *= math.Max(0.25, stats.seconds/families[i].work)
				}
				score := float64(p.saving) * float64(stats.accepted+1) / float64(stats.attempts+2) / cost
				if score > best {
					selected, best = i, score
					selectedWork = units
				}
			}
			if selected < 0 {
				unfinished := false
				for _, st := range streams {
					if st.head == nil && (!st.done || len(st.pending) > 0) {
						unfinished = true
						break
					}
				}
				if !unfinished {
					break
				}
				continue
			}
			untried := false
			for i, stream := range streams {
				if !tried[i] && (stream.head != nil || !stream.done || len(stream.pending) > 0) {
					untried = true
					break
				}
			}
			if !untried && committedBytes > 0 && best < float64(committedBytes)/time.Since(searchStarted).Seconds() {
				diminishing = true
				break
			}
			st := &streams[selected]
			p := *st.head
			input := st.input
			st.head = nil
			inline := strings.HasPrefix(p.kind, "inline_")
			// Style factoring appends fresh styles and changes Text on every event
			// it reassigns; existing styles and all other events remain unchanged.
			scoped := inline || p.kind == "style_factor"
			patches := append([]edit(nil), p.patches...)
			valid := true
			if inline {
				for j, e := range patches {
					k := spanIndex(input.spans, e.start)
					if k >= len(input.spans) || k >= len(liveSpans) || e.end > input.spans[k].end || input.doc.text[input.spans[k].start:input.spans[k].end] != liveDoc.text[liveSpans[k].start:liveSpans[k].end] {
						valid = false
						break
					}
					delta := liveSpans[k].start - input.spans[k].start
					patches[j].start += delta
					patches[j].end += delta
				}
			} else if input.doc.text != liveDoc.text {
				valid = false
			}
			if !valid {
				st.stop()
				if latest.doc.text != liveDoc.text {
					latest = prepareSearch(liveDoc)
					latest.events = sourceEvents
				}
				resetStream(selected, latest)
				continue
			}
			if inline {
				// Close each changed event under the proven cleanup rules before
				// rendering it; speculative deletions must not leave empty blocks.
				slices.SortFunc(patches, func(a, b edit) int { return a.start - b.start })
				var normalized []edit
				for j := 0; j < len(patches); {
					k := spanIndex(liveSpans, patches[j].start)
					span := liveSpans[k]
					cursor := span.start
					var text strings.Builder
					for j < len(patches) && patches[j].start < span.end {
						e := patches[j]
						text.WriteString(liveDoc.text[cursor:e.start])
						text.WriteString(e.text)
						cursor = e.end
						j++
					}
					text.WriteString(liveDoc.text[cursor:span.end])
					replacement, _ := input.cleaner.cleanText(text.String(), input.initials[k], input.styles.initial)
					normalized = append(normalized, edit{span.start, span.end, replacement, p.kind})
				}
				patches = normalized
			}
			var key strings.Builder
			for _, e := range patches {
				key.WriteString(strconv.Itoa(e.start) + ":" + strconv.Itoa(e.end) + ":" + strconv.Itoa(len(e.text)) + ":" + e.text)
			}
			if seen[key.String()] {
				continue
			}
			seen[key.String()] = true
			started := time.Now()
			text := materialize(liveDoc.text, patches)
			candidate := liveDoc.encode(text)
			if len(candidate) >= len(current) {
				continue
			}
			attempts++
			stats := &families[selected]
			stats.attempts++
			tried[selected] = true
			families[selected].work += selectedWork
			check := Verification{}
			var checkErr error
			// When the current result has zero error, checking an exact proposal
			// directly against the original is sufficient and keeps its frame tape
			// reusable. Include current landmarks and compare the complete track.
			direct := p.step == 0 && !scoped && !exhaustive && !options.Exhaustive && report.FinalVerification.MaxChannelError == 0 && report.FinalVerification.MaxActiveMeanError == 0
			if direct {
				originalOptions := options
				originalOptions.coverage = sourceEvents
				check, checkErr = verify(ctx, original, nil, candidate, originalOptions, 0, 0)
			} else if p.step == 0 {
				exact := exactOptions
				exact.Exhaustive = true
				exact.timestampLimit = 10000
				if exhaustive {
					check, checkErr = verify(ctx, current, nil, candidate, exact, 0, 0)
				} else {
					check.Reason = "coverage_limit"
				}
				if checkErr == nil && check.Reason == "coverage_limit" && !options.Exhaustive {
					var local []byte
					if scoped {
						local = current
					}
					check, checkErr = verify(ctx, current, local, candidate, exactOptions, 0, 0)
				}
			} else {
				check.MeetsTolerance = true
			}
			inherited := false
			if !direct && p.step == 0 && checkErr == nil && check.MeetsTolerance {
				previousTimes, previousErr := options.session.sampler.Timeline(ctx, slices.Concat(originalEvents, guard.before.Events), options.Exhaustive, 0)
				nextTimes, nextErr := options.session.sampler.Timeline(ctx, slices.Concat(originalEvents, guard.after.Events), options.Exhaustive, 0)
				inherited = previousErr == nil && nextErr == nil && slices.Equal(previousTimes, nextTimes)
			}
			if !direct && checkErr == nil && !inherited && (check.MeetsTolerance || check.Coverage == "exhaustive_integer_ms" && check.Reason == "no_visible_evidence") {
				var local []byte
				if scoped {
					local = current
				}
				check, checkErr = verify(ctx, original, local, candidate, options, 2, 0.25)
			}
			stats.seconds += time.Since(started).Seconds()
			if checkErr != nil {
				err = checkErr
				restart = true
				break
			}
			if check.MeetsTolerance || check.Coverage == "exhaustive_integer_ms" && check.Reason == "no_visible_evidence" {
				stats.accepted++
				committedBytes += len(current) - len(candidate)
				current = candidate
				if !inherited || report.FinalVerification.Coverage == "static_semantic_equivalence" {
					report.FinalVerification = check
				}
				report.FinalVerification.MeetsTolerance = true
				families[selected].batch = min(1024, families[selected].batch*2)
				changed = true
				clear(seen)
				liveDoc.text = text
				doc = liveDoc
				liveSpans = dialogueSpans(liveDoc.text)
				if !inline {
					clear(analysis)
					restart = true
				}
			} else {
				families[selected].batch = max(1, families[selected].batch/2)
				if inline && len(p.patches) > 1 {
					var active, rest []edit
					weights := make([]float64, len(p.patches))
					total := 0.0
					for i, e := range p.patches {
						weights[i] = 1
						k := spanIndex(input.spans, e.start)
						hit := false
						if k < len(input.events) {
							v := input.events[k]
							weights[i] = eventWork(v)
							hit = check.FirstExceededMS != nil && *check.FirstExceededMS >= v.Start && *check.FirstExceededMS < v.Start+v.Duration
						}
						total += weights[i]
						if hit {
							active = append(active, e)
						} else {
							rest = append(rest, e)
						}
					}
					if len(active) == 0 || len(rest) == 0 {
						mid, work := 1, weights[0]
						for mid < len(p.patches)-1 && work < total/2 {
							work += weights[mid]
							mid++
						}
						active, rest = p.patches[mid:], p.patches[:mid]
					}
					for _, group := range [][]edit{active, rest} {
						q := newProposal(input.doc, group, p.kind)
						q.step = p.step
						st.pending = append(st.pending, q)
					}
				}
			}
		}
		for _, st := range streams {
			st.stop()
		}
		if err != nil || !changed || diminishing {
			break
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return current, err
	}
	if ctx.Err() != nil {
		report.Status = "time_budget_exhausted"
	}
	return current, nil
}

// spanIndex returns the containing dialogue span, or its insertion position.
func spanIndex(spans []dialogueSpan, offset int) int {
	i, _ := slices.BinarySearchFunc(spans, offset, func(s dialogueSpan, x int) int {
		if s.end <= x {
			return -1
		}
		if s.start > x {
			return 1
		}
		return 0
	})
	return i
}
