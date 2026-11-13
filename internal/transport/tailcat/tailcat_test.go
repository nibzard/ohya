package tailcat

import "testing"

func TestOptionsValidate(t *testing.T) {
	base := Options{
		Hostname:   "hub",
		ControlURL: "https://control.example",
		StateDir:   "/tmp/state",
		Port:       9027,
	}
	for _, tc := range []struct {
		name string
		mut  func(*Options)
		hub  bool
		ok   bool
	}{
		{"valid hub", func(o *Options) {}, true, true},
		{"valid device ignores port", func(o *Options) { o.Port = 0 }, false, true},
		{"missing hostname", func(o *Options) { o.Hostname = "" }, true, false},
		{"blank hostname", func(o *Options) { o.Hostname = "  " }, true, false},
		{"missing control url", func(o *Options) { o.ControlURL = "" }, true, false},
		{"relative control url", func(o *Options) { o.ControlURL = "control.example" }, true, false},
		{"missing state dir", func(o *Options) { o.StateDir = "" }, true, false},
		{"hub port zero", func(o *Options) { o.Port = 0 }, true, false},
		{"hub port negative", func(o *Options) { o.Port = -1 }, true, false},
		{"hub port too high", func(o *Options) { o.Port = 65536 }, true, false},
	} {
		opts := base
		tc.mut(&opts)
		err := opts.validate(tc.hub)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}
