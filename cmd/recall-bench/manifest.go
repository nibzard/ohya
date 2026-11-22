package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	StartedAt     string             `json:"started_at"`
	HarnessCommit string             `json:"harness_commit,omitempty"`
	HarnessDirty  bool               `json:"harness_dirty,omitempty"`
	OhyaBinary    string             `json:"ohya_binary"`
	OhyaVersion   string             `json:"ohya_version,omitempty"`
	HubDB         string             `json:"hub_db"`
	HubSessions   int                `json:"hub_sessions"`
	CanarySeed    int64              `json:"canary_seed,omitempty"`
	Repeats       int                `json:"repeats"`
	Timeout       string             `json:"timeout"`
	Roots         []string           `json:"roots"`
	Questions     []manifestQuestion `json:"questions"`
}

type manifestQuestion struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func questionsSnapshotPath(outDir string) string {
	return filepath.Join(outDir, "questions-used.json")
}

func manifestPath(outDir string) string {
	return filepath.Join(outDir, "manifest.json")
}

func writeManifestFor(ctx context.Context, opts benchOptions, ohya string, res *resolver, questions []Question, paths []string) error {
	m := manifest{
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		OhyaBinary: ohya,
		HubDB:      opts.hubDB,
		Repeats:    opts.repeats,
		Timeout:    opts.timeout.String(),
		Roots:      opts.roots,
	}
	m.HarnessCommit, m.HarnessDirty = harnessIdentity()
	m.OhyaVersion = commandOutput(ohya, "version")
	if rows, err := res.query(ctx, "SELECT count(*) FROM sessions"); err == nil && len(rows) == 1 {
		if n, ok := rows[0][0].(float64); ok {
			m.HubSessions = int(n)
		}
	}
	m.CanarySeed = canarySeedOf(opts.out)
	for _, p := range paths {
		sum, err := fileSHA256(p)
		if err != nil {
			return err
		}
		m.Questions = append(m.Questions, manifestQuestion{Path: p, SHA256: sum})
	}
	if err := writeJSON(manifestPath(opts.out), m); err != nil {
		return err
	}
	return writeJSON(questionsSnapshotPath(opts.out), questions)
}

func harnessIdentity() (commit string, dirty bool) {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", false
	}
	commit = strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		dirty = strings.TrimSpace(string(st)) != ""
	}
	return commit, dirty
}

func commandOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(strings.TrimSpace(string(out))), " ")
}

func canarySeedOf(outDir string) int64 {
	data, err := os.ReadFile(filepath.Join(outDir, "canary-facts.json"))
	if err != nil {
		return 0
	}
	var facts struct {
		Seed int64 `json:"seed"`
	}
	json.Unmarshal(data, &facts)
	return facts.Seed
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readManifest(outDir string) (manifest, bool) {
	data, err := os.ReadFile(manifestPath(outDir))
	if err != nil {
		return manifest{}, false
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, false
	}
	return m, true
}

func loadQuestionsForRun(outDir string, extraPaths []string) ([]Question, error) {
	if _, err := os.Stat(questionsSnapshotPath(outDir)); err == nil {
		return loadQuestions([]string{questionsSnapshotPath(outDir)})
	}
	var paths []string
	paths = append(paths, extraPaths...)
	if _, err := os.Stat(canaryQuestionsPath(outDir)); err == nil {
		paths = append(paths, canaryQuestionsPath(outDir))
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s has no questions-used.json snapshot; pass -questions explicitly", outDir)
	}
	return loadQuestions(paths)
}

func loadRecords(outDir string) ([]runRecord, error) {
	traces, err := traceDirsOf(outDir)
	if err != nil {
		return nil, err
	}
	var records []runRecord
	for _, dir := range traces {
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			return nil, err
		}
		var rec runRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "meta.json"), err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func traceDirsOf(outDir string) ([]string, error) {
	runsDir := filepath.Join(outDir, "runs")
	armDirs, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", runsDir, err)
	}
	var dirs []string
	for _, armDir := range armDirs {
		if !armDir.IsDir() {
			continue
		}
		qDirs, err := os.ReadDir(filepath.Join(runsDir, armDir.Name()))
		if err != nil {
			return nil, err
		}
		for _, qDir := range qDirs {
			if !qDir.IsDir() {
				continue
			}
			repDirs, err := os.ReadDir(filepath.Join(runsDir, armDir.Name(), qDir.Name()))
			if err != nil {
				return nil, err
			}
			for _, repDir := range repDirs {
				dir := filepath.Join(runsDir, armDir.Name(), qDir.Name(), repDir.Name())
				if _, err := os.Stat(filepath.Join(dir, "stream.jsonl")); err == nil {
					dirs = append(dirs, dir)
				}
			}
		}
	}
	return dirs, nil
}
