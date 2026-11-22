package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func buildReport(records []runRecord, questions []Question, opts benchOptions) string {
	byKey := map[string]Question{}
	for _, q := range questions {
		byKey[q.Key] = q
	}
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	var b strings.Builder
	fmt.Fprintf(&b, "# Recall bench report\n\n")
	fmt.Fprintf(&b, "- date: %s\n", now)
	fmt.Fprintf(&b, "- runs: %d (%d questions x 2 arms x %d repeats)\n", len(records), len(questions), opts.repeats)
	fmt.Fprintf(&b, "- models seen: %s\n", strings.Join(uniqueModels(records), ", "))
	fmt.Fprintf(&b, "- timeout per run: %s\n", opts.timeout.String())

	perArm := map[string]armTotals{}
	for _, r := range records {
		t := perArm[r.Arm]
		t.runs++
		t.wall += r.WallSeconds
		t.input += r.InputTokens
		t.output += r.OutputTokens
		t.cacheRead += r.CacheReadTokens
		t.cacheNew += r.CacheNewTokens
		t.resultBytes += r.ToolResultBytes
		t.cost += r.CostUSD
		perArm[r.Arm] = t
	}

	fmt.Fprintf(&b, "\n## Totals per arm\n\n")
	fmt.Fprintf(&b, "| arm | runs | wall (s) | input tok | output tok | cache read tok | tool result KB | cost $ |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|\n")
	for _, arm := range []string{armRaw, armOhya} {
		t := perArm[arm]
		fmt.Fprintf(&b, "| %s | %d | %.0f | %d | %d | %d | %.0f | %.2f |\n",
			arm, t.runs, t.wall, t.input, t.output, t.cacheRead, float64(t.resultBytes)/1024, t.cost)
	}

	fmt.Fprintf(&b, "\n## Per question\n\n")
	fmt.Fprintf(&b, "| question | tier | arm | verdicts | wall med (s) | in/out tok med | calls med | result KB med |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|\n")
	var invalid []runRecord
	for _, q := range questions {
		for _, arm := range []string{armRaw, armOhya} {
			rs := filter(records, q.Key, arm)
			if len(rs) == 0 {
				continue
			}
			for _, r := range rs {
				if r.Verdict == "invalid" {
					invalid = append(invalid, r)
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %.1f | %s/%s | %.0f | %.1f |\n",
				q.Key, q.Tier, arm,
				verdictSummary(rs),
				medianFloat(mapTo(rs, func(r runRecord) float64 { return r.WallSeconds })),
				medianInt(mapTo(rs, func(r runRecord) int64 { return r.InputTokens })),
				medianInt(mapTo(rs, func(r runRecord) int64 { return r.OutputTokens })),
				medianFloat(mapTo(rs, func(r runRecord) float64 { return float64(r.ToolCalls) })),
				medianFloat(mapTo(rs, func(r runRecord) float64 { return float64(r.ToolResultBytes) / 1024 })),
			)
		}
	}

	fmt.Fprintf(&b, "\n## Tier summary (success fraction over all repeats)\n\n")
	tiers := uniqueTiers(questions)
	fmt.Fprintf(&b, "| tier | raw | ohya |\n|---|---|---|\n")
	for _, tier := range tiers {
		raw, ohya := tierFractions(records, questions, tier, armRaw), tierFractions(records, questions, tier, armOhya)
		fmt.Fprintf(&b, "| %s | %s | %s |\n", tier, raw, ohya)
	}

	if len(invalid) > 0 {
		fmt.Fprintf(&b, "\n## Invalid runs (excluded from interpretation)\n\n")
		for _, r := range invalid {
			kinds := sortedKeys(r.ToolKinds)
			fmt.Fprintf(&b, "- %s %s r%d: %v\n", r.Question, r.Arm, r.Repeat, kinds)
		}
	}

	var problems []string
	for _, r := range records {
		if len(r.Problems) > 0 {
			problems = append(problems, fmt.Sprintf("%s %s r%d: %s", r.Question, r.Arm, r.Repeat, strings.Join(r.Problems, "; ")))
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(&b, "\n## Run problems\n\n")
		for _, p := range problems {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	return b.String()
}

type armTotals struct {
	runs        int
	wall        float64
	input       int64
	output      int64
	cacheRead   int64
	cacheNew    int64
	resultBytes int64
	cost        float64
}

func filter(records []runRecord, key, arm string) []runRecord {
	var out []runRecord
	for _, r := range records {
		if r.Question == key && r.Arm == arm {
			out = append(out, r)
		}
	}
	return out
}

func verdictSummary(rs []runRecord) string {
	counts := map[string]int{}
	for _, r := range rs {
		counts[r.Verdict]++
	}
	if len(counts) == 1 {
		for v, n := range counts {
			return fmt.Sprintf("%d %s", n, v)
		}
	}
	var parts []string
	for _, v := range sortedVerdicts(counts) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[v], v))
	}
	return strings.Join(parts, ", ")
}

func sortedVerdicts(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for v := range counts {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func tierFractions(records []runRecord, questions []Question, tier, arm string) string {
	var rs []runRecord
	for _, r := range records {
		if r.Arm != arm {
			continue
		}
		if q, ok := questionByKey(questions, r.Question); ok && q.Tier == tier {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return "-"
	}
	success := 0
	for _, r := range rs {
		if r.Verdict == "correct" || r.Verdict == "negative-correct" {
			success++
		}
	}
	return fmt.Sprintf("%d/%d", success, len(rs))
}

func questionByKey(questions []Question, key string) (Question, bool) {
	for _, q := range questions {
		if q.Key == key {
			return q, true
		}
	}
	return Question{}, false
}

func uniqueTiers(questions []Question) []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range questions {
		if !seen[q.Tier] {
			seen[q.Tier] = true
			out = append(out, q.Tier)
		}
	}
	sort.Strings(out)
	return out
}

func uniqueModels(records []runRecord) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range records {
		if r.Model != "" && !seen[r.Model] {
			seen[r.Model] = true
			out = append(out, r.Model)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(none recorded)"}
	}
	return out
}

func mapTo[T any](rs []runRecord, f func(runRecord) T) []T {
	out := make([]T, 0, len(rs))
	for _, r := range rs {
		out = append(out, f(r))
	}
	return out
}

func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

func medianInt(xs []int64) string {
	if len(xs) == 0 {
		return "-"
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return fmt.Sprintf("%d", xs[len(xs)/2])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
