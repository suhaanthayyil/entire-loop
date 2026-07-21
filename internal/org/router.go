package org

import "strings"

// route is the control/router decision (course step 8): a deterministic if/switch
// on a validated signal — here the SIZE of the build seat's proposal diff — that
// selects how hard the round verifies the change.
type route int

const (
	// routeSoloCritic: a small change gets one quick critic pass and no separate
	// verifier audit.
	routeSoloCritic route = iota
	// routeFullAudit: a large or risky change triggers the full parallel
	// verifier-on-edge audit before its proposal may be accepted into the result.
	routeFullAudit
)

func (r route) String() string {
	switch r {
	case routeFullAudit:
		return "full-audit"
	default:
		return "solo-critic"
	}
}

// Routing thresholds. A proposal that crosses ANY of these is "large/risky" and
// routed to the full audit — so a giant one-liner (few lines, many bytes), a
// sprawling multi-file change, or a binary diff all get the hard look, not just a
// change with many diff lines.
const (
	// largeChangeLines is the added/removed content-line count at or above which a
	// proposal routes to the full audit.
	largeChangeLines = 20
	// largeChangeBytes is the total added/removed content-byte count at or above
	// which a proposal routes to the full audit (catches a giant one-liner).
	largeChangeBytes = 1024
	// largeChangeFiles is the changed-file count at or above which a proposal
	// routes to the full audit (catches a sprawling change).
	largeChangeFiles = 3
)

// diffStat is the routing signal extracted from a unified diff.
type diffStat struct {
	lines  int  // added/removed content lines (file headers excluded)
	bytes  int  // added/removed content bytes (markers excluded)
	files  int  // changed files (header pairs + binary-file stanzas)
	binary bool // the diff contains a binary-file change
}

// routeForProposal inspects the build proposal's diff and routes: a small change
// → one quick critic pass; a large or risky change → the full parallel verifier
// audit. Routing is code, deterministic, on validated output.
func routeForProposal(proposal string) route {
	st := analyzeDiff(proposal)
	if st.binary ||
		st.lines >= largeChangeLines ||
		st.bytes >= largeChangeBytes ||
		st.files >= largeChangeFiles {
		return routeFullAudit
	}
	return routeSoloCritic
}

// analyzeDiff walks a unified diff and tallies changed content lines, content
// bytes, changed files, and whether any change is binary.
//
// File headers are matched precisely — a `--- a/…` (or `--- /dev/null`) line
// IMMEDIATELY followed by a `+++ b/…` (or `+++ /dev/null`) line is the one header
// pair per file and is skipped. Only that exact form is treated as a header, so
// payload lines like `++foo`, `--bar`, `+++x`, `---y` (a real added/removed line
// whose content begins with +/-) are counted as changes rather than mistaken for
// headers and dropped.
func analyzeDiff(proposal string) diffStat {
	var st diffStat
	lines := strings.Split(proposal, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// The single file-header pair for a file: `--- <hdr>` then `+++ <hdr>`.
		if isMinusFileHeader(line) && i+1 < len(lines) && isPlusFileHeader(lines[i+1]) {
			st.files++
			i++ // consume the matching +++ header too
			continue
		}
		// Binary stanza: `Binary files a/x and b/x differ` — no +/- content lines,
		// so count it as a changed file and flag the diff binary.
		if strings.HasPrefix(line, "Binary files ") {
			st.files++
			st.binary = true
			continue
		}
		// Hunk headers (`@@ … @@`) and git metadata (`diff --git`, `index …`) are
		// not content.
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			st.lines++
			st.bytes += len(line) - 1 // drop the +/- marker byte
		}
	}
	return st
}

// isMinusFileHeader reports whether line is the old-side file header of a diff.
func isMinusFileHeader(line string) bool {
	return strings.HasPrefix(line, "--- a/") || strings.HasPrefix(line, "--- /dev/null")
}

// isPlusFileHeader reports whether line is the new-side file header of a diff.
func isPlusFileHeader(line string) bool {
	return strings.HasPrefix(line, "+++ b/") || strings.HasPrefix(line, "+++ /dev/null")
}

// proposalChangedLines counts added/removed content lines in a unified diff,
// excluding the per-file header pair. Retained as a named helper for readability.
func proposalChangedLines(proposal string) int {
	return analyzeDiff(proposal).lines
}
