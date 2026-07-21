package org

import (
	"fmt"
	"strings"
	"testing"
)

func TestRouteForProposal_SmallVsLarge(t *testing.T) {
	t.Parallel()
	small := "--- a/x\n+++ b/x\n+one line\n-another\n"
	if got := routeForProposal(small); got != routeSoloCritic {
		t.Errorf("small change routed to %v, want solo-critic", got)
	}

	// A large diff: 25 added content lines (headers excluded).
	var b strings.Builder
	b.WriteString("--- a/x\n+++ b/x\n")
	for i := 0; i < 25; i++ {
		b.WriteString("+added line\n")
	}
	if got := routeForProposal(b.String()); got != routeFullAudit {
		t.Errorf("large change routed to %v, want full-audit", got)
	}

	// An empty proposal is trivially small.
	if got := routeForProposal(""); got != routeSoloCritic {
		t.Errorf("empty proposal routed to %v, want solo-critic", got)
	}
}

func TestProposalChangedLines_IgnoresHeaders(t *testing.T) {
	t.Parallel()
	diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n+new\n-old\n context\n"
	// +++/--- are headers; @@ and " context" are not +/- content; only +new/-old count.
	if got := proposalChangedLines(diff); got != 2 {
		t.Errorf("changed lines = %d, want 2", got)
	}
}

// TestProposalChangedLines_CountsPlusMinusPayload guards the header-miscount fix:
// only the `--- a/…`/`+++ b/…` pair is a header. Real content lines whose text
// begins with +/- (rendered as `+++realchange` / `---realremoval` in the diff)
// must be COUNTED, not mistaken for headers and dropped.
func TestProposalChangedLines_CountsPlusMinusPayload(t *testing.T) {
	t.Parallel()
	diff := "--- a/x\n+++ b/x\n@@ @@\n+++realchange\n---realremoval\n context\n"
	if got := proposalChangedLines(diff); got != 2 {
		t.Errorf("payload lines beginning with ++/-- must be counted; changed lines = %d, want 2", got)
	}
}

// TestRouteForProposal_GiantOneLiner: a single added line that is huge in BYTES
// routes to the full audit even though its line count is tiny.
func TestRouteForProposal_GiantOneLiner(t *testing.T) {
	t.Parallel()
	oneLiner := "--- a/x\n+++ b/x\n@@ @@\n+" + strings.Repeat("a", largeChangeBytes+100) + "\n"
	if got := routeForProposal(oneLiner); got != routeFullAudit {
		t.Errorf("a giant one-liner should route full-audit on bytes; got %v", got)
	}
	if got := proposalChangedLines(oneLiner); got >= largeChangeLines {
		t.Fatalf("test precondition: the one-liner must be under the LINE threshold; lines=%d", got)
	}
}

// TestRouteForProposal_ManyFiles: a change spread across many files routes to the
// full audit on file count even when each file's change is tiny.
func TestRouteForProposal_ManyFiles(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 0; i < largeChangeFiles; i++ {
		fmt.Fprintf(&b, "--- a/f%d\n+++ b/f%d\n@@ @@\n+x\n", i, i)
	}
	if got := routeForProposal(b.String()); got != routeFullAudit {
		t.Errorf("a %d-file change should route full-audit on file count; got %v", largeChangeFiles, got)
	}
}

// TestRouteForProposal_BinaryDiff: a binary-file change (no +/- content lines)
// routes to the full audit.
func TestRouteForProposal_BinaryDiff(t *testing.T) {
	t.Parallel()
	bin := "diff --git a/img.png b/img.png\nBinary files a/img.png and b/img.png differ\n"
	if got := routeForProposal(bin); got != routeFullAudit {
		t.Errorf("a binary diff should route full-audit; got %v", got)
	}
}
