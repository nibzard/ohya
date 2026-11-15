package fixgen

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
)

const (
	ObservedClaudeSessions  = 1042
	ObservedClaudeMainBytes = 1280 << 10
	ObservedClaudeSubagents = 1.25
	ObservedCodexSessions   = 1600
	ObservedCodexMainBytes  = 1200 << 10
	ObservedCodexSubagents  = 2
	ObservedPiSessions      = 74
	ObservedPiMainBytes     = 28 << 10

	compactTextDiv    = 8
	compactSessionDiv = 4

	ImportScale = 1.0 / 32
	QueryScale  = 0.7
)

type Mix struct {
	Seed    int64
	Scale   float64
	Compact bool
}

type AgentFiles struct {
	Agent string
	Files []File
}

func ImportMix(scale float64, seed int64) Mix {
	return Mix{Seed: seed, Scale: scale}
}

func QueryMix(seed int64) Mix {
	return Mix{Seed: seed, Scale: QueryScale, Compact: true}
}

func (m Mix) seed() int64 {
	if m.Seed == 0 {
		return 1
	}
	return m.Seed
}

type mixSpec struct {
	agent    string
	sessions int
	corpus   Corpus
}

func scaleSessions(scale float64, observed int) int {
	n := int(math.Round(scale * float64(observed)))
	if n < 1 {
		n = 1
	}
	return n
}

func (m Mix) specs() []mixSpec {
	div := int64(1)
	if m.Compact {
		div = compactSessionDiv
	}
	claude := Corpus{
		Sessions:            scaleSessions(m.Scale, ObservedClaudeSessions),
		MainMeanBytes:       ObservedClaudeMainBytes / div,
		SubagentsPerSession: ObservedClaudeSubagents,
		Seed:                m.seed(),
		Compact:             m.Compact,
	}
	rollouts := Corpus{
		Sessions:            scaleSessions(m.Scale, ObservedCodexSessions),
		MainMeanBytes:       ObservedCodexMainBytes / div,
		SubagentsPerSession: ObservedCodexSubagents,
		Seed:                m.seed(),
		Compact:             m.Compact,
	}
	sessions := Corpus{
		Sessions:      scaleSessions(m.Scale, ObservedPiSessions),
		MainMeanBytes: ObservedPiMainBytes / div,
		Seed:          m.seed(),
		Compact:       m.Compact,
	}
	return []mixSpec{
		{agent: claudecode.AgentName, sessions: claude.Sessions, corpus: claude},
		{agent: codex.AgentName, sessions: rollouts.Sessions, corpus: rollouts},
		{agent: pi.AgentName, sessions: sessions.Sessions, corpus: sessions},
	}
}

func GenerateMix(m Mix) []AgentFiles {
	specs := m.specs()
	out := make([]AgentFiles, len(specs))
	for i, s := range specs {
		var files []File
		switch s.agent {
		case claudecode.AgentName:
			files = Generate(s.corpus)
		case codex.AgentName:
			files = GenerateCodex(s.corpus)
		case pi.AgentName:
			files = GeneratePi(s.corpus)
		}
		out[i] = AgentFiles{Agent: s.agent, Files: files}
	}
	return out
}

func WriteMix(root string, m Mix) (int64, error) {
	return WriteAgentFiles(root, GenerateMix(m))
}

func WriteAgentFiles(root string, corpora []AgentFiles) (int64, error) {
	var total int64
	for _, af := range corpora {
		n, err := writeFiles(filepath.Join(root, af.Agent), af.Files)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

type MixStats struct {
	Agent      string
	Sessions   int
	Files      int
	Records    int64
	Events     int64
	Bytes      int64
	MeanRecord int64
	MaxRecord  int64
}

func MeasureMix(m Mix, corpora []AgentFiles) ([]MixStats, error) {
	specs := m.specs()
	if len(corpora) != len(specs) {
		return nil, fmt.Errorf("fixgen: measured %d agent corpora, want %d", len(corpora), len(specs))
	}
	out := make([]MixStats, len(corpora))
	for i, af := range corpora {
		st := MixStats{Agent: af.Agent, Sessions: specs[i].sessions}
		for _, f := range af.Files {
			st.Files++
			st.Bytes += int64(len(f.Data))
			off := 0
			for off < len(f.Data) {
				end := bytes.IndexByte(f.Data[off:], '\n')
				if end < 0 {
					return nil, fmt.Errorf("fixgen: %s: record without final newline", f.Path)
				}
				st.Records++
				if int64(end) > st.MaxRecord {
					st.MaxRecord = int64(end)
				}
				off += end + 1
			}
			records, cursor, err := parseRecordCount(af.Agent, f.Data)
			if err != nil {
				return nil, fmt.Errorf("fixgen: %s: parse: %w", f.Path, err)
			}
			if cursor != int64(len(f.Data)) {
				return nil, fmt.Errorf("fixgen: %s: cursor %d, want %d", f.Path, cursor, len(f.Data))
			}
			st.Events += records
		}
		if st.Records > 0 {
			st.MeanRecord = st.Bytes / st.Records
		}
		out[i] = st
	}
	return out, nil
}

func parseRecordCount(agent string, data []byte) (int64, int64, error) {
	switch agent {
	case claudecode.AgentName:
		res, err := claudecode.Parse(data, 0, claudecode.Options{SourceScope: "fixgen-measure"})
		return int64(len(res.Records)), res.Cursor, err
	case codex.AgentName:
		res, err := codex.Parse(data, 0, codex.Options{SourceScope: "fixgen-measure"})
		return int64(len(res.Records)), res.Cursor, err
	case pi.AgentName:
		res, err := pi.Parse(data, 0, pi.Options{SourceScope: "fixgen-measure"})
		return int64(len(res.Records)), res.Cursor, err
	default:
		return 0, 0, fmt.Errorf("fixgen: agent %q has no parser", agent)
	}
}
