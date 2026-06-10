package conformance

import (
	"bufio"
	"sort"
	"strings"
)

// ParseAllowlist parses the `conformance_allowlist.txt` body into the
// set of node ids it tolerates as not-yet-served. `#` starts a comment
// (whole-line or trailing); blank lines are ignored. The ratchet count
// (criterion 1) is len(of the returned set): CI fails if it grows.
func ParseAllowlist(body string) []string {
	out := []string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}
