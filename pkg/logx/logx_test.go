package logx

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"
)

func truncate(s string, n int) string {
    for i := range s {
        if n == 0 {
            return s[:i]
        }
        n--
    }
    return s
}
func setupTest(t *testing.T, opts Options) string {
    t.Helper()
    t.Cleanup(Close)
    path := filepath.Join(t.TempDir(), "doge.log")
    opts.Path = path
    if err := Setup(opts); err != nil {
        t.Fatal(err)
    }
    return path
}

func readLog(t *testing.T, path string) string {
    t.Helper()
    data, err := os.ReadFile(path)
    if err != nil {
        t.Fatal(err)
    }
    return string(data)
}

func TestUninitializedDiscards(t *testing.T) {
    // No Setup: library/test use must never write or panic.
    Debugf("nope %d", 1)
    Infof("nope")
    Errorf("nope")
}

func TestLevelFilter(t *testing.T) {
    path := setupTest(t, Options{Level: LevelWarn})
    Debugf("debug-line")
    Infof("info-line")
    Warnf("warn-line")
    Errorf("error-line")
    out := readLog(t, path)
    for _, unwanted := range []string{"debug-line", "info-line"} {
        if strings.Contains(out, unwanted) {
            t.Fatalf("filtered line %q present:\n%s", unwanted, out)
        }
    }
    for _, want := range []string{"warn-line", "error-line"} {
        if !strings.Contains(out, want) {
            t.Fatalf("missing line %q:\n%s", want, out)
        }
    }
}

func TestSetLevelRuntime(t *testing.T) {
    path := setupTest(t, Options{Level: LevelOff})
    SetLevel(LevelDebug)
    Debugf("now-visible")
    if out := readLog(t, path); !strings.Contains(out, "now-visible") {
        t.Fatalf("line missing after SetLevel:\n%s", out)
    }
}

func TestOffDisablesAll(t *testing.T) {
    path := setupTest(t, Options{Level: LevelOff})
    Errorf("nothing")
    if out := readLog(t, path); strings.Contains(out, "nothing") {
        t.Fatalf("off level still wrote:\n%s", out)
    }
}

func TestBanner(t *testing.T) {
    path := setupTest(t, Options{Level: LevelInfo, Banner: "doge 0.5.0 pid 42"})
    if out := readLog(t, path); !strings.Contains(out, "===== doge 0.5.0 pid 42 =====") {
        t.Fatalf("banner missing:\n%s", out)
    }
}

func TestSizeRotation(t *testing.T) {
    dir := t.TempDir()
    t.Cleanup(Close)
    path := filepath.Join(dir, "doge.log")
    if err := Setup(Options{Path: path, Level: LevelInfo, MaxBytes: 200}); err != nil {
        t.Fatal(err)
    }
    for i := 0; i < 40; i++ {
        Infof("filler line %02d padding-padding-padding", i)
    }
    Close()

    // The size rollover archives under today's date (with a numeric suffix
    // for a second rollover on the same day).
    day := time.Now().Format("2006-01-02")
    entries, err := os.ReadDir(dir)
    if err != nil {
        t.Fatal(err)
    }
    var archives []string
    for _, e := range entries {
        if strings.Contains(e.Name(), day) {
            archives = append(archives, e.Name())
        }
    }
    if len(archives) == 0 {
        t.Fatalf("no dated archives found: %v", entries)
    }

    // The active file must still receive lines after rotation.
    if err := Setup(Options{Path: path, Level: LevelInfo, MaxBytes: 200}); err != nil {
        t.Fatal(err)
    }
    Infof("post-rotation-line")
    Close()
    if out := readLog(t, path); !strings.Contains(out, "post-rotation-line") {
        t.Fatalf("active file lost writes after rotation:\n%s", out)
    }
}

func TestDailyRotation(t *testing.T) {
    // Setup anchors the rotation day to the file mtime (real clock), so the
    // mocked clock must agree with it on day 1: derive both days from the
    // real "today" instead of a fixed date (which rots two days later).
    now := time.Now()
    cur := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 30, 0, time.Local)
    orig := nowFn
    nowFn = func() time.Time { return cur }
    defer func() { nowFn = orig }()

    dir := t.TempDir()
    t.Cleanup(Close)
    path := filepath.Join(dir, "doge.log")
    if err := Setup(Options{Path: path, Level: LevelInfo}); err != nil {
        t.Fatal(err)
    }
    Infof("yesterday-line")
    Close()

    // Roll the clock past midnight: the next write must archive the previous
    // day's file as doge.log.<yesterday> and start a fresh one.
    cur = cur.Add(2 * time.Minute)
    if err := Setup(Options{Path: path, Level: LevelInfo}); err != nil {
        t.Fatal(err)
    }
    Infof("today-line")
    Close()

    if out := readLog(t, path); !strings.Contains(out, "today-line") || strings.Contains(out, "yesterday-line") {
        t.Fatalf("active file = %q, want only today's line", out)
    }
    archive := path + "." + now.Format(dateFmt)
    if out := readLog(t, archive); !strings.Contains(out, "yesterday-line") {
        t.Fatalf("archive %s = %q, want yesterday's line", archive, out)
    }
}

func TestParseLevel(t *testing.T) {
    cases := map[string]Level{
        "": LevelInfo, "info": LevelInfo, "INFO": LevelInfo,
        "debug": LevelDebug, "warn": LevelWarn, "error": LevelError, "off": LevelOff,
    }
    for in, want := range cases {
        got, err := ParseLevel(in)
        if err != nil || got != want {
            t.Fatalf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
        }
    }
    if _, err := ParseLevel("loud"); err == nil {
        t.Fatal("ParseLevel accepted an invalid level")
    }
}
