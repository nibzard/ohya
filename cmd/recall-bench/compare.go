package main

import (
	"fmt"
	"sort"
	"strings"
)

func compareRuns(aDir, bDir string, extraQuestions []string) (string, error) {
	aQuestions, err := loadQuestionsForRun(aDir, extraQuestions)
	if err != nil {
		return "", fmt.Errorf("run A: %w", err)
	}
	bQuestions, err := loadQuestionsForRun(bDir, extraQuestions)
	if err != nil {
		return "", fmt.Errorf("run B: %w", err)
	}
	aRecords, err := loadRecords(aDir)
	if err != nil {
		return "", fmt.Errorf("run A: %w", err)
	}
	bRecords, err := loadRecords(bDir)
	if err != nil {
		return "", fmt.Errorf("run B: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Recall bench comparison\n\n")
	describeSide(&b, "A", aDir, aRecords)
	describeSide(&b, "B", bDir, bRecords)

	tierOf := map[string]string{}
	for _, q := range aQuestions {
		tierOf[q.Key] = q.Tier
	}
	for _, q := range bQuestions {
		if _, ok := tierOf[q.Key]; !ok {
			tierOf[q.Key] = q.Tier
		}
	}
	var keys []string
	for k := range tierOf {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(&b, "\n## Per tier and arm\n\n")
	fmt.Fprintf(&b, "| tier | arm | success A | success B | wall med A (s) | wall med B (s) | Δwall | in tok med A | in tok med B |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|\n")
	tiers := tierOrder(aQuestions, bQuestions)
	for _, tier := range tiers {
		for _, arm := range []string{armRaw, armOhya} {
			a := selectRecords(aRecords, tierOf, tier, arm)
			bRS := selectRecords(bRecords, tierOf, tier, arm)
			if len(a) == 0 && len(bRS) == 0 {
				continue
			}
			aWall := medianFloat(mapTo(a, func(r runRecord) float64 { return r.WallSeconds }))
			bWall := medianFloat(mapTo(bRS, func(r runRecord) float64 { return r.WallSeconds }))
			aIn := medianInt(mapTo(a, func(r runRecord) int64 { return r.InputTokens }))
			bIn := medianInt(mapTo(bRS, func(r runRecord) int64 { return r.InputTokens }))
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %.1f | %.1f | %s | %s | %s |\n",
				tier, arm, fraction(a), fraction(bRS), aWall, bWall, deltaPct(aWall, bWall), aIn, bIn)
		}
	}

	fmt.Fprintf(&b, "\n## Cost per correct answer\n\n")
	fmt.Fprintf(&b, "| arm | A | B |\n|---|---|---|\n")
	for _, arm := range []string{armRaw, armOhya} {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", arm,
			costPerCorrect(selectArm(aRecords, arm)), costPerCorrect(selectArm(bRecords, arm)))
	}

	onlyA, onlyB := keyDiff(aRecords, bRecords)
	if len(onlyA) > 0 || len(onlyB) > 0 {
		fmt.Fprintf(&b, "\n## Coverage differences\n\n")
		if len(onlyA) > 0 {
			fmt.Fprintf(&b, "- only in A: %s\n", strings.Join(onlyA, ", "))
		}
		if len(onlyB) > 0 {
			fmt.Fprintf(&b, "- only in B: %s\n", strings.Join(onlyB, ", "))
		}
	}
	return b.String(), nil
}

func describeSide(b *strings.Builder, label, dir string, records []runRecord) {
	m, ok := readManifest(dir)
	fmt.Fprintf(b, "- %s: %s", label, dir)
	if ok {
		fmt.Fprintf(b, " (started %s, ohya %s, harness %s", m.StartedAt, m.OhyaVersion, m.HarnessCommit)
		if m.HarnessDirty {
			fmt.Fprintf(b, ", dirty")
		}
		fmt.Fprintf(b, ", %d hub sessions)", m.HubSessions)
	} else {
		fmt.Fprintf(b, " (no manifest)")
	}
	models := uniqueModels(records)
	fmt.Fprintf(b, " — model %s\n", strings.Join(models, ", "))
}

func tierOrder(questionSets ...[]Question) []string {
	seen := map[string]bool{}
	var out []string
	for _, qs := range questionSets {
		for _, q := range qs {
			if !seen[q.Tier] {
				seen[q.Tier] = true
				out = append(out, q.Tier)
			}
		}
	}
	sort.Strings(out)
	return out
}

func selectRecords(records []runRecord, tierOf map[string]string, tier, arm string) []runRecord {
	var out []runRecord
	for _, r := range records {
		if r.Arm == arm && tierOf[r.Question] == tier {
			out = append(out, r)
		}
	}
	return out
}

func selectArm(records []runRecord, arm string) []runRecord {
	var out []runRecord
	for _, r := range records {
		if r.Arm == arm {
			out = append(out, r)
		}
	}
	return out
}

func fraction(rs []runRecord) string {
	if len(rs) == 0 {
		return "-"
	}
	n := 0
	for _, r := range rs {
		if r.Verdict == "correct" || r.Verdict == "negative-correct" {
			n++
		}
	}
	return fmt.Sprintf("%d/%d", n, len(rs))
}

func costPerCorrect(rs []runRecord) string {
	var cost float64
	n := 0
	for _, r := range rs {
		cost += r.CostUSD
		if r.Verdict == "correct" || r.Verdict == "negative-correct" {
			n++
		}
	}
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.3f", cost/float64(n))
}

func deltaPct(a, b float64) string {
	if a <= 0 || b <= 0 {
		return "-"
	}
	pct := (b - a) / a * 100
	return fmt.Sprintf("%+.0f%%", pct)
}

func keyDiff(aRecords, bRecords []runRecord) (onlyA, onlyB []string) {
	aKeys := recordKeys(aRecords)
	bKeys := recordKeys(bRecords)
	for k := range aKeys {
		if !bKeys[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range bKeys {
		if !aKeys[k] {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}

func recordKeys(records []runRecord) map[string]bool {
	keys := map[string]bool{}
	for _, r := range records {
		keys[r.Question] = true
	}
	return keys
}
