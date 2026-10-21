package subtrim

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type document struct {
	text  string
	bom   []byte
	order binary.ByteOrder
}

func decode(data []byte) (document, bool) {
	text := string(data)
	bom := []byte(nil)
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		order = binary.LittleEndian
		bom = data[:2]
	case bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		order = binary.BigEndian
		bom = data[:2]
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		bom = data[:3]
		text = string(data[3:])
	}
	if order != nil {
		if len(data)%2 != 0 {
			return document{}, false
		}
		units := make([]uint16, (len(data)-2)/2)
		for i := range units {
			units[i] = order.Uint16(data[2+i*2:])
		}
		for i := 0; i < len(units); i++ {
			if units[i] >= 0xd800 && units[i] <= 0xdbff {
				if i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return document{}, false
				}
				i++
			} else if units[i] >= 0xdc00 && units[i] <= 0xdfff {
				return document{}, false
			}
		}
		text = string(utf16.Decode(units))
	}
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 || strings.Contains(text, "\ufeff") {
		return document{}, false
	}
	for rest := text; ; {
		i := strings.IndexByte(rest, '\r')
		if i < 0 {
			break
		}
		if i+1 == len(rest) || rest[i+1] != '\n' {
			return document{}, false
		}
		rest = rest[i+2:]
	}
	return document{text, bom, order}, true
}

type dialogueSpan struct {
	start, end int
	effect     string
}

// eventFormat parses the complete ASS event schema at the text boundary.
func eventFormat(value string) map[string]int {
	columns := strings.Split(value, ",")
	if len(columns) != 10 || strings.ToLower(strings.TrimSpace(columns[9])) != "text" {
		return nil
	}
	fields := make(map[string]int, 10)
	for i, name := range columns {
		name = strings.ToLower(strings.Trim(name, " \t"))
		switch name {
		case "layer", "start", "end", "style", "name", "marginl", "marginr", "marginv", "effect", "text":
		default:
			return nil
		}
		if _, duplicate := fields[name]; duplicate {
			return nil
		}
		fields[name] = i
	}
	return fields
}

func dialogueSpans(text string) []dialogueSpan {
	var spans []dialogueSpan
	var fields map[string]int
	section, offset := "", 0
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimRight(line, "\r\n")
		stripped := strings.TrimLeft(body, "\ufeff \t")
		if strings.HasPrefix(stripped, "[") {
			section = stripped
			if strings.HasPrefix(strings.ToLower(stripped), "[events]") {
				section = "[Events]"
			}
			fields = nil
		}
		if section == "[Events]" {
			if strings.HasPrefix(stripped, "Format:") {
				fields = eventFormat(stripped[7:])
			} else if fields != nil && strings.HasPrefix(stripped, "Dialogue:") {
				start := strings.Index(body, "Dialogue:") + 9
				values := strings.SplitN(body[start:], ",", 10)
				if len(values) == 10 {
					spans = append(spans, dialogueSpan{offset + len(body) - len(values[9]), offset + len(body), strings.TrimSpace(values[fields["effect"]])})
				}
			}
		}
		offset += len(line)
	}
	return spans
}

// VideoSize selects a complete LayoutRes pair, then PlayRes, then 1920x1080.
func VideoSize(data []byte) [2]int {
	if doc, ok := decode(data); ok {
		if values, err := scriptDimensions(doc.text); err == nil {
			for _, prefix := range []string{"LayoutRes", "PlayRes"} {
				if x, y := values[prefix+"X"], values[prefix+"Y"]; x > 0 && y > 0 {
					return [2]int{x, y}
				}
			}
		}
	}
	return [2]int{1920, 1080}
}

func scriptDimensions(text string) (map[string]int, error) {
	values := map[string]int{}
	section := ""
	infoCount := 0
	explicit := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.Trim(strings.TrimRight(raw, "\r"), " \t")
		lower := line
		if strings.HasPrefix(line, "[") {
			lower = strings.ToLower(line)
		}
		matched := false
		for _, header := range []string{"[script info]", "[v4+ styles]", "[v4 styles]", "[events]", "[fonts]"} {
			if !strings.HasPrefix(lower, header) {
				continue
			}
			if lower != header {
				return nil, errors.New("noncanonical section header or suffix")
			}
			section = header
			explicit = header == "[script info]"
			if explicit {
				infoCount++
			}
			if header == "[v4 styles]" {
				return nil, errors.New("SSA declarations are not accepted")
			}
			matched = true
			break
		}
		if matched {
			continue
		}
		if strings.HasPrefix(line, "[") {
			explicit = false
		}
		name, value, colon := strings.Cut(line, ":")
		if !colon || section != "[script info]" {
			continue
		}
		canonical := ""
		for _, field := range []string{"PlayResX", "PlayResY", "LayoutResX", "LayoutResY"} {
			if strings.EqualFold(strings.Trim(name, " \t"), field) {
				canonical = field
			}
		}
		if canonical != "" {
			if !explicit || name != canonical {
				return nil, errors.New("noncanonical resolution declaration")
			}
			if _, ok := values[name]; ok {
				return nil, errors.New("duplicate resolution declaration")
			}
			value = strings.Trim(value, " \t")
			if !unsignedPattern.MatchString(value) {
				return nil, errors.New("resolution must be a positive integer")
			}
			n, err := strconv.ParseInt(strings.TrimLeft(value, "0"), 10, 32)
			if err != nil || n < 1 {
				return nil, errors.New("resolution outside positive signed 32-bit range")
			}
			values[name] = int(n)
		} else if strings.EqualFold(strings.Trim(name, " \t"), "ScriptType") && (name != "ScriptType" || strings.Trim(value, " \t") != "v4.00+") {
			return nil, errors.New("noncanonical or non-ASS ScriptType")
		}
	}
	if infoCount != 1 {
		return nil, errors.New("one Script Info section with explicit PlayResX and PlayResY is required")
	}
	if (values["LayoutResX"] == 0) != (values["LayoutResY"] == 0) {
		return nil, errors.New("LayoutResX and LayoutResY must be declared together")
	}
	return values, nil
}

func (d document) encode(text string) []byte {
	if d.order == nil {
		return append(bytes.Clone(d.bom), []byte(text)...)
	}
	units := utf16.Encode([]rune(text))
	result := make([]byte, 2+2*len(units))
	copy(result, d.bom)
	for i, v := range units {
		d.order.PutUint16(result[2+2*i:], v)
	}
	return result
}

// encodedLen excludes the document BOM, which is unchanged by patches.
func (d document) encodedLen(s string) int {
	if d.order == nil {
		return len(s)
	}
	n := 0
	for _, r := range s {
		n += 2
		if r > 0xffff {
			n += 2
		}
	}
	return n
}
