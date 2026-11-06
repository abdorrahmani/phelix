package table

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/olekukonko/tablewriter/pkg/twwidth"
)

type Table struct {
	writer   io.Writer
	centered bool
	header   []string
	rows     [][]string
}

// New creates either the plain layout or the centered layout with row separators.
func New(writer io.Writer, centered bool) *Table {
	return &Table{writer: writer, centered: centered}
}

// Header sets the column labels using the existing header normalization.
func (t *Table) Header(labels []string) {
	t.header = make([]string, len(labels))
	for i, label := range labels {
		t.header[i] = headerLabel(t.prepare(label))
	}
}

// Append copies one logical row for rendering.
func (t *Table) Append(row []string) {
	t.rows = append(t.rows, append([]string(nil), row...))
}

// Render preserves table output and the existing behavior of ignoring writer errors.
func (t *Table) Render() error {
	columns := len(t.header)
	for _, row := range t.rows {
		columns = max(columns, len(row))
	}
	if columns == 0 && len(t.rows) == 0 {
		return nil
	}
	widths := make([]int, columns)
	for i, cell := range t.header {
		widths[i] = twwidth.Width(cell) + 2
	}
	rows := make([][][]string, len(t.rows))
	for i, row := range t.rows {
		rows[i] = make([][]string, len(row))
		for j, cell := range row {
			rows[i][j] = strings.Split(t.prepare(cell), "\n")
			for _, line := range rows[i][j] {
				widths[j] = max(widths[j], twwidth.Width(line)+2)
			}
		}
	}
	var output strings.Builder
	border := func(left, middle, right string) {
		output.WriteString(left)
		lineWidth := max(twwidth.Width("─"), 1)
		for i, width := range widths {
			if i > 0 {
				output.WriteString(middle)
			}
			output.WriteString(strings.Repeat("─", width/lineWidth))
			output.WriteString(strings.Repeat(" ", width%lineWidth))
		}
		output.WriteString(right)
		output.WriteByte('\n')
	}
	line := func(cells []string, header bool) {
		output.WriteString("│")
		for i, width := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			padding := max(width-twwidth.Width(cell), 0)
			left := 1
			if header || t.centered && i > 0 {
				left = padding / 2
			}
			output.WriteString(strings.Repeat(" ", left))
			output.WriteString(cell)
			output.WriteString(strings.Repeat(" ", max(padding-left, 0)))
			output.WriteString("│")
		}
		output.WriteByte('\n')
	}
	border("┌", "┬", "┐")
	if len(t.header) > 0 {
		line(t.header, true)
		if len(rows) > 0 {
			border("├", "┼", "┤")
		}
	}
	cells := make([]string, columns)
	for i, row := range rows {
		if i > 0 && t.centered {
			border("├", "┼", "┤")
		}
		height := 0
		for _, cell := range row {
			height = max(height, len(cell))
		}
		for n := 0; n < height; n++ {
			clear(cells)
			for j, cell := range row {
				if n < len(cell) {
					cells[j] = cell[n]
				}
			}
			if n > 0 {
				blank := true
				for _, cell := range cells {
					if t.prepare(cell) != "" {
						blank = false
						break
					}
				}
				if blank {
					continue
				}
			}
			line(cells, false)
		}
	}
	border("└", "┴", "┘")
	_, _ = io.WriteString(t.writer, output.String())
	return nil
}

// prepare preserves each layout's whitespace rules and terminal tab expansion.
func (t *Table) prepare(cell string) string {
	if !t.centered {
		cell = strings.TrimSpace(cell)
	}
	if strings.ContainsRune(cell, '\t') {
		cell = strings.ReplaceAll(cell, "\t", strings.Repeat(" ", twwidth.TabWidth()))
	}
	return cell
}

// headerLabel preserves the existing camel-case splitting and punctuation spacing.
func headerLabel(label string) string {
	var words []string
	if !utf8.ValidString(label) {
		words = []string{label}
	} else {
		var groups [][]rune
		previous := 0
		for _, r := range label {
			class := 4
			switch {
			case unicode.IsLower(r):
				class = 1
			case unicode.IsUpper(r):
				class = 2
			case unicode.IsDigit(r):
				class = 3
			}
			if class == previous {
				groups[len(groups)-1] = append(groups[len(groups)-1], r)
			} else {
				groups = append(groups, []rune{r})
			}
			previous = class
		}
		for i := 0; i+1 < len(groups); i++ {
			if unicode.IsUpper(groups[i][0]) && unicode.IsLower(groups[i+1][0]) {
				last := len(groups[i]) - 1
				groups[i+1] = append([]rune{groups[i][last]}, groups[i+1]...)
				groups[i] = groups[i][:last]
			}
		}
		for _, group := range groups {
			word := string(group)
			if strings.TrimSpace(word) != "" && word != "_" {
				words = append(words, word)
			}
		}
	}
	label = strings.Join(words, " ")
	runes := []rune(label)
	for i, r := range runes {
		if r == '_' || r == '.' && (i > 0 && !numericOrSpace(runes[i-1]) || i+1 < len(runes) && !numericOrSpace(runes[i+1])) {
			runes[i] = ' '
		}
	}
	result := strings.TrimSpace(string(runes))
	if result == "" && label != "" {
		result = " "
	}
	return strings.ToUpper(result)
}

// numericOrSpace identifies the characters preserved around header decimal points.
func numericOrSpace(r rune) bool { return r >= '0' && r <= '9' || r == ' ' }
