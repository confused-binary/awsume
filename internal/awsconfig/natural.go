package awsconfig

import "strings"

// NaturalLess reports whether a sorts before b in "human" order: runs of
// digits are compared by numeric value (so "prod-9" precedes "prod-10") and
// letters are compared case-insensitively. Ties fall back to byte order so
// the ordering is total and stable.
func NaturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		ca, cb := la[i], lb[j]
		if isDigit(ca) && isDigit(cb) {
			// Compare the whole digit runs numerically: strip leading zeros,
			// then a longer run is a larger number, then lexical.
			si, sj := i, j
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na := strings.TrimLeft(la[si:i], "0")
			nb := strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			// Same value: fewer leading zeros first.
			if (i - si) != (j - sj) {
				return (i - si) < (j - sj)
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	if len(la)-i != len(lb)-j {
		return len(la)-i < len(lb)-j
	}
	return a < b
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
