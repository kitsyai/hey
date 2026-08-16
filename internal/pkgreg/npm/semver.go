package npm

import (
	"strconv"
	"strings"
)

// semver is a minimal semantic-version parse: just enough to compare two
// version strings for the dist-tag "latest" decision. No ranges, no
// coercion beyond what npm itself always produces.
type semver struct {
	major, minor, patch int
	prerelease          []string // nil = release version (always > any prerelease)
}

func parseSemver(v string) semver {
	v = strings.TrimPrefix(v, "v")
	core, pre, _ := strings.Cut(v, "-")
	core, _, _ = strings.Cut(core, "+") // drop build metadata
	if pre != "" {
		pre, _, _ = strings.Cut(pre, "+")
	}
	parts := strings.SplitN(core, ".", 3)
	get := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, _ := strconv.Atoi(parts[i])
		return n
	}
	s := semver{major: get(0), minor: get(1), patch: get(2)}
	if pre != "" {
		s.prerelease = strings.Split(pre, ".")
	}
	return s
}

// newer reports whether a is semver-greater than b.
func newer(a, b string) bool {
	return compareSemver(parseSemver(a), parseSemver(b)) > 0
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		return sign(a.major - b.major)
	}
	if a.minor != b.minor {
		return sign(a.minor - b.minor)
	}
	if a.patch != b.patch {
		return sign(a.patch - b.patch)
	}
	// Per semver: no prerelease > any prerelease; both present compares
	// identifier by identifier.
	if len(a.prerelease) == 0 && len(b.prerelease) == 0 {
		return 0
	}
	if len(a.prerelease) == 0 {
		return 1
	}
	if len(b.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		ai, aerr := strconv.Atoi(a.prerelease[i])
		bi, berr := strconv.Atoi(b.prerelease[i])
		if aerr == nil && berr == nil {
			if ai != bi {
				return sign(ai - bi)
			}
			continue
		}
		if a.prerelease[i] != b.prerelease[i] {
			if a.prerelease[i] < b.prerelease[i] {
				return -1
			}
			return 1
		}
	}
	return sign(len(a.prerelease) - len(b.prerelease))
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}
