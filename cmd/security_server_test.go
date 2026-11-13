package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestServerSecurityCommands(t *testing.T) {
	for _, name := range []string{"scan", "status", "inventory", "history"} {
		cmd := newServerSecurityCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{name, "--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "--json") {
			t.Fatalf("%s lacks JSON help", name)
		}
	}
	cmd := newServerSecurityCommand()
	cmd.SetArgs([]string{"inventory", "--limit", "501"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted unbounded page")
	}
}
