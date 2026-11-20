// promtext.go: a hand-rolled parser for the Prometheus text exposition
// format — the handful of samples this adapter reads are not worth a
// client dependency (the router's metrics.go sets the same precedent).
// It parses `name{labels} value [timestamp]` lines, skipping HELP/TYPE
// comments and anything it cannot read as a float.
package livestats

import "strings"

// promSample is one parsed metric line.
type promSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Get reads one label from the sample's label set.
func (s promSample) Get(label string) string { return s.Labels[label] }

// parsePromText parses every readable sample line. Lines it cannot parse
// are skipped — backends ship partial dialects, and the reader asks for
// specific names anyway.
func parsePromText(body string) []promSample {
	var out []promSample
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest := cutSampleName(line)
		if name == "" {
			continue
		}
		labels := map[string]string{}
		if strings.HasPrefix(rest, "{") {
			end := strings.Index(rest, "}")
			if end < 0 {
				continue
			}
			labels = parseLabels(rest[1:end])
			rest = rest[end+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, ok := parsePromFloat(fields[0])
		if !ok {
			continue
		}
		out = append(out, promSample{Name: name, Labels: labels, Value: v})
	}
	return out
}

// cutSampleName splits the metric name (up to '{' or whitespace).
func cutSampleName(line string) (string, string) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '{' || c == ' ' || c == '\t' {
			return line[:i], strings.TrimLeft(line[i:], " \t")
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == ':') {
			return "", ""
		}
	}
	return line, ""
}

// parseLabels reads `name="value",…` (escapes: \" and \\).
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		name := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " ")
		if !strings.HasPrefix(s, `"`) {
			break
		}
		var val strings.Builder
		i := 1
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		out[name] = val.String()
		s = strings.TrimLeft(s[i:], " ")
		if strings.HasPrefix(s, ",") {
			s = strings.TrimLeft(s[1:], " ")
		} else {
			break
		}
	}
	return out
}

// parsePromFloat accepts what the exposition format allows: plain
// floats, +Inf/-Inf, NaN (NaN is skipped by the caller — an unknown is
// not a number).
func parsePromFloat(s string) (float64, bool) {
	switch s {
	case "+Inf", "Inf":
		return 0, false // infinite readings are not readings
	case "-Inf":
		return 0, false
	case "NaN":
		return 0, false
	}
	var f float64
	var neg bool
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			f = f*10 + float64(c-'0')
		} else if c == '.' {
			// fraction
			j := i + 1
			scale := 0.1
			for ; j < len(s); j++ {
				if s[j] < '0' || s[j] > '9' {
					break
				}
				f += float64(s[j]-'0') * scale
				scale /= 10
			}
			i = j - 1
		} else if c == 'e' || c == 'E' {
			return finishExponent(f, neg, s[i+1:])
		} else {
			return 0, false
		}
	}
	if neg {
		f = -f
	}
	return f, true
}

func finishExponent(base float64, neg bool, exp string) (float64, bool) {
	if exp == "" {
		return 0, false
	}
	expNeg := false
	if strings.HasPrefix(exp, "+") {
		exp = exp[1:]
	} else if strings.HasPrefix(exp, "-") {
		expNeg = true
		exp = exp[1:]
	}
	if exp == "" {
		return 0, false
	}
	e := 0
	for i := 0; i < len(exp); i++ {
		if exp[i] < '0' || exp[i] > '9' {
			return 0, false
		}
		e = e*10 + int(exp[i]-'0')
		if e > 400 {
			return 0, false
		}
	}
	for i := 0; i < e; i++ {
		if expNeg {
			base /= 10
		} else {
			base *= 10
		}
	}
	if neg {
		base = -base
	}
	return base, true
}
