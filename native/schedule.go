package native

import (
	"cmp"
	"context"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var ErrCoverage = errors.New("timestamp limit cannot cover the requested timeline")

// Sampler memoizes timed-tag landmarks within one optimization, bounded to 2 MiB.
type Sampler struct {
	events map[Event][]int64
	bytes  int
}

// Timeline selects landmarks or every active integer millisecond.
// A coverage limit rejects the timeline; it never silently discards timestamps.
func (s *Sampler) Timeline(ctx context.Context, events []Event, exhaustive bool, limit int) ([]int64, error) {
	if !exhaustive {
		return s.sampledTimeline(events), nil
	}
	type interval struct{ start, end int64 }
	var ranges []interval
	for _, event := range events {
		if event.Duration > 0 {
			start, end := event.Start, event.Start+event.Duration
			start--
			end += 2
			ranges = append(ranges, interval{start, end})
		}
	}
	slices.SortFunc(ranges, func(a, b interval) int { return cmp.Compare(a.start, b.start) })
	merged := ranges[:0]
	for _, r := range ranges {
		if len(merged) > 0 && r.start <= merged[len(merged)-1].end {
			merged[len(merged)-1].end = max(merged[len(merged)-1].end, r.end)
		} else {
			merged = append(merged, r)
		}
	}
	count := int64(0)
	for _, r := range merged {
		count += r.end - r.start
		if limit > 0 && count > int64(limit) {
			return nil, ErrCoverage
		}
	}
	var times []int64
	for _, r := range merged {
		for t := r.start; t < r.end; t++ {
			if len(times)%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			times = append(times, t)
		}
	}
	if len(times) == 0 {
		times = []int64{0}
	}
	return times, nil
}

var animationTag = regexp.MustCompile(`\\(t|move|fad|fade)\(`)
var karaokeTag = regexp.MustCompile(`\\(kt|kf|ko|k|K)\s*(-?\d+(?:\.\d+)?)`)
var dynamicTag = regexp.MustCompile(`\\(?:t\(|move\(|fad\(|fade\(|k[fot]?[\s+\-\d]|K)`)

// sampledTimeline includes event boundaries, active-set midpoints, animation landmarks,
// and their neighbours. It is sampled evidence, not exhaustive equivalence.
func (s *Sampler) sampledTimeline(events []Event) []int64 {
	points, boundaries := map[int64]bool{0: true}, map[int64]bool{}
	seen := map[Event]bool{}
	grids := map[[2]int64]uint8{}
	for _, event := range events {
		if event.Duration <= 0 {
			continue
		}
		// The union needs each event's landmarks once, even when original,
		// baseline and candidate tracks contain the same event.
		if seen[event] {
			continue
		}
		seen[event] = true
		start, end := event.Start, event.Start+event.Duration
		boundaries[start] = true
		boundaries[end] = true
		var landmarks []int64
		recording := false
		near := func(value float64) {
			if math.IsNaN(value) || math.IsInf(value, 0) || value <= math.MinInt64+2 || value >= math.MaxInt64-2 {
				return
			}
			for t := int64(math.Floor(value)) - 1; t <= int64(math.Ceil(value))+1; t++ {
				if t >= start-1 && t <= end+1 {
					points[t] = true
					if recording {
						landmarks = append(landmarks, t)
					}
				}
			}
		}
		// Events sharing an interval contribute the same uniform grid.
		// Their tag-specific landmarks below still need separate analysis.
		key := [2]int64{start, event.Duration}
		grid := grids[key]
		if grid == 0 {
			for i := 0; i <= 4; i++ {
				near(float64(start) + float64(event.Duration)*float64(i)/4)
			}
			grids[key] = 1
		}
		if grid < 2 && (event.Effect != "" || dynamicTag.MatchString(event.Text)) {
			for i := 0; i <= 16; i++ {
				near(float64(start) + float64(event.Duration)*float64(i)/16)
			}
			grids[key] = 2
		}
		if cached, ok := s.events[event]; ok {
			for _, t := range cached {
				points[t] = true
			}
			continue
		}
		recording = s.bytes < 2<<20 && len(s.events) < 4096
		for _, match := range animationTag.FindAllStringSubmatchIndex(event.Text, -1) {
			depth, stop := 1, match[1]
			for ; stop < len(event.Text); stop++ {
				if event.Text[stop] == '(' {
					depth++
				}
				if event.Text[stop] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if depth != 0 {
				continue
			}
			tag, body := event.Text[match[2]:match[3]], event.Text[match[1]:stop]
			if tag == "t" {
				body, _, _ = strings.Cut(body, `\`)
				body = strings.TrimRight(body, " ,")
			}
			var values []float64
			valid := true
			if body != "" {
				for _, part := range strings.Split(body, ",") {
					v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
					if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
						valid = false
						break
					}
					values = append(values, v)
				}
			}
			if !valid {
				continue
			}
			var times []float64
			switch tag {
			case "t":
				if len(values) == 2 || len(values) == 3 {
					times = values[:2]
				} else {
					times = []float64{0, float64(event.Duration)}
				}
				if times[1] == 0 {
					times[1] = float64(event.Duration)
				}
			case "move":
				if len(values) == 6 {
					times = values[4:]
				} else {
					times = []float64{0, float64(event.Duration)}
				}
			case "fad":
				if len(values) == 2 {
					times = []float64{0, values[0], float64(event.Duration) - values[1], float64(event.Duration)}
				}
			case "fade":
				if len(values) == 7 {
					times = values[3:]
				}
			}
			for i, t := range times {
				near(float64(start) + t)
				if i > 0 {
					a := times[i-1]
					near(float64(start) + (a+t)/2)
					if tag == "t" && t > a {
						near(float64(start) + a + (t-a)*0.25)
						near(float64(start) + a + (t-a)*0.75)
					}
				}
			}
		}
		cursor := 0.0
		for _, match := range karaokeTag.FindAllStringSubmatch(event.Text, -1) {
			value, err := strconv.ParseFloat(match[2], 64)
			offset := value * 10
			if err != nil || math.IsInf(offset, 0) {
				continue
			}
			if match[1] == "kt" {
				cursor = offset
			} else {
				near(float64(start) + cursor + offset/2)
				cursor += offset
			}
			near(float64(start) + cursor)
		}
		if len(landmarks) > 0 {
			slices.Sort(landmarks)
			landmarks = slices.Compact(landmarks)
			size := len(event.Text) + len(event.Effect) + 8*cap(landmarks) + 64
			if size <= 2<<20-s.bytes {
				if s.events == nil {
					s.events = map[Event][]int64{}
				}
				s.events[event] = landmarks
				s.bytes += size
			}
		}
	}
	ordered := make([]int64, 0, len(boundaries))
	for time := range boundaries {
		ordered = append(ordered, time)
	}
	slices.Sort(ordered)
	for i := 1; i < len(ordered); i++ {
		points[int64(math.Floor(float64(ordered[i-1]+ordered[i])/2))] = true
	}
	result := make([]int64, 0, len(points))
	for time := range points {
		result = append(result, time)
	}
	slices.Sort(result)
	return result
}
