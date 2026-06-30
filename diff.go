package main

import (
	"fmt"
	"strings"
)

// diffText returns a compact unified-style diff between old and new, with up to
// `ctx` lines of context around each change. Returns "" when they are equal.
// Line-based LCS; fine for the modestly sized text files this server serves.
func diffText(old, new string, ctx int) string {
	a := splitLines(old)
	b := splitLines(new)

	// LCS length table.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	// Walk to produce a sequence of ops: ' ' equal, '-' removed, '+' added.
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, op{' ', a[i]})
			i, j = i+1, j+1
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			ops = append(ops, op{'-', a[i]})
			i++
		} else {
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}

	// Mark which equal lines are within ctx of a change, so we can collapse
	// long unchanged runs.
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind == ' ' {
			continue
		}
		for d := -ctx; d <= ctx; d++ {
			if k+d >= 0 && k+d < len(ops) {
				keep[k+d] = true
			}
		}
	}

	var b2 strings.Builder
	gap := false
	for k, o := range ops {
		if o.kind == ' ' && !keep[k] {
			gap = true
			continue
		}
		if gap {
			b2.WriteString("  @@\n")
			gap = false
		}
		fmt.Fprintf(&b2, "%c %s\n", o.kind, o.text)
	}
	return b2.String()
}

// splitLines splits s into lines, dropping a single trailing newline so a file
// ending in "\n" does not yield a spurious empty final line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
