package nodediag

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"time"
)

// lineMeta is the timestamp, level and logger of a go-log line, in either
// the JSON or the plaintext/colorized format.
type lineMeta struct {
	ts     time.Time
	level  int
	logger string
	hasTS  bool
	hasLvl bool
}

func (m lineMeta) time() (time.Time, bool) { return m.ts, m.hasTS }

const levelUnknown = -1

var levelRanks = map[string]int{
	"debug":  0,
	"info":   1,
	"warn":   2,
	"error":  3,
	"dpanic": 4,
	"panic":  5,
	"fatal":  6,
}

func parseLevel(s string) int {
	if r, ok := levelRanks[strings.ToLower(s)]; ok {
		return r
	}
	return levelUnknown
}

var tsLayouts = []string{
	"2006-01-02T15:04:05.000Z0700", // zapcore.ISO8601TimeEncoder, used by go-log
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05.000",
}

func parseTS(b []byte) (time.Time, bool) {
	s := string(b)
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 1e9 && f < 1e11 {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(frac*1e9)), true
	}
	return time.Time{}, false
}

// jsonHeadBytes bounds how far into a JSON line fields are looked up; zap
// writes level, ts and logger first.
const jsonHeadBytes = 512

func parseMeta(line []byte) lineMeta {
	if len(line) > 0 && line[0] == '{' {
		return parseJSONMeta(line[:min(len(line), jsonHeadBytes)])
	}
	return parseTextMeta(line)
}

func jsonField(head []byte, key string) ([]byte, bool) {
	k := []byte(`"` + key + `":`)
	i := bytes.Index(head, k)
	if i < 0 {
		return nil, false
	}
	v := head[i+len(k):]
	v = bytes.TrimLeft(v, " ")
	if len(v) == 0 {
		return nil, false
	}
	if v[0] == '"' {
		end := bytes.IndexByte(v[1:], '"')
		if end < 0 {
			return nil, false
		}
		return v[1 : 1+end], true
	}
	end := bytes.IndexAny(v, ",}")
	if end < 0 {
		return nil, false
	}
	return bytes.TrimSpace(v[:end]), true
}

func parseJSONMeta(head []byte) lineMeta {
	m := lineMeta{level: levelUnknown}
	if v, ok := jsonField(head, "ts"); ok {
		m.ts, m.hasTS = parseTS(v)
	}
	if v, ok := jsonField(head, "level"); ok {
		m.level = parseLevel(string(v))
		m.hasLvl = m.level != levelUnknown
	}
	if v, ok := jsonField(head, "logger"); ok {
		m.logger = string(v)
	}
	return m
}

// parseTextMeta parses "<ts>\t<LEVEL>\t<logger>\t..." lines, with optional
// ANSI colour codes.
func parseTextMeta(line []byte) lineMeta {
	m := lineMeta{level: levelUnknown}
	if len(line) == 0 || line[0] < '0' || line[0] > '9' {
		if len(line) == 0 || line[0] != 0x1b {
			return m
		}
	}
	head := stripANSI(line[:min(len(line), jsonHeadBytes)])
	fields := bytes.SplitN(head, []byte{'\t'}, 4)
	if len(fields) < 2 {
		return m
	}
	m.ts, m.hasTS = parseTS(bytes.TrimSpace(fields[0]))
	if !m.hasTS {
		return m
	}
	m.level = parseLevel(string(bytes.TrimSpace(fields[1])))
	m.hasLvl = m.level != levelUnknown
	if len(fields) >= 3 {
		m.logger = string(bytes.TrimSpace(fields[2]))
	}
	return m
}

func stripANSI(b []byte) []byte {
	if bytes.IndexByte(b, 0x1b) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// metaFilter filters lines by level, logger and time. Lines without their
// own metadata (multi-line messages, stack traces, raw child process output)
// inherit the metadata of the last line that had it.
type metaFilter struct {
	minLevel     int
	loggers      map[string]bool
	since, until *time.Time

	last     lineMeta
	haveLast bool
}

func newMetaFilter(minLevel string, loggers []string, since, until *time.Time) (*metaFilter, error) {
	if minLevel == "" && len(loggers) == 0 && since == nil && until == nil {
		return nil, nil
	}
	f := &metaFilter{minLevel: levelUnknown, since: since, until: until}
	if minLevel != "" {
		f.minLevel = parseLevel(minLevel)
		if f.minLevel == levelUnknown {
			return nil, badRequest("unknown level " + strconv.Quote(minLevel) + "; use debug, info, warn or error")
		}
	}
	if len(loggers) > 0 {
		f.loggers = map[string]bool{}
		for _, l := range loggers {
			f.loggers[l] = true
		}
	}
	if since != nil && until != nil && until.Before(*since) {
		return nil, badRequest("until is before since")
	}
	return f, nil
}

// check reports whether the line passes the filter, and whether the scan
// can stop because lines are now well past Until.
func (f *metaFilter) check(line []byte) (ok bool, stop bool) {
	m := parseMeta(line)
	if m.hasTS {
		f.last, f.haveLast = m, true
	} else if f.haveLast {
		m = f.last
	} else {
		return false, false
	}

	if f.until != nil && m.hasTS && m.ts.After(f.until.Add(untilSlack)) {
		return false, true
	}
	if f.since != nil && (!m.hasTS || m.ts.Before(*f.since)) {
		return false, false
	}
	if f.until != nil && (!m.hasTS || m.ts.After(*f.until)) {
		return false, false
	}
	if f.minLevel != levelUnknown && (!m.hasLvl || m.level < f.minLevel) {
		return false, false
	}
	if f.loggers != nil && !f.loggers[m.logger] {
		return false, false
	}
	return true, false
}
