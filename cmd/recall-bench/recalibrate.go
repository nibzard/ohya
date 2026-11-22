package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type hubCounter interface {
	countSessionsWithText(ctx context.Context, marker string, settled bool) (int, error)
}

var filesWithText = countFilesWithText

func recalibrate(ctx context.Context, opts benchOptions) error {
	if len(opts.questions) == 0 {
		return fmt.Errorf("recalibrate needs -questions files to update")
	}
	ohya, err := resolveExecutable(opts.ohya)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if opts.hubDB == "" {
		opts.hubDB = filepath.Join(home, ".ohya", "hub.db")
	}
	if opts.state == "" {
		opts.state = filepath.Join(opts.out, "resolver-state")
	}
	res, err := openResolver(ctx, ohya, opts.hubDB, opts.state)
	if err != nil {
		return err
	}
	defer res.close()
	return recalibrateWith(ctx, res, opts.questions, opts.roots)
}

func recalibrateWith(ctx context.Context, hub hubCounter, paths, roots []string) error {
	updated, checked := 0, 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var qs []Question
		if err := json.Unmarshal(data, &qs); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		changed := false
		for i := range qs {
			q := &qs[i]
			if q.EvidenceMarker == "" {
				if q.Negative {
					fmt.Printf("%-24s negative without marker; nothing to verify\n", q.Key)
				}
				continue
			}
			checked++
			total, err := hub.countSessionsWithText(ctx, q.EvidenceMarker, false)
			if err != nil {
				return fmt.Errorf("%s: %w", q.Key, err)
			}
			settled, err := hub.countSessionsWithText(ctx, q.EvidenceMarker, true)
			if err != nil {
				return fmt.Errorf("%s: %w", q.Key, err)
			}
			files, err := filesWithText(q.EvidenceMarker, roots)
			if err != nil {
				return fmt.Errorf("%s: %w", q.Key, err)
			}
			if q.Negative {
				if total != 0 || files != 0 {
					return fmt.Errorf("%s: negative term now matches %d hub sessions and %d local files; regenerate the question instead of recalibrating", q.Key, total, files)
				}
				fmt.Printf("%-24s negative verified: still zero matches\n", q.Key)
				continue
			}
			wantSettled := q.ExpectedSettled
			if wantSettled == 0 {
				wantSettled = q.ExpectedSessions
			}
			if total != q.ExpectedSessions || settled != wantSettled || files != q.ExpectedFiles {
				fmt.Printf("%-24s sessions %d -> %d, settled %d -> %d, files %d -> %d\n",
					q.Key, q.ExpectedSessions, total, wantSettled, settled, q.ExpectedFiles, files)
				if total < q.ExpectedSessions {
					fmt.Printf("  warning: hub matches decreased; sessions were deleted or the hub lost data\n")
				}
				q.ExpectedSessions, q.ExpectedSettled, q.ExpectedFiles = total, settled, files
				changed = true
				updated++
			}
			if total != 0 && settled < total {
				fmt.Printf("  warning: %d live (unsettled) matching sessions; consider rerunning when idle\n", total-settled)
			}
		}
		if changed {
			out, err := json.MarshalIndent(qs, "", "  ")
			if err != nil {
				return err
			}
			out = append(out, '\n')
			if err := os.WriteFile(path, out, 0o600); err != nil {
				return err
			}
			fmt.Printf("updated %s; review the diff and commit it\n", path)
		}
	}
	fmt.Printf("recalibrated: %d questions checked, %d updated\n", checked, updated)
	return nil
}
