package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, env, file, inline, want string
		wantErr                       bool
	}{
		{name: "env wins", env: "from-env", file: file, inline: "inline", want: "from-env"},
		{name: "file beats inline", file: file, inline: "inline", want: "from-file"},
		{name: "inline last", inline: "inline", want: "inline"},
		{name: "missing file", file: filepath.Join(dir, "absent"), wantErr: true},
		{name: "empty file", file: empty, inline: "inline", wantErr: true},
		{name: "nothing", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("IMMICH_API_TOKEN", c.env)
			got, err := resolveToken(c.file, c.inline)
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("resolveToken = %q, %v; want %q, error=%v", got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := func() *appFlags {
		interval, breakdown, timeout := 2*time.Minute, 15*time.Minute, 20*time.Second
		topN, fanout, cities := 25, 200, 200
		return &appFlags{
			interval: &interval, breakdownInterval: &breakdown, requestTimeout: &timeout,
			topN: &topN, fanoutLimit: &fanout, cityLimit: &cities,
		}
	}
	if err := validate(valid()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	for name, breakIt := range map[string]func(*appFlags){
		"zero interval":     func(f *appFlags) { *f.interval = 0 },
		"tiny breakdown":    func(f *appFlags) { *f.breakdownInterval = time.Second },
		"no timeout":        func(f *appFlags) { *f.requestTimeout = 0 },
		"negative topn":     func(f *appFlags) { *f.topN = -1 },
		"negative city cap": func(f *appFlags) { *f.cityLimit = -5 },
		"negative fanout":   func(f *appFlags) { *f.fanoutLimit = -1 },
	} {
		f := valid()
		breakIt(f)
		if err := validate(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
