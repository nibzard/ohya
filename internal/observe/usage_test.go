package observe

import (
	"reflect"
	"strings"
	"testing"
)

func usageFieldNames(t *testing.T) map[string]bool {
	t.Helper()
	fields := make(map[string]bool)
	typeOf := reflect.TypeOf(UsageRecord{})
	for i := 0; i < typeOf.NumField(); i++ {
		tag := typeOf.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			t.Fatalf("field %s lacks a json tag", typeOf.Field(i).Name)
		}
		fields[name] = true
	}
	return fields
}

func TestUsageFieldClassificationIsComplete(t *testing.T) {
	fields := usageFieldNames(t)
	if len(fields) != len(UsageFieldClasses) {
		t.Fatalf("%d usage fields but %d declared classes; the declaration must cover exactly the schema", len(fields), len(UsageFieldClasses))
	}
	for field := range fields {
		class, ok := UsageFieldClasses[field]
		if !ok {
			t.Errorf("usage field %q has no class declaration", field)
			continue
		}
		switch class {
		case ClassExportSafe, ClassLocalOnly:
		case ClassNeverCollected:
			t.Errorf("usage field %q is classified never-collected; the schema must hold no field for it", field)
		default:
			t.Errorf("usage field %q carries unknown class %q", field, class)
		}
	}
	for field := range UsageFieldClasses {
		if !fields[field] {
			t.Errorf("class declaration names %q, which is not a usage record field", field)
		}
	}
}

func TestUsageSchemaHoldsNoNeverCollectedField(t *testing.T) {
	fields := usageFieldNames(t)
	if len(NeverCollectedFields) == 0 {
		t.Fatal("the never-collected name list must name the banned categories")
	}
	banned := make(map[string]bool, len(NeverCollectedFields))
	for _, name := range NeverCollectedFields {
		if name != strings.ToLower(name) || strings.ContainsAny(name, " /") {
			t.Errorf("banned name %q must be a stable lowercase snake identifier", name)
		}
		if banned[name] {
			t.Errorf("duplicate banned name %q", name)
		}
		banned[name] = true
	}
	for field := range fields {
		if banned[field] {
			t.Errorf("usage record holds field %q, which is on the never-collected list", field)
		}
	}
}

func TestUsageExportBoundaryClasses(t *testing.T) {
	if ClassExportSafe != "export-safe" || ClassLocalOnly != "local-only" || ClassNeverCollected != "never-collected" {
		t.Fatal("class constants must keep their wire names")
	}
	localOnly := 0
	for _, class := range UsageFieldClasses {
		if class == ClassLocalOnly {
			localOnly++
		}
	}
	if localOnly == 0 {
		t.Error("at least one field must stay local-only; identifiers never leave the machine")
	}
}

func TestMapClient(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", UsageClientUnknown},
		{"claude-code", UsageClientClaudeCode},
		{"codex", UsageClientCodex},
		{"pi", UsageClientPi},
		{"Gemini CLI", UsageClientOther},
		{"claude-code-1.0", UsageClientOther},
	}
	for _, tc := range cases {
		if got := MapClient(tc.in); got != tc.want {
			t.Errorf("MapClient(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBucketVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2.1.280", "2.1"},
		{"0.166.0", "0.166"},
		{"1.5", "1.5"},
		{"v2.1.3", "2.1"},
		{"2", UsageClientUnknown},
		{"dev", UsageClientUnknown},
		{"", UsageClientUnknown},
		{"latest.label", UsageClientUnknown},
	}
	for _, tc := range cases {
		if got := BucketVersion(tc.in); got != tc.want {
			t.Errorf("BucketVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHourBucket(t *testing.T) {
	if got := HourBucket(1794840423123); got != "2026-11-16T14:00Z" {
		t.Errorf("HourBucket = %q, want 2026-11-16T14:00Z", got)
	}
}

func TestNormalizeReads(t *testing.T) {
	got := NormalizeReads([]string{"events.raw_json", "secret_table.col", "events.text", "events.raw_json"})
	want := []string{"events.raw_json", "events.text", "other"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeReads = %v, want %v", got, want)
	}
	if got := NormalizeReads(nil); len(got) != 0 {
		t.Errorf("NormalizeReads(nil) = %v, want empty", got)
	}
}

func TestNormalizeFunctions(t *testing.T) {
	got := NormalizeFunctions([]string{"bm25", "custom_function", "like", "LIKE", "snippet"})
	want := []string{"bm25", "like", "other", "snippet"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeFunctions = %v, want %v", got, want)
	}
}

func TestReadCatalogueCoversStorageDecisionColumns(t *testing.T) {
	for _, column := range []string{"events.raw_json", "events.text", "events_settled.raw_json"} {
		if !readSet[column] {
			t.Errorf("read catalogue must cover %s", column)
		}
	}
}

func TestUsageRecordValidate(t *testing.T) {
	valid := func() UsageRecord {
		return UsageRecord{
			SchemaVersion: UsageFormatVersion,
			TimeBucket:    "2026-11-16T14:00Z",
			TimeMS:        1794840423123,
			InstanceID:    "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d",
			Tool:          UsageToolSQL,
			Client:        UsageClientClaudeCode,
			ClientVersion: "2.1",
			Path:          UsagePathLocal,
			DeviceID:      "0123456789abcdef0123456789abcdef",
			SessionHint:   "28f633ba62e1aafb",
			Outcome:       UsageOutcomeOK,
			SQLiteCode:    0,
			PhaseMS:       UsagePhases{AdmissionWaitMS: 0.1, ReaderWaitMS: 0.2, FirstRowMS: 1, IterationMS: 2, TotalMS: 3.3},
			Rows:          10,
			ResultBytes:   2048,
			Reads:         []string{"events.id"},
			Functions:     []string{"length"},
			FTS:           false,
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*UsageRecord)
	}{
		{"wrong schema version", func(u *UsageRecord) { u.SchemaVersion = 2 }},
		{"unknown tool", func(u *UsageRecord) { u.Tool = "delete" }},
		{"unknown path", func(u *UsageRecord) { u.Path = "peer" }},
		{"unknown client", func(u *UsageRecord) { u.Client = "gemini" }},
		{"unknown outcome", func(u *UsageRecord) { u.Outcome = "crashed" }},
		{"missing timestamp", func(u *UsageRecord) { u.TimeMS = 0 }},
		{"bucket mismatch", func(u *UsageRecord) { u.TimeBucket = "2026-11-16T15:00Z" }},
		{"negative rows", func(u *UsageRecord) { u.Rows = -1 }},
		{"negative phase", func(u *UsageRecord) { u.PhaseMS.TotalMS = -1 }},
		{"sql with annotate ops", func(u *UsageRecord) { u.AnnotateOps = &AnnotateOps{Upsert: 1} }},
		{"uncatalogued read", func(u *UsageRecord) { u.Reads = []string{"events.secret_column"} }},
		{"uncatalogued function", func(u *UsageRecord) { u.Functions = []string{"load_extension"} }},
	}
	for _, tc := range cases {
		record := valid()
		tc.mutate(&record)
		if err := record.Validate(); err == nil {
			t.Errorf("%s must fail validation", tc.name)
		}
	}
	annotate := valid()
	annotate.Tool = UsageToolAnnotate
	annotate.AnnotateOps = &AnnotateOps{Upsert: 2, Delete: 1}
	if err := annotate.Validate(); err != nil {
		t.Fatalf("valid annotate record rejected: %v", err)
	}
	annotate.AnnotateOps.Delete = -3
	if err := annotate.Validate(); err == nil {
		t.Error("negative operation count must fail validation")
	}
}
