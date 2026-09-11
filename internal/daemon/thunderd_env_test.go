package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestThunderdEnvFromEnviron(t *testing.T) {
	got, err := thunderdEnvFromEnviron([]string{"OTHER=ignored", "THUNDERD_ENV_TOKEN= a=b ", "THUNDERD_ENV_EMPTY=", "THUNDERD_ENV_lower_2=yes"})
	want := map[string]string{"TOKEN": " a=b ", "EMPTY": "", "lower_2": "yes"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	for _, entry := range []string{"THUNDERD_ENV_=secret", "THUNDERD_ENV_2BAD=secret", "THUNDERD_ENV_BAD-KEY=secret", "THUNDERD_ENV_KEY=secret\x00"} {
		if _, err := thunderdEnvFromEnviron([]string{entry}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("expected validation error without value for %q, got %v", entry, err)
		}
	}
}

func TestConfigFromEnvReadsThunderdSettings(t *testing.T) {
	t.Setenv(EnvNode, "node-a")
	t.Setenv(EnvMinNVDriverVersion, "535.104.05")
	t.Setenv(EnvThunderAPIToken, "token")
	t.Setenv("THUNDERD_ENV_TEST_VALUE", " exact value ")
	t.Setenv("THUNDERD_ENV_TEST_EMPTY", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := cfg.ThunderdEnv["TEST_EMPTY"]; !ok || value != "" || cfg.ThunderdEnv["TEST_VALUE"] != " exact value " {
		t.Fatalf("settings were not preserved: %v", cfg.ThunderdEnv)
	}
}

func TestMergeThunderdEnv(t *testing.T) {
	for _, test := range []struct {
		name, before, after string
		desired             map[string]string
	}{
		{"unchanged", "# don't rewrite\nA = 'same'\nB=untouched", "# don't rewrite\nA = 'same'\nB=untouched", map[string]string{"A": "same"}},
		{"override", "# comment\nA=old\nB=keep\n", "# comment\nA=\"new\"\nB=keep\n", map[string]string{"A": "new"}},
		{"append", "B=keep", "B=keep\nA=\"new\"\nZ=\"last\"\n", map[string]string{"A": "new", "Z": "last"}},
		{"empty", "A=old\n", "A=\nB=\n", map[string]string{"A": "", "B": ""}},
		{"duplicates", "A=old\nA=last\n", "A=old\nA=\"new\"\n", map[string]string{"A": "new"}},
		{"duplicate already effective", "A=old\nA=new\n", "A=old\nA=new\n", map[string]string{"A": "new"}},
		{"multiline unrelated", "B='one\nA=not an assignment\nthree'\nA=old\n", "B='one\nA=not an assignment\nthree'\nA=\"new\"\n", map[string]string{"A": "new"}},
		{"multiline replaced", "A=\"one\ntwo\"\nB=keep\n", "A=\"new\"\nB=keep\n", map[string]string{"A": "new"}},
		{"continuation", "A=one\\\ntwo\n", "A=one\\\ntwo\n", map[string]string{"A": "onetwo"}},
		{"unquoted spaces", "A=  one two \t\n", "A=  one two \t\n", map[string]string{"A": "one two"}},
		{"escaped trailing space", "A=one\\ \n", "A=one\\ \n", map[string]string{"A": "one "}},
		{"nothing supplied", "A=old\n", "A=old\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := mergeThunderdEnv(test.before, test.desired)
			if err != nil || got != test.after {
				t.Fatalf("got %q, %v; want %q", got, err, test.after)
			}
			again, err := mergeThunderdEnv(got, test.desired)
			if err != nil || again != got {
				t.Fatalf("second merge changed result: %q, %v", again, err)
			}
		})
	}
}

func TestEnvFileValueRoundTrip(t *testing.T) {
	for _, value := range []string{"", "simple", " a=b \t", "a\nb\r\nc", `quotes'" and \\ and $HOME and $(false) and ` + "`false`", "日本語"} {
		encoded := "KEY=" + quoteEnvFileValue(value) + "\n"
		decoded, end, err := parseEnvFileValue(encoded, 4)
		if err != nil || decoded != value || end != len(encoded) {
			t.Fatalf("round trip %q: got %q, %d, %v", value, decoded, end, err)
		}
	}
	if _, err := mergeThunderdEnv("OTHER='unterminated\n", map[string]string{"KEY": "value"}); err == nil {
		t.Fatal("should not overwrite an unterminated environment file")
	}
}

func TestThunderdEnvAtomicWrite(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "replace"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thunder", "thunderd.env")
			mode := os.FileMode(0600)
			if existing {
				mode = 0640
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("OLD=value\n"), mode); err != nil {
					t.Fatal(err)
				}
			}
			data := "KEY=" + quoteEnvFileValue("quotes'\"\\\n$(false) `false` $HOME") + "\n"
			if err := (osCommandRunner{}).RunShellInput(context.Background(), "test env write", thunderdEnvWriteCommand(path), data); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("read after write: %q, %v", got, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("file permissions not preserved: %v, %v", info, err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary files left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestConfigureThunderdEnvSkipsUnchanged(t *testing.T) {
	root := t.TempDir()
	path := resolveNodePath(root, thunderdEnvPath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("A='same'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	for _, desired := range []map[string]string{nil, {"A": "same"}} {
		if err := configureThunderdEnv(context.Background(), Config{HostRoot: root, ThunderdEnv: desired}, runner); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.recordedCommands()) != 0 {
		t.Fatal("unchanged configuration executed host commands")
	}
}

func TestRunConfiguresThunderdEnvOnceEvenOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &scriptedRunner{shellErr: errors.New("write failed")}
	cfg := Config{HostRoot: t.TempDir(), ThunderdEnv: map[string]string{"A": "new"}, ThunderdLogUnit: "off"}
	if err := run(ctx, cfg, runner, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup should proceed after write failure: %v", err)
	}
	if len(runner.attempted) != 1 {
		t.Fatalf("expected one startup write, got %d", len(runner.attempted))
	}
	// A healthy reconciliation must never reapply settings or restart services.
	runner.statuses = []scriptedStatus{{output: `{"healthy":true,"service":{"active":"active"},"localApi":{"healthy":true},"config":{"authTokenConfigured":true}}`}}
	r := &reconciler{cfg: cfg, runner: runner}
	for range 3 {
		if err := r.ensureEnrolled(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.attempted) != 1 || len(runner.combined) != 0 {
		t.Fatalf("reconciliation executed additional commands: %v, %v", runner.attempted, runner.combined)
	}
}
