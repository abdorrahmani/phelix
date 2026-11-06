package table

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

type tableFixture struct {
	Name     string
	Header   []string
	Rows     [][]string
	Centered bool
	Output   string
}

func renderReference(w io.Writer, header []string, rows [][]string, centered bool) error {
	var options []tablewriter.Option
	if centered {
		options = []tablewriter.Option{
			tablewriter.WithRendition(tw.Rendition{Settings: tw.Settings{Separators: tw.Separators{BetweenRows: tw.On}}}),
			tablewriter.WithConfig(tablewriter.Config{Header: tw.CellConfig{Alignment: tw.CellAlignment{Global: tw.AlignCenter}}, Row: tw.CellConfig{Alignment: tw.CellAlignment{Global: tw.AlignCenter, PerColumn: []tw.Align{tw.AlignLeft}}}}),
		}
	}
	table := tablewriter.NewTable(w, options...)
	table.Header(header)
	for _, row := range rows {
		if err := table.Append(row); err != nil {
			return err
		}
	}
	return table.Render()
}

func TestTableReference(t *testing.T) {
	if os.Getenv("PHELIX_TABLE_CHILD") == "1" {
		testTableReference(t)
		return
	}
	for _, mode := range []string{"narrow", "wide", "modern"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTableReference$")
			cmd.Env = append(os.Environ(), "PHELIX_TABLE_CHILD=1", "PHELIX_TABLE_MODE="+mode, "TABWIDTH=4", "LC_ALL=C", "LC_CTYPE=C", "LANG=C", "TERM=vt100", "TERM_PROGRAM=", "VTE_VERSION=", "ALACRITTY_LOG=", "WT_PROFILE_ID=", "ConEmuANSI=", "TERM_PROGRAM_WSL=", "RUNEWIDTH_EASTASIAN=0")
			if mode == "wide" {
				cmd.Env = append(cmd.Env, "RUNEWIDTH_EASTASIAN=1")
			}
			if mode == "modern" {
				cmd.Env = append(cmd.Env, "RUNEWIDTH_EASTASIAN=1", "TERM_PROGRAM=kitty")
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}

func testTableReference(t *testing.T) {
	path := filepath.Join("testdata", os.Getenv("PHELIX_TABLE_MODE")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []tableFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for i := range fixtures {
		f := &fixtures[i]
		t.Run(f.Name, func(t *testing.T) {
			var output bytes.Buffer
			if err := renderReference(&output, f.Header, f.Rows, f.Centered); err != nil {
				t.Fatal(err)
			}
			if output.String() != f.Output {
				t.Fatalf("got:\n%q\nwant:\n%q", output.String(), f.Output)
			}
		})
	}
}

func BenchmarkTableRender(b *testing.B) {
	for _, count := range []int{10, 1000} {
		for _, centered := range []bool{false, true} {
			b.Run(fmt.Sprintf("rows=%d/centered=%t", count, centered), func(b *testing.B) {
				header := []string{"ID", "Name", "Version", "Status", "Details"}
				rows := make([][]string, count)
				for i := range rows {
					rows[i] = []string{fmt.Sprintf("app-%d", i), "\x1b[34m订单\x1b[0m", "v12", "running", "first line\nsecond line"}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if err := renderReference(io.Discard, header, rows, centered); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestTableWriterError(t *testing.T) {
	err := renderReference(failingWriter{}, []string{"Name"}, [][]string{{"example"}}, false)
	if err != nil {
		t.Fatalf("reference renderer ignores writer errors, got %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("table output failed") }
