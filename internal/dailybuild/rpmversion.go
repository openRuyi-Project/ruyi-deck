package dailybuild

import "strings"

// rpmVersion carries the epoch, version and release used by RPM comparisons.
type rpmVersion struct {
	Epoch   uint64
	Version string
	Release string
}

// compareRPM orders epoch first, then version and release using RPM segment rules.
func compareRPM(a, b rpmVersion) int {
	if a.Epoch < b.Epoch {
		return -1
	}
	if a.Epoch > b.Epoch {
		return 1
	}
	if n := rpmvercmp(a.Version, b.Version); n != 0 {
		return n
	}
	return rpmvercmp(a.Release, b.Release)
}

// digit and alpha deliberately use RPM's ASCII classification.
func digit(c byte) bool { return c >= '0' && c <= '9' }

// alpha identifies an ASCII alphabetic version component.
func alpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// rpmvercmp handles numeric runs, separators, leading zeros, tilde and caret.
func rpmvercmp(a, b string) int {
	for a != "" || b != "" {
		for a != "" && !digit(a[0]) && !alpha(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for b != "" && !digit(b[0]) && !alpha(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		at, bt := strings.HasPrefix(a, "~"), strings.HasPrefix(b, "~")
		if at || bt {
			if !at {
				return 1
			}
			if !bt {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		ac, bc := strings.HasPrefix(a, "^"), strings.HasPrefix(b, "^")
		if ac || bc {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if !ac {
				return 1
			}
			if !bc {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if a == "" || b == "" {
			break
		}
		numeric := digit(a[0])
		i, j := 0, 0
		if numeric {
			for i < len(a) && digit(a[i]) {
				i++
			}
			for j < len(b) && digit(b[j]) {
				j++
			}
			if j == 0 {
				return 1
			}
		} else {
			for i < len(a) && alpha(a[i]) {
				i++
			}
			for j < len(b) && alpha(b[j]) {
				j++
			}
			if j == 0 {
				return -1
			}
		}
		left, right := a[:i], b[:j]
		if numeric {
			left, right = strings.TrimLeft(left, "0"), strings.TrimLeft(right, "0")
			if len(left) < len(right) {
				return -1
			}
			if len(left) > len(right) {
				return 1
			}
		}
		if n := strings.Compare(left, right); n != 0 {
			return n
		}
		a, b = a[i:], b[j:]
	}
	if a != "" {
		return 1
	}
	if b != "" {
		return -1
	}
	return 0
}
